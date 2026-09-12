package main

// observe: live diagnostics via a local pass-through proxy.
//
// Forwards Anthropic/OpenAI traffic UNCHANGED (read-only — never optimizes) and
// diagnoses each request as it flows: cache anti-patterns, prefix drift vs the
// previous call, and a running hit rate. Point your SDK's base URL at it.

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
)

type observer struct {
	upstream string
	client   *http.Client
	mu       sync.Mutex
	n        int
	byModel  map[string]*agg
	findings map[string]int
	lastPfx  map[string]string // provider -> previous cacheable-prefix repr
}

func cmdObserve(args []string) int {
	fs := flag.NewFlagSet("observe", flag.ExitOnError)
	port := fs.Int("port", 7070, "local port to listen on")
	upstream := fs.String("upstream", "", "force one upstream base URL (else route by path)")
	fs.Parse(args)

	o := &observer{
		upstream: strings.TrimRight(*upstream, "/"),
		client:   &http.Client{},
		byModel:  map[string]*agg{},
		findings: map[string]int{},
		lastPfx:  map[string]string{},
	}
	srv := &http.Server{Addr: fmt.Sprintf(":%d", *port), Handler: o}

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-stop
		srv.Shutdown(context.Background())
	}()

	fmt.Printf("cachedoctor observing on http://localhost:%d  (read-only; Ctrl-C for summary)\n", *port)
	fmt.Printf("point your SDK at it:\n")
	fmt.Printf("  export ANTHROPIC_BASE_URL=http://localhost:%d\n", *port)
	fmt.Printf("  export OPENAI_BASE_URL=http://localhost:%d/v1\n\n", *port)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		fmt.Fprintln(os.Stderr, "cachedoctor:", err)
		return 1
	}
	o.summary()
	return 0
}

func (o *observer) route(path string) (provider, base string) {
	switch {
	case strings.Contains(path, "messages"):
		provider, base = "anthropic", "https://api.anthropic.com"
	case strings.Contains(path, "chat/completions"), strings.Contains(path, "responses"):
		provider, base = "openai", "https://api.openai.com"
	}
	if o.upstream != "" {
		base = o.upstream
	}
	return
}

