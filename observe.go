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
	"time"
)

type observer struct {
	upstream     string
	includeUsage bool // inject stream_options.include_usage into OpenAI streams
	client       *http.Client
	mu           sync.Mutex
	n            int
	byModel      map[string]*agg
	findings     map[string]int
	// seenPfx tracks recently seen cacheable-prefix representations per
	// provider|model. Drift is flagged when a *new* prefix appears where
	// others already exist — a prefix seen before never re-flags, so two
	// apps (or interleaved request kinds) sharing the proxy don't produce
	// a spurious HIGH on every request.
	seenPfx map[string]map[string]bool
}

const seenPfxCap = 16 // per provider|model; beyond this, drift detection resets

func cmdObserve(args []string) int {
	fs := flag.NewFlagSet("observe", flag.ExitOnError)
	port := fs.Int("port", 7070, "local port to listen on")
	upstream := fs.String("upstream", "", "force one upstream base URL (else route by path)")
	includeUsage := fs.Bool("include-usage", false,
		"set stream_options.include_usage on OpenAI streaming requests so usage is measurable (the one deliberate exception to read-only; clients see the extra usage chunk)")
	fs.Parse(args)

	o := &observer{
		upstream:     strings.TrimRight(*upstream, "/"),
		includeUsage: *includeUsage,
		client:       &http.Client{},
		byModel:      map[string]*agg{},
		findings:     map[string]int{},
		seenPfx:      map[string]map[string]bool{},
	}
	srv := &http.Server{Addr: fmt.Sprintf(":%d", *port), Handler: o}

	// On signal: stop accepting, wait (bounded) for in-flight requests so
	// their observations land, then let cmdObserve print the summary.
	done := make(chan struct{})
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-stop
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		srv.Shutdown(ctx)
		close(done)
	}()

	mode := "read-only"
	if o.includeUsage {
		mode = "read-only + include_usage injection for OpenAI streams"
	}
	fmt.Printf("cachedoctor observing on http://localhost:%d  (%s; Ctrl-C for summary)\n", *port, mode)
	fmt.Printf("point your SDK at it:\n")
	fmt.Printf("  export ANTHROPIC_BASE_URL=http://localhost:%d\n", *port)
	fmt.Printf("  export OPENAI_BASE_URL=http://localhost:%d/v1\n\n", *port)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		fmt.Fprintln(os.Stderr, "cachedoctor:", err)
		return 1
	}
	<-done // in-flight requests finish (bounded) before the summary
	o.summary()
	return 0
}

func (o *observer) route(path string) (provider, base string) {
	switch {
	// OpenAI paths first: /v1/threads/*/messages (Assistants) contains
	// "messages" but is not Anthropic traffic.
	case strings.Contains(path, "chat/completions"), strings.Contains(path, "responses"),
		strings.Contains(path, "/threads/"):
		provider, base = "openai", "https://api.openai.com"
	case strings.Contains(path, "messages"):
		provider, base = "anthropic", "https://api.anthropic.com"
	}
	if o.upstream != "" {
		base = o.upstream
	}
	return
}

func (o *observer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, maxBodyBytes+1))
	r.Body.Close()
	if err != nil {
		http.Error(w, "cachedoctor: reading request body: "+err.Error(), http.StatusBadRequest)
		return
	}
	if len(body) > maxBodyBytes {
		// Refuse rather than silently forward a truncated body the upstream
		// would reject with a baffling parse error.
		http.Error(w, fmt.Sprintf("cachedoctor: request body exceeds %dMB", maxBodyBytes>>20), http.StatusRequestEntityTooLarge)
		return
	}
	prov, base := o.route(r.URL.Path)
	if base == "" {
		http.Error(w, "cachedoctor: can't route this path (expected an Anthropic or OpenAI endpoint)", http.StatusBadGateway)
		return
	}

	// The one deliberate write: opt-in usage reporting for OpenAI streams.
	// Chat Completions only — the Responses API has no stream_options and
	// 400s on it (its streams always report usage anyway). Diagnosis below
	// still runs on the original body — we report what the app sends, not
	// what we forwarded.
	upstreamBody := body
	if o.includeUsage && prov == "openai" && strings.Contains(r.URL.Path, "chat/completions") {
		upstreamBody = injectIncludeUsage(body)
	}

	target := base + r.URL.Path
	if r.URL.RawQuery != "" {
		target += "?" + r.URL.RawQuery
	}
	// The client's context cancels the upstream call when the client hangs
	// up — otherwise an abandoned SSE stream is drained to EOF (a goroutine
	// and connection leak, and a Shutdown that never returns).
	out, err := http.NewRequestWithContext(r.Context(), r.Method, target, bytes.NewReader(upstreamBody))
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
	var buf capture
	flushCopy(w, io.TeeReader(resp.Body, &buf))

	o.observe(prov, r.URL.Path, resp.StatusCode, body, buf.Bytes(), buf.seamAt())
}

