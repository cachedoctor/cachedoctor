package main

// Integration tests: the layers unit tests can't see — the built binary's
// exit-code contract (including --exact against a mock count_tokens
// endpoint) and the observe proxy end-to-end over real HTTP.

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

var (
	binOnce sync.Once
	binPath string
	binErr  error
)

// testBinary builds the CLI once per test run.
func testBinary(t *testing.T) string {
	t.Helper()
	binOnce.Do(func() {
		dir, err := os.MkdirTemp("", "cachedoctor-bin")
		if err != nil {
			binErr = err
			return
		}
		binPath = filepath.Join(dir, "cachedoctor")
		out, err := exec.Command("go", "build", "-o", binPath, ".").CombinedOutput()
		if err != nil {
			binErr = fmt.Errorf("build: %v: %s", err, out)
		}
	})
	if binErr != nil {
		t.Fatal(binErr)
	}
	return binPath
}

func mustRead(t *testing.T, p string) string { return string(mustReq(t, p)) }

func runBin(t *testing.T, stdin string, env []string, args ...string) (int, string) {
	t.Helper()
	cmd := exec.Command(testBinary(t), args...)
	cmd.Env = append(os.Environ(), env...)
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	out, _ := cmd.CombinedOutput()
	return cmd.ProcessState.ExitCode(), string(out)
}

// TestCLIExitContract locks in the documented exit codes: 0 clean, 1 error,
// 2 high-severity finding, 64 usage error — a flag typo must never read as a
// finding, and help is not an error.
func TestCLIExitContract(t *testing.T) {
	cases := []struct {
		name string
		args []string
		in   string
		want int
	}{
		{"check clean", []string{"check", "examples/good.json"}, "", 0},
		{"check HIGH", []string{"check", "examples/no-cache.json"}, "", 2},
		{"check stdin", []string{"check", "-"}, mustRead(t, "examples/good.json"), 0},
		{"check missing file", []string{"check", "no-such-file.json"}, "", 1},
		{"check null body", []string{"check", "-"}, "null", 1},
		{"check unsupported provider", []string{"check", "-"}, `{"model":"llama-3-70b"}`, 1},
		{"check bad flag", []string{"check", "--bogus", "x.json"}, "", 64},
		{"check help", []string{"check", "-h"}, "", 0},
		{"diff HIGH", []string{"diff", "examples/callA.json", "examples/callB.json"}, "", 2},
		{"diff clean", []string{"diff", "examples/good.json", "examples/good.json"}, "", 0},
		{"diff double stdin", []string{"diff", "-", "-"}, "{}", 64},
		{"analyze clean", []string{"analyze", "examples/logs.jsonl"}, "", 0},
		{"analyze empty", []string{"analyze", "-"}, "", 1},
		{"top-level help", []string{"--help"}, "", 0},
		{"no args", nil, "", 64},
		{"unknown command", []string{"frobnicate"}, "", 64},
		{"observe bad flag", []string{"observe", "--bogus"}, "", 64},
	}
	for _, c := range cases {
		if rc, out := runBin(t, c.in, nil, c.args...); rc != c.want {
			t.Errorf("%s: exit %d, want %d\n%s", c.name, rc, c.want, clip(out, 200))
		}
	}
}

// TestCLIExactAgainstMock exercises the full --exact flow: flag parsing, key
// handling, and both count_tokens calls, against a local mock endpoint.
func TestCLIExactAgainstMock(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		n := 300
		var doc map[string]any
		json.Unmarshal(body, &doc)
		if doc["system"] != nil || doc["tools"] != nil {
			n = 1500
		}
		fmt.Fprintf(w, `{"input_tokens":%d}`, n)
	}))
	defer srv.Close()

	env := []string{"ANTHROPIC_API_KEY=sk-test", "ANTHROPIC_BASE_URL=" + srv.URL}
	rc, out := runBin(t, "", env, "check", "--exact", "examples/no-cache.json")
	if rc != 2 || !strings.Contains(out, "is 1200 tokens") || strings.Contains(out, "~1200") {
		t.Errorf("--exact: rc=%d, want exact '1200 tokens' without ~:\n%s", rc, clip(out, 300))
	}
	// no key: clear error, exit 1
	rc, out = runBin(t, "", []string{"ANTHROPIC_API_KEY="}, "check", "--exact", "examples/no-cache.json")
	if rc != 1 || !strings.Contains(out, "ANTHROPIC_API_KEY") {
		t.Errorf("--exact keyless: rc=%d out=%s", rc, clip(out, 200))
	}
}

// mockSSEUpstream imitates an OpenAI Chat Completions SSE endpoint that only
// reports usage when stream_options.include_usage is set.
func mockSSEUpstream() *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var doc map[string]any
		json.Unmarshal(body, &doc)
		include := false
		if so, ok := doc["stream_options"].(map[string]any); ok {
			include, _ = so["include_usage"].(bool)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, `data: {"choices":[{"delta":{"content":"hi"}}]}`+"\n\n")
		if include {
			fmt.Fprint(w, `data: {"usage":{"prompt_tokens":2000,"completion_tokens":5,"prompt_tokens_details":{"cached_tokens":1500}}}`+"\n\n")
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
}

// TestObserveEndToEnd drives the real observer handler over HTTP: forwarding,
// include_usage injection, usage metering, and the oversized-body 413.
func TestObserveEndToEnd(t *testing.T) {
	up := mockSSEUpstream()
	defer up.Close()

	o := &observer{
		upstream:     up.URL,
		includeUsage: true,
		client:       &http.Client{},
		byModel:      map[string]*agg{},
		findings:     map[string]int{},
		seenPfx:      map[string]map[string]bool{},
	}
	proxy := httptest.NewServer(o)
	defer proxy.Close()

	req := `{"model":"gpt-4o","stream":true,"messages":[{"role":"user","content":"hi"}]}`
	resp, err := http.Post(proxy.URL+"/v1/chat/completions", "application/json", strings.NewReader(req))
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || !strings.Contains(string(body), "cached_tokens") {
		t.Fatalf("forwarding/injection: status=%d body=%s", resp.StatusCode, clip(string(body), 200))
	}

	// observe() runs after the response is fully streamed; the client can
	// finish reading before the handler's bookkeeping does — poll briefly.
	var a *agg
	var metered int
	for i := 0; i < 100; i++ {
		o.mu.Lock()
		a, metered = o.byModel["gpt-4o"], o.metered
		o.mu.Unlock()
		if a != nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if a == nil || a.cacheRead != 1500 || a.in != 500 || metered != 1 {
		o.mu.Lock()
		t.Fatalf("metering: agg=%+v metered=%d n=%d findings=%v byModel=%v", a, metered, o.n, o.findings, o.byModel)
	}

	// oversized body → 413, not silent truncation
	huge := strings.Repeat("x", maxBodyBytes+10)
	resp, err = http.Post(proxy.URL+"/v1/chat/completions", "application/json", strings.NewReader(huge))
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Errorf("oversized body: status=%d, want 413", resp.StatusCode)
	}

	// with --upstream set, ANY path forwards (that's what forcing an
	// upstream means) — the unroutable-path 502 applies only when routing
	// by path, so check that mode with a second observer.
	o2 := &observer{client: &http.Client{}, byModel: map[string]*agg{},
		findings: map[string]int{}, seenPfx: map[string]map[string]bool{}}
	proxy2 := httptest.NewServer(o2)
	defer proxy2.Close()
	resp, _ = http.Post(proxy2.URL+"/unknown", "application/json", strings.NewReader("{}"))
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		t.Errorf("unroutable path (no upstream): status=%d, want 502", resp.StatusCode)
	}
}