func (o *observer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(io.LimitReader(r.Body, 32<<20))
	r.Body.Close()
	prov, base := o.route(r.URL.Path)
	if base == "" {
		http.Error(w, "cachedoctor: can't route this path (expected an Anthropic or OpenAI endpoint)", http.StatusBadGateway)
		return
	}

	target := base + r.URL.Path
	if r.URL.RawQuery != "" {
		target += "?" + r.URL.RawQuery
	}
	out, err := http.NewRequest(r.Method, target, bytes.NewReader(body))
	if err != nil {
		http.Error(w, "cachedoctor: "+err.Error(), http.StatusBadGateway)
		return
	}
	for k, vs := range r.Header {
		if strings.EqualFold(k, "Host") || strings.EqualFold(k, "Accept-Encoding") {
			continue
		}
		for _, v := range vs {
			out.Header.Add(k, v)
		}
	}
	out.Header.Set("Accept-Encoding", "identity") // keep the response parseable
	resp, err := o.client.Do(out)
	if err != nil {
		http.Error(w, "cachedoctor: upstream error: "+err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	for k, vs := range resp.Header {
		if hopByHop(k) {
			continue
		}
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	var buf bytes.Buffer
	flushCopy(w, io.TeeReader(resp.Body, &buf))

	o.observe(prov, r.URL.Path, resp.StatusCode, body, buf.Bytes())
}

func hopByHop(h string) bool {
	switch strings.ToLower(h) {
	case "connection", "keep-alive", "transfer-encoding", "te", "trailer",
		"upgrade", "proxy-authenticate", "proxy-authorization":
		return true
	}
	return false
}

func flushCopy(w http.ResponseWriter, r io.Reader) {
	fl, _ := w.(http.Flusher)
	b := make([]byte, 32*1024)
	for {
		n, err := r.Read(b)
		if n > 0 {
			w.Write(b[:n])
			if fl != nil {
				fl.Flush()
			}
		}
		if err != nil {
			return
		}
	}
}

func (o *observer) observe(prov, path string, status int, reqBody, respBody []byte) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.n++
	idx := o.n

	var findings []Finding
	var model, pfx string
	switch prov {
	case "anthropic":
		var req Request
		if json.Unmarshal(reqBody, &req) == nil {
			model = req.Model
			findings = filterSev(check(&req))
			pfx = prefixRepr(&req)
		}
	case "openai":
		var oa OARequest
		if json.Unmarshal(reqBody, &oa) == nil {
			model = oa.Model
			findings = filterSev(checkOpenAI(&oa))
			pfx = oa.prefixText()
		}
	}
	if pfx != "" {
		if last, ok := o.lastPfx[prov]; ok && last != pfx {
			findings = append([]Finding{{Sev: "HIGH",
				Title: "Cacheable prefix changed since the previous call"}}, findings...)
		}
		o.lastPfx[prov] = pfx
	}

	u := extractUsageBytes(respBody, model)
	hitStr := "—"
	if inR, readR, writeR, ok := rateFor(u.model); ok && u.in+u.cacheRead+u.cacheWrite > 0 {
		getAgg(o.byModel, u.model).add(u, inR, readR, writeR)
		if total := u.in + u.cacheRead + u.cacheWrite; total > 0 {
			hitStr = fmt.Sprintf("%.0f%%", 100*u.cacheRead/total)
		}
	}

	sev := "🟢"
	for _, f := range findings {
		o.findings[f.Title]++
		if f.Sev == "HIGH" {
			sev = "🔴"
		} else if sev == "🟢" {
			sev = "🟡"
		}
	}
	fmt.Printf("%s #%d %s %s → %d · hit %s", sev, idx, prov, path, status, hitStr)
	if len(findings) > 0 {
		titles := make([]string, len(findings))
		for i, f := range findings {
			titles[i] = f.Title
		}
		fmt.Printf("  · %s", strings.Join(titles, "; "))
	}
	fmt.Println()
}

// usageFieldRe precompiles the token-count field patterns extractUsageBytes
// scans for on every proxied response.
var usageFieldRe = func() map[string]*regexp.Regexp {
	m := map[string]*regexp.Regexp{}
	for _, name := range []string{
		"cache_read_input_tokens", "cached_tokens", "cache_creation_input_tokens",
		"input_tokens", "prompt_tokens", "output_tokens", "completion_tokens",
	} {
		m[name] = regexp.MustCompile(`"` + name + `"\s*:\s*(\d+)`)
	}
	return m
}()

// extractUsageBytes pulls token counts from a response body (JSON or SSE) by
// scanning for the usage fields — robust to Anthropic's message_start/_delta
// framing and OpenAI's nested cached_tokens.
func extractUsageBytes(b []byte, model string) urec {
	u := urec{model: model}
	maxField := func(name string) float64 {
		var mx float64
		for _, m := range usageFieldRe[name].FindAllSubmatch(b, -1) {
			if v, err := strconv.ParseFloat(string(m[1]), 64); err == nil && v > mx {
				mx = v
			}
		}
		return mx
	}
	u.cacheRead = maxField("cache_read_input_tokens")
	if u.cacheRead == 0 {
		u.cacheRead = maxField("cached_tokens")
	}
	u.cacheWrite = maxField("cache_creation_input_tokens")
	u.in = maxField("input_tokens")
	if u.in == 0 {
		if pt := maxField("prompt_tokens"); pt > 0 {
			if u.in = pt - u.cacheRead; u.in < 0 {
				u.in = 0
			}
		}
	}
	u.out = maxField("output_tokens")
	if u.out == 0 {
		u.out = maxField("completion_tokens")
	}
	return u
}

func (o *observer) summary() {
	o.mu.Lock()
	defer o.mu.Unlock()
	fmt.Printf("\n── session summary ──\n")
	if o.n == 0 {
		fmt.Println("no requests observed.")
		return
	}
	var tSpent, tRecover, tIn, tRead, tWrite float64
	for _, a := range o.byModel {
		tSpent += a.spent
		tRecover += a.recoverable
		tIn += a.in
		tRead += a.cacheRead
		tWrite += a.cacheWrite
	}
	fmt.Printf("%d requests · hit rate %.0f%% · spent ~$%.2f · recoverable ~$%.2f (this session)\n",
		o.n, hitPct(tIn, tRead, tWrite), tSpent, tRecover)
	if len(o.findings) > 0 {
		type fc struct {
			t string
			c int
		}
		var list []fc
		for t, c := range o.findings {
			list = append(list, fc{t, c})
		}
		sort.Slice(list, func(i, j int) bool { return list[i].c > list[j].c })
		fmt.Println("\nfindings seen:")
		for _, x := range list {
			fmt.Printf("  ×%d  %s\n", x.c, x.t)
		}
	}
}