// captureLimit is how many bytes capture retains from each end of a response.
// The usage fields extractUsageBytes scans for live in small frames at the
// stream's edges (Anthropic: message_start / final message_delta; OpenAI: the
// last chunk), so the middle of a long stream is never needed.
const captureLimit = 64 << 10

// capture is an io.Writer that keeps the head and tail of a stream, bounding
// memory for arbitrarily long responses.
type capture struct {
	head, tail []byte
	truncated  bool
}

func (c *capture) Write(p []byte) (int, error) {
	n := len(p)
	if room := captureLimit - len(c.head); room > 0 {
		take := min(room, len(p))
		c.head = append(c.head, p[:take]...)
		p = p[take:]
	}
	if len(p) == 0 {
		return n, nil
	}
	c.truncated = true
	if c.tail == nil {
		c.tail = make([]byte, 0, captureLimit)
	}
	if len(p) >= captureLimit {
		c.tail = append(c.tail[:0], p[len(p)-captureLimit:]...)
	} else if len(c.tail)+len(p) <= captureLimit {
		c.tail = append(c.tail, p...)
	} else {
		keep := captureLimit - len(p)
		copy(c.tail, c.tail[len(c.tail)-keep:])
		c.tail = append(c.tail[:keep], p...)
	}
	return n, nil
}

// Bytes returns the retained head and tail, joined by a NUL so a regex can
// never match across the elided middle.
func (c *capture) Bytes() []byte {
	if !c.truncated {
		return c.head
	}
	out := make([]byte, 0, len(c.head)+1+len(c.tail))
	out = append(out, c.head...)
	out = append(out, 0)
	return append(out, c.tail...)
}

// seamAt returns the byte offset of the head/tail seam in Bytes(), or -1 if
// nothing was elided. A numeric match ending exactly at the seam may be a
// truncated number and must not be trusted.
func (c *capture) seamAt() int {
	if !c.truncated {
		return -1
	}
	return len(c.head)
}

func hopByHop(h string) bool {
	switch strings.ToLower(h) {
	case "connection", "keep-alive", "transfer-encoding", "te", "trailer",
		"upgrade", "proxy-authenticate", "proxy-authorization":
		return true
	}
	return false
}

// maxBodyBytes caps proxied request bodies (the providers' own limit is
// smaller); oversized requests get a clear 413 instead of silent truncation.
const maxBodyBytes = 32 << 20

func flushCopy(w http.ResponseWriter, r io.Reader) {
	fl, _ := w.(http.Flusher)
	b := make([]byte, 32*1024)
	for {
		n, err := r.Read(b)
		if n > 0 {
			if _, werr := w.Write(b[:n]); werr != nil {
				return // client gone; stop draining the upstream
			}
			if fl != nil {
				fl.Flush()
			}
		}
		if err != nil {
			return
		}
	}
}

func (o *observer) observe(prov, path string, status int, reqBody, respBody []byte, respSeam int) {
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
		if strings.Contains(path, "chat/completions") {
			var oa OARequest
			if json.Unmarshal(reqBody, &oa) == nil {
				model = oa.Model
				findings = filterSev(checkOpenAI(&oa))
				pfx = oa.prefixText()
			}
		} else {
			// Responses API: different shape (input, not messages) and its
			// streams always report usage — meter it, don't misdiagnose it.
			model = peekModel(reqBody)
		}
	}
	if pfx != "" {
		key := prov + "|" + model
		seen := o.seenPfx[key]
		if seen == nil {
			seen = map[string]bool{}
			o.seenPfx[key] = seen
		}
		if !seen[pfx] && len(seen) > 0 {
			findings = append([]Finding{{Sev: "HIGH",
				Title: "New cacheable prefix (drifted from previous calls?)"}}, findings...)
		}
		if len(seen) >= seenPfxCap {
			clear(seen)
		}
		seen[pfx] = true
	}

	u := extractUsageBytes(respBody, model, respSeam)
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
// framing and OpenAI's nested cached_tokens. seam is the elision boundary
// from capture.seamAt() (-1 if none): a number ending exactly there may be
// truncated and is discarded rather than trusted.
func extractUsageBytes(b []byte, model string, seam int) urec {
	u := urec{model: model}
	maxField := func(name string) float64 {
		var mx float64
		for _, loc := range usageFieldRe[name].FindAllSubmatchIndex(b, -1) {
			if loc[3] == seam {
				continue // possibly cut mid-number at the seam
			}
			if v, err := strconv.ParseFloat(string(b[loc[2]:loc[3]]), 64); err == nil && v > mx {
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
		fmt.Printf("\n%s\n", funnelLine())
	}
}
