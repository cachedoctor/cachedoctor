package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func hasSev(fs []Finding, sev string) bool {
	for _, f := range fs {
		if f.Sev == sev {
			return true
		}
	}
	return false
}

// TestCheckFixtures locks in the verdict for every example request body.
func TestCheckFixtures(t *testing.T) {
	wantHigh := map[string]bool{
		"callA":           false,
		"callB":           false,
		"good":            false,
		"no-cache":        true,
		"openai-good":     false,
		"openai-small":    false,
		"openai-volatile": true,
		"volatile":        true,
	}
	for name, high := range wantHigh {
		b, err := os.ReadFile(filepath.Join("examples", name+".json"))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		fs, err := checkBytes(b)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if got := hasSev(fs, "HIGH"); got != high {
			t.Errorf("%s: HIGH finding = %v, want %v (findings: %+v)", name, got, high, fs)
		}
	}
}

func TestCheckBytesInvalidJSON(t *testing.T) {
	if _, err := checkBytes([]byte("{nope")); err == nil {
		t.Error("want error for invalid JSON")
	}
}

func mustReq(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestDiffToolsReordered(t *testing.T) {
	a := []byte(`{"model":"claude-sonnet-4-5","tools":[{"name":"x"},{"name":"y"}],"system":"s"}`)
	b := []byte(`{"model":"claude-sonnet-4-5","tools":[{"name":"y"},{"name":"x"}],"system":"s"}`)
	fs, err := diffBytes(a, b)
	if err != nil {
		t.Fatal(err)
	}
	if !hasSev(fs, "HIGH") || !strings.Contains(fs[0].Title, "reordered") {
		t.Errorf("want HIGH reorder finding, got %+v", fs)
	}
}

func TestDiffToolsChanged(t *testing.T) {
	a := []byte(`{"model":"claude-sonnet-4-5","tools":[{"name":"x"}]}`)
	b := []byte(`{"model":"claude-sonnet-4-5","tools":[{"name":"z"}]}`)
	fs, err := diffBytes(a, b)
	if err != nil {
		t.Fatal(err)
	}
	if !hasSev(fs, "HIGH") || !strings.Contains(fs[0].Title, "changed") {
		t.Errorf("want HIGH changed finding, got %+v", fs)
	}
}

func TestDiffIdentical(t *testing.T) {
	b := mustReq(t, "examples/good.json")
	fs, err := diffBytes(b, b)
	if err != nil {
		t.Fatal(err)
	}
	if len(fs) != 1 || fs[0].Sev != "OK" {
		t.Errorf("want single OK finding, got %+v", fs)
	}
}

func TestDiffOpenAIPrefix(t *testing.T) {
	a := []byte(`{"model":"gpt-4o","messages":[{"role":"system","content":"stable"}]}`)
	b := []byte(`{"model":"gpt-4o","messages":[{"role":"system","content":"drifted"}]}`)
	fs, err := diffBytes(a, b)
	if err != nil {
		t.Fatal(err)
	}
	if !hasSev(fs, "HIGH") {
		t.Errorf("want HIGH prefix-differs finding, got %+v", fs)
	}
	if fs, _ := diffBytes(a, a); !hasSev(fs, "OK") {
		t.Errorf("identical OpenAI prefix: want OK, got %+v", fs)
	}
}

func TestProviderOf(t *testing.T) {
	cases := map[string]string{
		"gpt-4o":             "openai",
		"chatgpt-4o-latest":  "openai",
		"o3-mini":            "openai",
		"claude-sonnet-4-5":  "anthropic",
		"claude-haiku-4-5":   "anthropic",
		"":                   "anthropic", // default
		"anthropic/claude-x": "anthropic",
		"openai/gpt-4o-mini": "openai",
	}
	for model, want := range cases {
		if got := providerOf(model); got != want {
			t.Errorf("providerOf(%q) = %q, want %q", model, got, want)
		}
	}
}

func TestRateFor(t *testing.T) {
	// exact key
	in, read, _, ok := rateFor("claude-sonnet-4-5")
	if !ok || in != 3.0 || read != 0.3 {
		t.Errorf("exact: got in=%v read=%v ok=%v", in, read, ok)
	}
	// provider prefix stripped
	if _, _, _, ok := rateFor("anthropic/claude-sonnet-4-5"); !ok {
		t.Error("prefix strip failed")
	}
	// date suffix stripped
	if _, _, _, ok := rateFor("claude-sonnet-4-5-20991231"); !ok {
		t.Error("date-suffix strip failed")
	}
	// family fallback for an unknown future model
	if _, _, _, ok := rateFor("claude-sonnet-9-experimental"); !ok {
		t.Error("family fallback failed")
	}
	// unsupported provider
	if _, _, _, ok := rateFor("gemini-2.5-pro"); ok {
		t.Error("gemini should not be priced")
	}
}

func TestExtractUsageAnthropic(t *testing.T) {
	m := map[string]any{
		"model": "claude-sonnet-4-5",
		"usage": map[string]any{
			"input_tokens":                float64(100),
			"cache_read_input_tokens":     float64(900),
			"cache_creation_input_tokens": float64(50),
			"output_tokens":               float64(20),
		},
	}
	u := extractUsage(m)
	if u.model != "claude-sonnet-4-5" || u.in != 100 || u.cacheRead != 900 || u.cacheWrite != 50 || u.out != 20 {
		t.Errorf("got %+v", u)
	}
}

func TestExtractUsageOpenAI(t *testing.T) {
	// prompt_tokens includes the cached portion; in = prompt - cached
	m := map[string]any{
		"model": "gpt-4o",
		"usage": map[string]any{
			"prompt_tokens":     float64(1000),
			"completion_tokens": float64(40),
			"prompt_tokens_details": map[string]any{
				"cached_tokens": float64(600),
			},
		},
	}
	u := extractUsage(m)
	if u.in != 400 || u.cacheRead != 600 || u.out != 40 {
		t.Errorf("got %+v", u)
	}
}

func TestExtractUsageBytesSSE(t *testing.T) {
	body := []byte(`event: message_start
data: {"usage":{"input_tokens":10,"cache_read_input_tokens":1200}}

event: message_delta
data: {"usage":{"output_tokens":33}}
`)
	u := extractUsageBytes(body, "claude-sonnet-4-5")
	if u.in != 10 || u.cacheRead != 1200 || u.out != 33 {
		t.Errorf("got %+v", u)
	}
}

func TestAggAdd(t *testing.T) {
	var a agg
	// 1000 input-side tokens, none cached: at 80% target, 800 tokens move from
	// $3/1M to $0.3/1M → recoverable = 800 * 2.7 / 1e6.
	a.add(urec{in: 1000}, 3.0, 0.3, 3.75)
	if a.calls != 1 || a.spent != 1000*3.0/1e6 {
		t.Errorf("spent: got %+v", a)
	}
	if want := 800 * 2.7 / 1e6; a.recoverable != want {
		t.Errorf("recoverable = %v, want %v", a.recoverable, want)
	}
	// fully cached call: nothing to recover
	var b agg
	b.add(urec{cacheRead: 1000}, 3.0, 0.3, 3.75)
	if b.recoverable != 0 {
		t.Errorf("fully cached: recoverable = %v, want 0", b.recoverable)
	}
}

func TestHitPct(t *testing.T) {
	if got := hitPct(200, 800, 0); got != 80 {
		t.Errorf("hitPct = %v, want 80", got)
	}
	if got := hitPct(0, 0, 0); got != 0 {
		t.Errorf("empty hitPct = %v, want 0", got)
	}
}

func TestFirstDiff(t *testing.T) {
	got := firstDiff("abcdef", "abcxef")
	if !strings.Contains(got, "divergence at byte 3") {
		t.Errorf("got %q", got)
	}
	if got := firstDiff("same", "same"); got != "(identical)" {
		t.Errorf("identical: got %q", got)
	}
}

func TestVolatileDetection(t *testing.T) {
	req := []byte(`{"model":"claude-sonnet-4-5","system":"Today is 2026-09-12, be helpful.","tools":[{"name":"t","cache_control":{"type":"ephemeral","ttl":"1h"}}]}`)
	fs, err := checkBytes(req)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, f := range fs {
		if f.Sev == "HIGH" && strings.Contains(f.Title, "Volatile") {
			found = true
		}
	}
	if !found {
		t.Errorf("want volatile-content HIGH finding, got %+v", fs)
	}
}
