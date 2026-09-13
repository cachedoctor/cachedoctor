package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math"
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
	// non-finite input must terminate ("Infinity" parses as a valid float)
	if got := epochSeconds(math.Inf(1)); got != 0 {
		t.Errorf("epochSeconds(+Inf) = %v, want 0", got)
	}
	if got := epochSeconds(math.NaN()); got != 0 {
		t.Errorf("epochSeconds(NaN) = %v, want 0", got)
	}
	if got := parseTS(map[string]any{"timestamp": "Infinity"}); got != 0 {
		t.Errorf("parseTS(Infinity) = %v, want 0", got)
	}
}

func TestRateForGuards(t *testing.T) {
	// substring lookalikes and open-weights models must not be priced
	for _, m := range []string{
		"llama-3.1-sonnetto", "gpt-oss-120b", "groq/gpt-oss-120b", "gemini-2.5-flash",
		// third-party families that collided with bare variant tokens
		"solar-pro", "upstage/solar-pro2", "cybertron-7b", "terranova-1", "nano-llm-v1", "codexglue-base",
	} {
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
	cases := map[string]string{
		"azure/o1-mini":            "openai",
		"openai/davinci-002":       "openai", // prefix is the only signal
		"openai/codex-mini-latest": "openai",
		"anthropic/claude-x":       "anthropic",
	}
	for m, want := range cases {
		if got := providerOf(m); got != want {
			t.Errorf("providerOf(%q) = %q, want %q", m, got, want)
		}
	}
}

func TestReportsUsageNullInput(t *testing.T) {
	// "input": null must not suppress the Chat Completions streaming WARN
	body := []byte(`{"model":"gpt-4o","stream":true,"input":null,"messages":[{"role":"user","content":"hi"}]}`)
	fs, err := checkBytes(body)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, f := range fs {
		if strings.Contains(f.Title, "usage reporting") {
			found = true
		}
	}
	if !found {
		t.Error("null input suppressed the streaming-usage WARN")
	}
}

func TestExtractUsageBytesSSE(t *testing.T) {
	body := []byte(`event: message_start
data: {"usage":{"input_tokens":10,"cache_read_input_tokens":1200}}

event: message_delta
data: {"usage":{"output_tokens":33}}
`)
	u := extractUsageBytes(body, "claude-sonnet-4-5", -1)
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

func TestSeamTruncatedNumberDiscarded(t *testing.T) {
	// A usage number cut exactly at the capture seam must be discarded,
	// not read as its truncated prefix.
	var c capture
	head := `{"usage":{"input_tokens": 987`
	c.Write(bytes.Repeat([]byte("x"), captureLimit-len(head)))
	c.Write([]byte(head))                            // head ends after "...987"
	c.Write([]byte("654}}"))                         // rest of the number lands in the tail
	c.Write(bytes.Repeat([]byte("y"), captureLimit)) // flush it away
	u := extractUsageBytes(c.Bytes(), "m", c.seamAt())
	if u.in != 0 {
		t.Errorf("truncated seam number trusted: in=%v, want 0", u.in)
	}
	// but a number safely inside the head is still read
	var c2 capture
	c2.Write([]byte(`{"usage":{"input_tokens": 42} `))
	c2.Write(bytes.Repeat([]byte("y"), 3*captureLimit))
	if u := extractUsageBytes(c2.Bytes(), "m", c2.seamAt()); u.in != 42 {
		t.Errorf("intact head number lost: in=%v, want 42", u.in)
	}
}

func TestDisplayWidth(t *testing.T) {
	if w := displayWidth("abc"); w != 3 {
		t.Errorf("ascii: %d", w)
	}
	if w := displayWidth("提示詞"); w != 6 {
		t.Errorf("CJK should be double-width: %d", w)
	}
	if w := displayWidth("a提b"); w != 4 {
		t.Errorf("mixed: %d", w)
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
	u := extractUsageBytes(got, "claude-sonnet-4-5", c.seamAt())
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
	u := extractUsageBytes(c.Bytes(), "claude-sonnet-4-5", c.seamAt())
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

func hasTitle(fs []Finding, sub string) bool {
	for _, f := range fs {
		if strings.Contains(f.Title, sub) {
			return true
		}
	}
	return false
}

func TestVolatileDetection(t *testing.T) {
	// Volatile date INSIDE the cached span (breakpoint on the system block).
	req := []byte(`{"model":"claude-sonnet-4-5","system":[{"type":"text","text":"Today is 2026-09-12, be helpful.","cache_control":{"type":"ephemeral","ttl":"1h"}}]}`)
	fs, err := checkBytes(req)
	if err != nil {
		t.Fatal(err)
	}
	if !hasTitle(fs, "Volatile") {
		t.Errorf("want volatile-content HIGH finding, got %+v", fs)
	}
}

// TestBreakpointPositionAware locks in the position semantics: the cached
// span ends at the LAST breakpoint (render order tools → system → messages).
func TestBreakpointPositionAware(t *testing.T) {
	// 1. Tools-only breakpoint: volatile content in the (uncached) system
	// prompt must NOT flag — it sits below the breakpoint, exactly where the
	// fix text tells users to put it.
	toolsOnly := []byte(`{"model":"claude-sonnet-4-5","system":"Today is 2026-09-12.","tools":[{"name":"t","description":"d","cache_control":{"type":"ephemeral","ttl":"1h"}}]}`)
	fs, err := checkBytes(toolsOnly)
	if err != nil {
		t.Fatal(err)
	}
	if hasTitle(fs, "Volatile") {
		t.Errorf("volatile below the breakpoint must not flag, got %+v", fs)
	}

	// 2. Conversation-only breakpoint (what cachedoctord's injectConversation
	// emits): the span includes tools+system+history, so a long conversation
	// must not get a bogus "below the minimum" WARN.
	long := strings.Repeat("stable conversation content. ", 400)
	convo := []byte(`{"model":"claude-sonnet-4-5","system":"You are helpful.","messages":[` +
		`{"role":"user","content":"` + long + `"},` +
		`{"role":"assistant","content":[{"type":"text","text":"ok","cache_control":{"type":"ephemeral","ttl":"1h"}}]}]}`)
	fs, err = checkBytes(convo)
	if err != nil {
		t.Fatal(err)
	}
	if hasTitle(fs, "below the minimum") {
		t.Errorf("conversation-breakpoint span is large; below-minimum WARN is wrong: %+v", fs)
	}
	// its default-TTL sibling in messages WOULD have been missed before:
	convoDefaultTTL := bytes.Replace(convo, []byte(`,"ttl":"1h"`), nil, 1)
	fs, err = checkBytes(convoDefaultTTL)
	if err != nil {
		t.Fatal(err)
	}
	if !hasTitle(fs, "5-minute") {
		t.Errorf("message-block breakpoint with default TTL must WARN, got %+v", fs)
	}
}

// TestBlockLevelSpan locks in per-BLOCK (not per-region) span semantics.
func TestBlockLevelSpan(t *testing.T) {
	// Breakpoint on the FIRST of two tools: only that tiny tool is cached —
	// the huge second tool must not inflate the span past the minimum.
	big := strings.Repeat("stable schema text ", 400)
	firstTool := []byte(`{"model":"claude-sonnet-4-5","tools":[` +
		`{"name":"a","description":"tiny","cache_control":{"type":"ephemeral","ttl":"1h"}},` +
		`{"name":"b","description":"` + big + `"}]}`)
	fs, err := checkBytes(firstTool)
	if err != nil {
		t.Fatal(err)
	}
	if !hasTitle(fs, "below the minimum") {
		t.Errorf("first-tool breakpoint: tiny span must WARN below-minimum, got %+v", fs)
	}
	// Volatile date in the SECOND tool (below the breakpoint) must not flag.
	volBelow := []byte(`{"model":"claude-sonnet-4-5","tools":[` +
		`{"name":"a","description":"` + big + `","cache_control":{"type":"ephemeral","ttl":"1h"}},` +
		`{"name":"b","description":"generated 2026-09-13"}]}`)
	fs, err = checkBytes(volBelow)
	if err != nil {
		t.Fatal(err)
	}
	if hasTitle(fs, "Volatile") {
		t.Errorf("volatile below the tool breakpoint must not flag, got %+v", fs)
	}
}

// TestMsgSpanCountsTextNotJSON: the conversation span estimate must count
// flattened text — raw JSON over-counted ~10x and hid below-minimum WARNs.
func TestMsgSpanCountsTextNotJSON(t *testing.T) {
	// 150 tiny block-array messages: text is trivial (~300 tokens of "ok"),
	// but the raw JSON syntax is ~10x that.
	var msgs []string
	for i := 0; i < 149; i++ {
		msgs = append(msgs, `{"role":"user","content":[{"type":"text","text":"ok"}]}`)
	}
	msgs = append(msgs, `{"role":"assistant","content":[{"type":"text","text":"ok","cache_control":{"type":"ephemeral","ttl":"1h"}}]}`)
	body := []byte(`{"model":"claude-sonnet-4-5","messages":[` + strings.Join(msgs, ",") + `]}`)
	fs, err := checkBytes(body)
	if err != nil {
		t.Fatal(err)
	}
	if !hasTitle(fs, "below the minimum") {
		t.Errorf("tiny conversation span must WARN below-minimum (JSON syntax must not count), got %+v", fs)
	}
}

// TestOpenAIVolatileAtEnd: with no stable prefix at all, the fallback scans
// only the leading turn — a date in the FINAL user turn (where the fix text
// says to put it) must not flag.
// TestMsgBlockLevelCut: the span ends at the breakpoint's BLOCK inside the
// final message — later blocks in the same message must not count.
func TestMsgBlockLevelCut(t *testing.T) {
	big := strings.Repeat("stable words ", 800)
	body := []byte(`{"model":"claude-sonnet-4-5","messages":[{"role":"user","content":[` +
		`{"type":"text","text":"tiny","cache_control":{"type":"ephemeral","ttl":"1h"}},` +
		`{"type":"text","text":"` + big + `"}]}]}`)
	fs, err := checkBytes(body)
	if err != nil {
		t.Fatal(err)
	}
	if !hasTitle(fs, "below the minimum") {
		t.Errorf("span cut at breakpoint block: tiny span must WARN, got %+v", fs)
	}
	// breakpoint on the LAST block: the big text counts, no WARN
	body2 := []byte(`{"model":"claude-sonnet-4-5","messages":[{"role":"user","content":[` +
		`{"type":"text","text":"` + big + `"},` +
		`{"type":"text","text":"tiny","cache_control":{"type":"ephemeral","ttl":"1h"}}]}]}`)
	fs, _ = checkBytes(body2)
	if hasTitle(fs, "below the minimum") {
		t.Errorf("breakpoint on last block: big span must not WARN, got %+v", fs)
	}
}

// TestToolTranscriptSpan: tool_result nested content and tool_use input are
// cacheable span content — an agent transcript with a final-message
// breakpoint must not false-WARN below-minimum.
func TestToolTranscriptSpan(t *testing.T) {
	bigResult := strings.Repeat("file contents line here ", 600)
	body := []byte(`{"model":"claude-sonnet-4-5","system":"agent","messages":[` +
		`{"role":"user","content":"do the task"},` +
		`{"role":"assistant","content":[{"type":"tool_use","input":{"path":"main.go"}}]},` +
		`{"role":"user","content":[{"type":"tool_result","content":[{"type":"text","text":"` + bigResult + `"}]}]},` +
		`{"role":"assistant","content":[{"type":"text","text":"done","cache_control":{"type":"ephemeral","ttl":"1h"}}]}]}`)
	fs, err := checkBytes(body)
	if err != nil {
		t.Fatal(err)
	}
	if hasTitle(fs, "below the minimum") {
		t.Errorf("tool_result content must count toward the span, got %+v", fs)
	}
}

func TestOpenAIVolatileAtEnd(t *testing.T) {
	long := strings.Repeat("stable words ", 400)
	body := []byte(`{"model":"gpt-4o","messages":[` +
		`{"role":"user","content":"` + long + `"},` +
		`{"role":"system","content":"mid-conversation note"},` +
		`{"role":"user","content":"today is 2026-09-13, next question"}]}`)
	fs, err := checkBytes(body)
	if err != nil {
		t.Fatal(err)
	}
	if hasTitle(fs, "Volatile") {
		t.Errorf("volatile content at the end of the prompt must not flag, got %+v", fs)
	}
}

func TestTTLDefaultNotDrift(t *testing.T) {
	// {type:ephemeral} and {type:ephemeral,ttl:"5m"} are the same behavior.
	a := []byte(`{"model":"claude-sonnet-4-5","system":[{"type":"text","text":"s","cache_control":{"type":"ephemeral"}}]}`)
	b := []byte(`{"model":"claude-sonnet-4-5","system":[{"type":"text","text":"s","cache_control":{"type":"ephemeral","ttl":"5m"}}]}`)
	fs, err := diffBytes(a, b)
	if err != nil {
		t.Fatal(err)
	}
	if hasTitle(fs, "breakpoints changed") {
		t.Errorf("default vs explicit 5m must not read as drift, got %+v", fs)
	}
}

func TestDiffBreakpointDrift(t *testing.T) {
	// Byte-identical content, breakpoint removed in call B: the old diff
	// returned a wrong OK; placement is part of the cache contract.
	a := []byte(`{"model":"claude-sonnet-4-5","system":[{"type":"text","text":"stable","cache_control":{"type":"ephemeral"}}]}`)
	b := []byte(`{"model":"claude-sonnet-4-5","system":[{"type":"text","text":"stable"}]}`)
	fs, err := diffBytes(a, b)
	if err != nil {
		t.Fatal(err)
	}
	if !hasTitle(fs, "breakpoints changed") {
		t.Errorf("want breakpoint-drift HIGH, got %+v", fs)
	}
	// TTL change alone also flags
	c := []byte(`{"model":"claude-sonnet-4-5","system":[{"type":"text","text":"stable","cache_control":{"type":"ephemeral","ttl":"1h"}}]}`)
	fs, _ = diffBytes(a, c)
	if !hasTitle(fs, "breakpoints changed") {
		t.Errorf("want TTL-drift HIGH, got %+v", fs)
	}
}

func TestUnsupportedProviderRejected(t *testing.T) {
	// Llama/Gemini bodies must error (exit 1), not get Anthropic advice and
	// a bogus exit-2 through the CI gate.
	body := []byte(`{"model":"llama-3.1-70b-instruct","system":"` + strings.Repeat("stable ", 500) + `"}`)
	if _, err := checkBytes(body); err == nil {
		t.Error("llama body: want unsupported-provider error, got verdict")
	}
	if _, err := diffBytes(body, body); err == nil {
		t.Error("llama diff: want unsupported-provider error")
	}
}

func TestMinPrefixTokens(t *testing.T) {
	cases := map[string]int{
		"claude-fable-5-1":          512,
		"claude-opus-5":             512,
		"claude-haiku-4-5-20251001": 4096,
		"claude-opus-4-6":           4096,
		"claude-opus-4-7":           2048,
		"claude-3-5-haiku-20241022": 2048,
		"claude-opus-4-8":           1024,
		"claude-sonnet-4-5":         1024,
		"claude-sonnet-5":           1024,
	}
	for model, want := range cases {
		r := &Request{Model: model}
		if got := r.minPrefixTokens(); got != want {
			t.Errorf("minPrefixTokens(%s) = %d, want %d", model, got, want)
		}
	}
}

func TestRateForFineTuneSlugs(t *testing.T) {
	// real OpenAI fine-tune model strings: ft:<base>:<org>::<id>
	in, _, _, ok := rateFor("ft:gpt-4o-2024-08-06:acme::abc123")
	if !ok || in >= 10.0 {
		t.Errorf("ft slug priced at in=%v ok=%v — must use base/ft rates, not the astra catch-all", in, ok)
	}
	if _, _, _, ok := rateFor("sec-cyber-scanner"); ok {
		t.Error("non-gpt name with a variant token must not price")
	}
}

func TestToFRejectsGarbage(t *testing.T) {
	// "NaN"/"Inf" parse as valid floats and poisoned every total; negatives
	// deflated sibling rows.
	for _, v := range []any{"NaN", "Inf", "-Infinity", "-100", float64(-5)} {
		if f, ok := toF(v); ok {
			t.Errorf("toF(%v) accepted %v — must reject non-finite/negative", v, f)
		}
	}
	if f, ok := toF("500"); !ok || f != 500 {
		t.Errorf("toF(\"500\") = %v ok=%v", f, ok)
	}
}

func TestExtractUsageBytesResponsesCached(t *testing.T) {
	// OpenAI Responses: input_tokens INCLUDES cached_tokens — observe must
	// subtract or cached tokens bill at input AND read rates.
	body := []byte(`{"usage":{"input_tokens":1000,"input_tokens_details":{"cached_tokens":600},"output_tokens":10}}`)
	u := extractUsageBytes(body, "gpt-4o", -1)
	if u.in != 400 || u.cacheRead != 600 {
		t.Errorf("responses usage: in=%v read=%v, want 400/600", u.in, u.cacheRead)
	}
	// Anthropic: input_tokens EXCLUDES cache reads — no subtraction
	body2 := []byte(`{"usage":{"input_tokens":100,"cache_read_input_tokens":900}}`)
	u = extractUsageBytes(body2, "claude-sonnet-5", -1)
	if u.in != 100 || u.cacheRead != 900 {
		t.Errorf("anthropic usage: in=%v read=%v, want 100/900", u.in, u.cacheRead)
	}
}

func TestExtractUsageWriteBreakdown(t *testing.T) {
	m := map[string]any{
		"model": "claude-sonnet-5",
		"usage": map[string]any{
			"cache_creation": map[string]any{
				"ephemeral_5m_input_tokens": float64(100000),
				"ephemeral_1h_input_tokens": float64(50000),
			},
		},
	}
	if u := extractUsage(m); u.cacheWrite != 150000 {
		t.Errorf("per-TTL write breakdown: cacheWrite=%v, want 150000", u.cacheWrite)
	}
}
