package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
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
	// Malformed JSON, and well-formed JSON that isn't a request object —
	// null/arrays/strings unmarshal into the structs as no-ops and used to
	// get a diagnostic verdict instead of an error.
	for _, in := range []string{"{nope", "null", "[1,2,3]", `"hello"`, "42", "  \n null"} {
		if _, err := checkBytes([]byte(in)); err == nil {
			t.Errorf("checkBytes(%q): want error, got verdict", in)
		}
	}
	if _, err := diffBytes([]byte("null"), []byte("{}")); err == nil {
		t.Error("diffBytes(null, {}): want error")
	}
}

func TestReadLineCapped(t *testing.T) {
	// An oversized line is skipped (nil, nil) and reading continues — a
	// bufio.Scanner would stop dead and silently drop the rest of the file.
	big := strings.Repeat("x", 100)
	input := "line1\n" + big + "\nline3"
	br := bufio.NewReaderSize(strings.NewReader(input), 16)
	l1, err := readLineCapped(br, 50)
	if err != nil || strings.TrimSpace(string(l1)) != "line1" {
		t.Fatalf("line1: %q err=%v", l1, err)
	}
	skip, err := readLineCapped(br, 50)
	if skip != nil || err != nil {
		t.Fatalf("oversized: want (nil,nil), got %q err=%v", skip, err)
	}
	l3, err := readLineCapped(br, 50)
	if string(l3) != "line3" || err != io.EOF {
		t.Fatalf("line3: %q err=%v", l3, err)
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
	// family fallback for an unknown future model — must track the family's
	// NEWEST priced entry, not a stale generation
	if in, read, _, ok := rateFor("claude-sonnet-9-experimental"); !ok || in != 2.0 || read != 0.2 {
		t.Errorf("sonnet family fallback: got in=%v read=%v ok=%v, want claude-sonnet-5 rates (2.0/0.2)", in, read, ok)
	}
	if in, _, _, ok := rateFor("gpt-7-preview"); !ok || in != 10.0 {
		t.Errorf("gpt family fallback: got in=%v ok=%v, want gpt-6-astra rates (10.0)", in, ok)
	}
	// variant families match before the generic gpt token
	if in, _, _, ok := rateFor("gpt-6.1-luna-preview"); !ok || in != 0.2 {
		t.Errorf("luna family fallback: got in=%v ok=%v, want gpt-5.6-luna rates (0.2)", in, ok)
	}
	if in, _, _, ok := rateFor("gpt-5.7-codex"); !ok || in != 1.75 {
		t.Errorf("codex family fallback: got in=%v ok=%v, want gpt-5.3-codex rates (1.75)", in, ok)
	}
	if in, _, _, ok := rateFor("claude-opus-6-internal"); !ok || in != 5.0 {
		t.Errorf("opus family fallback: got in=%v ok=%v, want claude-opus-5 rates (5.0)", in, ok)
	}
	if in, read, _, ok := rateFor("claude-fable-6"); !ok || in != 10.0 || read != 0.25 {
		t.Errorf("fable family fallback: got in=%v read=%v ok=%v, want claude-fable-5-1 rates (10.0/0.25)", in, read, ok)
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

func TestExtractUsageWriteOnly(t *testing.T) {
	// Write-without-read traffic (the "paying the premium for nothing"
	// pathology) must survive extraction — it used to be silently dropped
	// by analyze's usable-record filter.
	m := map[string]any{
		"model": "claude-sonnet-5",
		"usage": map[string]any{"input_tokens": float64(0), "cache_creation_input_tokens": float64(5000)},
	}
	if u := extractUsage(m); u.cacheWrite != 5000 {
		t.Errorf("got %+v", u)
	}
}

func TestExtractUsageResponsesAPI(t *testing.T) {
	// Responses API: input_tokens INCLUDES the cached portion, and cached
	// lives under input_tokens_details.
	m := map[string]any{
		"model": "gpt-5.6-luna",
		"usage": map[string]any{
			"input_tokens":         float64(10000),
			"output_tokens":        float64(50),
			"input_tokens_details": map[string]any{"cached_tokens": float64(9000)},
		},
	}
	u := extractUsage(m)
	if u.in != 1000 || u.cacheRead != 9000 {
		t.Errorf("responses usage: got in=%v read=%v, want 1000/9000", u.in, u.cacheRead)
	}
	// double nesting: response.usage.{...}
	m2 := map[string]any{
		"model":    "gpt-4o",
		"response": map[string]any{"usage": map[string]any{"prompt_tokens": float64(700), "completion_tokens": float64(9)}},
	}
	if u := extractUsage(m2); u.in != 700 || u.out != 9 {
		t.Errorf("response.usage nesting: got %+v", u)
	}
}

func TestEpochSeconds(t *testing.T) {
	sec := 1.7570000e9
	for _, v := range []float64{sec, sec * 1e3, sec * 1e6, sec * 1e9} {
		if got := epochSeconds(v); got != sec {
			t.Errorf("epochSeconds(%v) = %v, want %v", v, got, sec)
		}
	}
}

func TestRateForGuards(t *testing.T) {
	// substring lookalikes and open-weights models must not be priced
	for _, m := range []string{"llama-3.1-sonnetto", "gpt-oss-120b", "groq/gpt-oss-120b", "gemini-2.5-flash"} {
		if _, _, _, ok := rateFor(m); ok {
			t.Errorf("rateFor(%q): priced, want unsupported", m)
		}
	}
	// mini families keep their own (much cheaper) tier
	if in, _, _, ok := rateFor("o1-mini-2024-09-12"); !ok || in > 5 {
		t.Errorf("o1-mini fallback: in=%v ok=%v, want its own cheap tier", in, ok)
	}
}

func TestResponsesAPIBodies(t *testing.T) {
	a := []byte(`{"model":"gpt-5.6-luna","stream":true,"instructions":"stable system text","input":"question one"}`)
	b := []byte(`{"model":"gpt-5.6-luna","stream":true,"instructions":"DRIFTED system text","input":"question one"}`)
	// diff must see the instructions drift (used to return a wrong OK)
	fs, err := diffBytes(a, b)
	if err != nil || !hasSev(fs, "HIGH") {
		t.Errorf("responses diff: want HIGH, got %+v err=%v", fs, err)
	}
	// volatile content in instructions must be caught by check
	v := []byte(`{"model":"gpt-4o","instructions":"Today is 2026-09-13. Be helpful.","input":"hi"}`)
	fs, _ = checkBytes(v)
	if !hasSev(fs, "HIGH") {
		t.Errorf("responses volatile: want HIGH, got %+v", fs)
	}
	// streaming Responses bodies always report usage — no WARN
	fs, _ = checkBytes(a)
	for _, f := range fs {
		if strings.Contains(f.Title, "usage reporting") {
			t.Errorf("responses stream: bogus usage-reporting WARN")
		}
	}
}

func TestProviderOfPrefixed(t *testing.T) {
	if got := providerOf("azure/o1-mini"); got != "openai" {
		t.Errorf("azure/o1-mini routed to %q", got)
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

func TestCheckOpenAIStreamUsageWarn(t *testing.T) {
	warned := func(body string) bool {
		fs, err := checkBytes([]byte(body))
		if err != nil {
			t.Fatal(err)
		}
		for _, f := range fs {
			if strings.Contains(f.Title, "usage reporting") {
				return true
			}
		}
		return false
	}
	base := `"model":"gpt-4o","messages":[{"role":"system","content":"s"}]`
	if !warned(`{` + base + `,"stream":true}`) {
		t.Error("streaming without include_usage: want WARN")
	}
	if warned(`{` + base + `,"stream":true,"stream_options":{"include_usage":true}}`) {
		t.Error("include_usage set: want no WARN")
	}
	if warned(`{` + base + `}`) {
		t.Error("non-streaming: want no WARN")
	}
}

func TestInjectIncludeUsage(t *testing.T) {
	parse := func(b []byte) *OARequest {
		var r OARequest
		if err := json.Unmarshal(b, &r); err != nil {
			t.Fatal(err)
		}
		return &r
	}
	// injected for a bare streaming request, other fields preserved
	out := injectIncludeUsage([]byte(`{"model":"gpt-4o","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	r := parse(out)
	if !r.reportsUsage() || r.Model != "gpt-4o" || len(r.Messages) != 1 {
		t.Errorf("got %s", out)
	}
	// merged into existing stream_options without clobbering it
	out = injectIncludeUsage([]byte(`{"stream":true,"stream_options":{"other":1}}`))
	var m map[string]any
	json.Unmarshal(out, &m)
	so := m["stream_options"].(map[string]any)
	if so["include_usage"] != true || so["other"] != float64(1) {
		t.Errorf("got %s", out)
	}
	// untouched: non-streaming, already set, invalid JSON
	for _, in := range []string{
		`{"model":"gpt-4o"}`,
		`{"stream":true,"stream_options":{"include_usage":true}}`,
		`{nope`,
	} {
		if got := injectIncludeUsage([]byte(in)); string(got) != in {
			t.Errorf("want unchanged, got %s from %s", got, in)
		}
	}
}

func TestCaptureBounded(t *testing.T) {
	var c capture
	head := []byte(`data: {"usage":{"input_tokens":10,"cache_read_input_tokens":1200}}` + "\n")
	c.Write(head)
	filler := bytes.Repeat([]byte("data: {\"delta\":{\"text\":\"x\"}}\n"), 1)
	for written := 0; written < 5*captureLimit; written += len(filler) {
		c.Write(filler)
	}
	c.Write([]byte(`data: {"usage":{"output_tokens":33}}` + "\n"))

	got := c.Bytes()
	if len(got) > 2*captureLimit+1 {
		t.Errorf("capture retained %d bytes, want <= %d", len(got), 2*captureLimit+1)
	}
	u := extractUsageBytes(got, "claude-sonnet-4-5")
	if u.in != 10 || u.cacheRead != 1200 || u.out != 33 {
		t.Errorf("usage from truncated stream: got %+v", u)
	}
}

func TestCaptureSmallStreamIntact(t *testing.T) {
	var c capture
	in := []byte("hello, small stream")
	for _, b := range in { // worst case: one byte per Write
		c.Write([]byte{b})
	}
	if got := c.Bytes(); !bytes.Equal(got, in) {
		t.Errorf("got %q, want %q", got, in)
	}
}

func TestCaptureNoSeamMatch(t *testing.T) {
	// A usage key split across the elided middle must not produce a bogus
	// match: the NUL separator keeps regexes from spanning the gap.
	var c capture
	c.Write(bytes.Repeat([]byte("x"), captureLimit-len(`"input_tokens":`)))
	c.Write([]byte(`"input_tokens":`)) // head ends exactly with the key
	c.Write([]byte("123"))             // tail begins with digits
	c.Write(bytes.Repeat([]byte("y"), captureLimit-3))
	u := extractUsageBytes(c.Bytes(), "claude-sonnet-4-5")
	if u.in != 0 {
		t.Errorf("seam produced a false match: in = %v, want 0", u.in)
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

// TestEstTokensCalibrated checks the script-aware estimator against o200k
// reference counts (Anthropic's tokenizer is unpublished; o200k magnitudes
// are the proxy). Band: never more than ~10% over the reference (over-counting
// can hide a below-minimum prefix), never less than 65% of it.
func TestEstTokensCalibrated(t *testing.T) {
	cases := []struct {
		name string
		text string
		ref  int // o200k reference count
	}{
		{"prose", strings.Repeat("The quick brown fox jumps over the lazy dog and keeps running through the quiet forest. ", 10), 171},
		{"json", strings.Repeat(`{"type":"object","properties":{"query":{"type":"string"}},"required":["query"]}`, 10), 190},
		{"traditional-zh", strings.Repeat("提示詞快取可以大幅降低大型語言模型的輸入成本，但它會靜默失效。", 10), 250},
		{"simplified-zh", strings.Repeat("提示词缓存可以大幅降低大模型的输入成本，但它会静默失效。", 10), 200},
		{"korean", strings.Repeat("프롬프트 캐시는 입력 비용을 크게 줄일 수 있습니다.", 10), 150},
	}
	for _, c := range cases {
		got := estTokens(c.text)
		lo, hi := int(0.65*float64(c.ref)), int(1.10*float64(c.ref))
		if got < lo || got > hi {
			t.Errorf("%s: estTokens = %d, want within [%d, %d] (ref %d)", c.name, got, lo, hi, c.ref)
		}
	}
}

func TestAnthropicPrefixExact(t *testing.T) {
	// The mock returns a larger count when the payload carries system/tools;
	// the prefix is the difference of the two calls.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/messages/count_tokens" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		if r.Header.Get("x-api-key") != "sk-test" {
			t.Error("missing api key header")
		}
		body, _ := io.ReadAll(r.Body)
		n := 300
		if bytes.Contains(body, []byte(`"system"`)) {
			n = 1500
		}
		fmt.Fprintf(w, `{"input_tokens":%d}`, n)
	}))
	defer srv.Close()

	doc := map[string]any{
		"model":    "claude-sonnet-4-5",
		"system":   "big stable prompt",
		"messages": []any{map[string]any{"role": "user", "content": "q"}},
	}
	n, err := anthropicPrefixExact("sk-test", srv.URL, doc)
	if err != nil || n != 1200 {
		t.Errorf("got n=%d err=%v, want 1200", n, err)
	}
	// No prefix at all: zero without any API call.
	n, err = anthropicPrefixExact("sk-test", "http://invalid.invalid", map[string]any{"model": "claude-x"})
	if err != nil || n != 0 {
		t.Errorf("prefixless: got n=%d err=%v, want 0", n, err)
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
