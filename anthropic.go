package main

// Anthropic Messages API: request model and detection rules. Caching is
// explicit (cache_control breakpoints, TTLs, per-model prefix minimums).

import (
	"encoding/json"
	"fmt"
	"hash/fnv"
	"io"
	"os"
	"slices"
	"strconv"
	"strings"
)

type Request struct {
	Model     string          `json:"model"`
	System    json.RawMessage `json:"system"` // string OR []Block
	Tools     []Tool          `json:"tools"`
	Messages  []Message       `json:"messages"`
	MaxTokens int             `json:"max_tokens"`
}

type Tool struct {
	Name         string          `json:"name"`
	Description  string          `json:"description"`
	InputSchema  json.RawMessage `json:"input_schema"`
	CacheControl *CacheControl   `json:"cache_control"`
}

type CacheControl struct {
	Type string `json:"type"`
	TTL  string `json:"ttl"`
}

type Message struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"` // string OR []Block
}

type Block struct {
	Type         string          `json:"type"`
	Text         string          `json:"text"`
	Content      json.RawMessage `json:"content,omitempty"` // tool_result: nested content (string or []Block)
	Input        json.RawMessage `json:"input,omitempty"`   // tool_use: arguments JSON
	CacheControl *CacheControl   `json:"cache_control"`
}

// flatText flattens a block's cacheable content: its text, tool_result
// nested content (recursively), and tool_use arguments. Counting only .Text
// under-counted agent transcripts ~1000x and false-WARNed below-minimum.
func (b Block) flatText() string {
	var sb strings.Builder
	// Total nested-Content bytes we're willing to re-parse: the true cost of
	// hostile nesting is repeated Unmarshal of RawMessages, so it's budgeted
	// directly. 4x the scan cap can't change a verdict (counts saturate at
	// tokenScanCap); exhaustion under-counts, the safe direction.
	budget := 4 * tokenScanCap
	b.appendFlat(&sb, 0, &budget)
	return sb.String()
}

// appendFlat is flatText's bounded worker. Depth cap + scan-cap early-out +
// parse budget keep pathological nesting cheap: naive recursion re-parsed
// each level's RawMessage (containing the whole remaining chain), an
// O(depth²) blowup measured at ~15s of CPU per MB — a DoS through observe
// and CI-gate fixtures. Real tool_result nesting is 1-2 levels deep.
func (b Block) appendFlat(sb *strings.Builder, depth int, budget *int) {
	if depth > 20 || sb.Len() > tokenScanCap {
		return
	}
	sb.WriteString(b.Text)
	if len(b.Content) > 0 && *budget > 0 {
		*budget -= len(b.Content)
		if b.Content[0] == '"' {
			var s string
			json.Unmarshal(b.Content, &s)
			sb.WriteString(s)
		} else {
			var nested []Block
			json.Unmarshal(b.Content, &nested)
			for _, n := range nested {
				n.appendFlat(sb, depth+1, budget)
			}
		}
	}
	if len(b.Input) > 0 {
		sb.Write(b.Input)
	}
}

// systemText flattens `system` (string or block array) to its text.
func (r *Request) systemText() string {
	if len(r.System) == 0 {
		return ""
	}
	if r.System[0] == '"' {
		var s string
		json.Unmarshal(r.System, &s)
		return s
	}
	var sb strings.Builder
	for _, b := range r.systemBlocks() {
		sb.WriteString(b.Text)
		sb.WriteString("\n")
	}
	return sb.String()
}

func (r *Request) systemBlocks() []Block {
	if len(r.System) == 0 || r.System[0] == '"' {
		return nil
	}
	var blocks []Block
	json.Unmarshal(r.System, &blocks)
	return blocks
}

// breakpoints counts cache_control markers across tools, system, messages.
// bpInfo describes the cache_control breakpoints of a request, positioned in
// the documented render order tools → system → messages. The cached span is
// everything up to and including the LAST breakpoint — rules that ignore
// position misdiagnose real bodies (e.g. a conversation-only breakpoint
// caches tools+system+history, not "nothing").
type bpInfo struct {
	n        int
	ttls     []string // TTLs of every breakpoint ("" = 5m default)
	lastIn   string   // "tools" | "system" | "messages" | ""
	spanText string   // flattened text of the span cached by the LAST breakpoint
	volText  string   // regenerated-per-call portion of that span (volatile scan)
}

func (r *Request) breakpointInfo() bpInfo {
	var bi bpInfo
	lastTool, lastSys, lastMsg, lastMsgBlk := -1, -1, -1, -1
	for i, t := range r.Tools {
		if t.CacheControl != nil {
			bi.n++
			bi.ttls = append(bi.ttls, t.CacheControl.TTL)
			lastTool = i
		}
	}
	sysBlocks := r.systemBlocks()
	for i, b := range sysBlocks {
		if b.CacheControl != nil {
			bi.n++
			bi.ttls = append(bi.ttls, b.CacheControl.TTL)
			lastSys = i
		}
	}
	for i, m := range r.Messages {
		if len(m.Content) > 0 && m.Content[0] == '[' {
			var blocks []Block
			json.Unmarshal(m.Content, &blocks)
			for k, b := range blocks {
				if b.CacheControl != nil {
					bi.n++
					bi.ttls = append(bi.ttls, b.CacheControl.TTL)
					lastMsg, lastMsgBlk = i, k
				}
			}
		}
	}

	// The span is cut at the breakpoint's BLOCK, not its region: a
	// breakpoint on the first of two tools caches only that first tool.
	switch {
	case lastMsg >= 0:
		bi.lastIn = "messages"
		bi.volText = r.toolsTextUpTo(len(r.Tools)-1) + r.systemText()
		bi.spanText = bi.volText + r.messagesTextUpTo(lastMsg, lastMsgBlk)
	case lastSys >= 0:
		bi.lastIn = "system"
		bi.volText = r.toolsTextUpTo(len(r.Tools)-1) + r.systemTextUpTo(lastSys)
		bi.spanText = bi.volText
	case lastTool >= 0:
		bi.lastIn = "tools"
		bi.volText = r.toolsTextUpTo(lastTool)
		bi.spanText = bi.volText
	}
	return bi
}

// toolsTextUpTo flattens tool definitions through index i (inclusive).
func (r *Request) toolsTextUpTo(i int) string {
	var sb strings.Builder
	for j, t := range r.Tools {
		if j > i {
			break
		}
		sb.WriteString(t.Name)
		sb.WriteString(t.Description)
		sb.Write(t.InputSchema)
	}
	return sb.String()
}

// systemTextUpTo flattens system blocks through index i (inclusive); a
// string system prompt is returned whole.
func (r *Request) systemTextUpTo(i int) string {
	if len(r.System) > 0 && r.System[0] == '"' {
		return r.systemText()
	}
	var sb strings.Builder
	for j, b := range r.systemBlocks() {
		if j > i {
			break
		}
		sb.WriteString(b.Text)
		sb.WriteString("\n")
	}
	return sb.String()
}

// messagesTextUpTo flattens cacheable message content through message index
// i, cutting the FINAL message at block index blk (the span ends at the
// breakpoint's block, not at the end of its message). Raw JSON would
// over-count ~10x; .Text alone under-counts tool transcripts ~1000x —
// flatText covers text, tool_result nested content, and tool_use input.
func (r *Request) messagesTextUpTo(i, blk int) string {
	var sb strings.Builder
	for j, m := range r.Messages {
		if j > i {
			break
		}
		if len(m.Content) == 0 {
			continue
		}
		if m.Content[0] == '"' {
			var s string
			json.Unmarshal(m.Content, &s)
			sb.WriteString(s)
			continue
		}
		var blocks []Block
		json.Unmarshal(m.Content, &blocks)
		for k, b := range blocks {
			if j == i && k > blk {
				break
			}
			sb.WriteString(b.flatText())
		}
	}
	return sb.String()
}

func (r *Request) breakpoints() int { return r.breakpointInfo().n }

func (r *Request) toolsText() string {
	var sb strings.Builder
	for _, t := range r.Tools {
		sb.WriteString(t.Name)
		sb.WriteString(t.Description)
		sb.Write(t.InputSchema)
	}
	return sb.String()
}

// minPrefixTokens per the prompt-caching docs (verified 2026-09): below the
// model's minimum, cache_control is silently ignored. Unknown models take
// their family's strictest (highest) minimum — over-warning beats silently
// missing an ignored prefix.
func (r *Request) minPrefixTokens() int {
	m := strings.ToLower(r.Model)
	switch {
	case strings.Contains(m, "fable-5"), strings.Contains(m, "opus-5"),
		strings.Contains(m, "mythos-5"):
		return 512
	case strings.Contains(m, "haiku-4-5"), strings.Contains(m, "opus-4-6"),
		strings.Contains(m, "opus-4-5"):
		return 4096
	case strings.Contains(m, "opus-4-7"), strings.Contains(m, "mythos-preview"),
		strings.Contains(m, "haiku-3"), strings.Contains(m, "3-5-haiku"):
		return 2048
	case strings.Contains(m, "haiku"): // unknown haiku: strictest known
		return 4096
	default: // opus-4-8, sonnet 4.5/4.6/5, unknown models
		return 1024
	}
}

// supportsCaching reports whether the model has prompt caching at all.
// Caching exists on Claude 3.5+ plus Claude 3 Opus/Haiku; Claude 1/2/Instant
// and Claude 3 Sonnet (the one Claude 3+ model left out) never got it.
// Contains, not prefix: Bedrock ids wrap the name ("us.anthropic.claude-v2").
// Unknown names pass — the normal rules are the status quo, and a false
// "can't cache" verdict is worse than a borderline warning.
func (r *Request) supportsCaching() bool {
	m := strings.ToLower(r.Model)
	for _, legacy := range []string{
		"claude-instant", "claude-1", "claude-2", "claude-v1", "claude-v2",
		"claude-3-sonnet",
	} {
		if strings.Contains(m, legacy) {
			return false
		}
	}
	return true
}

// prefixRepr fingerprints the regenerated-per-call prefix (full tool
// definitions + system text), used by observe to detect drift between
// consecutive calls. Tool bodies are included: a changed description or
// schema invalidates the cache exactly like a changed name.
func prefixRepr(r *Request) string {
	h := fnv.New64a()
	io.WriteString(h, r.toolsText())
	h.Write([]byte{0})
	io.WriteString(h, r.systemText())
	return strconv.FormatUint(h.Sum64(), 16)
}

func check(r *Request) []Finding {
	var f []Finding
	bi := r.breakpointInfo()

	if !r.supportsCaching() {
		// Every rule below is advice for a cache this model doesn't have.
		// Gate before the exact-count path too: no point spending count_tokens
		// calls on a verdict that can't change.
		if bi.n > 0 {
			return []Finding{{"HIGH",
				"cache_control on a model without prompt caching",
				fmt.Sprintf("%q has no prompt caching (Claude 3.5+ and Claude 3 Opus/Haiku only) — its %d cache_control breakpoint(s) never create or read a cache; the full prompt is billed on every call.", r.Model, bi.n),
				"Move to a current Claude model to make this prefix cacheable."}}
		}
		if tsTok := estTokens(r.toolsText() + r.systemText()); tsTok >= 500 {
			return []Finding{{"HIGH",
				"Model has no prompt caching",
				fmt.Sprintf("%q has no prompt caching, and your tools+system prefix is ~%d tokens of stable content billed at full price on every call.", r.Model, tsTok),
				"Move to a current Claude model to make this prefix cacheable."}}
		}
		return []Finding{{"INFO",
			"Model has no prompt caching",
			fmt.Sprintf("%q has no prompt caching. Your stable prefix is small, so little is lost yet — but caching needs a current Claude model.", r.Model), ""}}
	}

	// Exact tools+system token count when armed (check --exact).
	tsTok := estTokens(r.toolsText() + r.systemText())
	tsApprox := "~"
	if anthropicExactCount != nil {
		if n, err := anthropicExactCount(); err == nil {
			tsTok, tsApprox = n, ""
		} else {
			fmt.Fprintf(os.Stderr, "cachedoctor: count_tokens failed (%v); falling back to the estimate\n", err)
		}
	}

	// The cached span is everything up to the LAST breakpoint's block in
	// render order (tools → system → messages). The volatile scan covers
	// only the regenerated-per-call portion of that span — appended
	// conversation history is byte-stable once written and would
	// false-positive.
	spanTok, spanApprox := tsTok, tsApprox
	volatileRegion := r.toolsText() + r.systemText()
	spanName := "tools+system prefix"
	if bi.lastIn != "" {
		volatileRegion = bi.volText
		spanTok, spanApprox = estTokens(bi.spanText), "~"
		switch bi.lastIn {
		case "tools":
			spanName = "cached span (the breakpoint is on a tool)"
		case "system":
			spanName = "cached span (through the system breakpoint)"
			// exact count covers full tools+system; only equivalent when the
			// breakpoint is on the final system block
			if tsApprox == "" && bi.spanText == r.toolsText()+r.systemText() {
				spanTok, spanApprox = tsTok, ""
			}
		case "messages":
			spanName = "tools+system+conversation span (the breakpoint is on a message)"
			if tsApprox == "" { // exact tools+system + estimated conversation
				spanTok = tsTok + estTokens(bi.spanText[len(bi.volText):])
			}
		}
	}

	// R1 — no caching at all
	if bi.n == 0 {
		if tsTok >= 500 {
			f = append(f, Finding{"HIGH",
				"No prompt caching enabled",
				fmt.Sprintf("There is no cache_control anywhere, but your tools+system prefix is %s%d tokens of stable, reusable content — currently billed at full price on every call.", tsApprox, tsTok),
				"Add cache_control (ephemeral, ttl:\"1h\") to the last tool and/or the end of the system prompt."})
		} else {
			f = append(f, Finding{"INFO",
				"No prompt caching enabled",
				"No cache_control found. Your stable prefix is small, so caching may not help much yet.", ""})
		}
		// with nothing cached, the rest of the prefix rules are moot
	} else {
		// R6 — too many breakpoints
		if bi.n > 4 {
			f = append(f, Finding{"HIGH",
				fmt.Sprintf("%d cache breakpoints (max is 4)", bi.n),
				"The API rejects requests with more than 4 cache_control breakpoints (400 error) — this request fails outright.",
				"Keep 4 or fewer breakpoints, at the stable/volatile boundaries."})
		}
		// R3 — sub-threshold cached span
		if spanTok < r.minPrefixTokens() {
			f = append(f, Finding{"WARN",
				"Cached prefix may be below the minimum",
				fmt.Sprintf("The %s is %s%d tokens; below ~%d, cache_control is silently ignored for this model.", spanName, spanApprox, spanTok, r.minPrefixTokens()),
				"Cache a longer stable prefix, or accept that caching won't engage here."})
		}
		// R2 — default 5-minute TTL (any breakpoint, wherever it sits)
		for _, ttl := range bi.ttls {
			if ttl == "" {
				f = append(f, Finding{"WARN",
					"Using the default 5-minute cache TTL",
					"cache_control without an explicit ttl expires after 5 minutes. If your calls are spaced further apart, every one misses.",
					"Set ttl:\"1h\" for spaced-out reuse, or keep the cache warm."})
				break
			}
		}
	}

	// R7 — volatility in the regenerated part of the cached (or would-be
	// cached) span. Content BELOW the last breakpoint can change freely.
	spanWord := "cached prefix"
	if bi.n == 0 {
		spanWord = "would-be cached prefix" // nothing is cached yet; don't imply otherwise
	}
	for _, v := range volatile {
		if m := v.re.FindString(volatileRegion); m != "" {
			f = append(f, Finding{"HIGH",
				"Volatile content in the cached prefix",
				fmt.Sprintf("Your %s contains a volatile value (%s: %q). If it changes between calls, the prefix is no longer byte-identical and every call misses — silently, at full price.", spanWord, v.name, clip(m, 40)),
				"Move anything that changes (timestamps, IDs, dates) out of the cached prefix, or below the last cache_control breakpoint."})
			break
		}
	}

	if len(f) == 0 {
		f = append(f, Finding{"OK",
			"No cache anti-patterns found",
			"This request looks cache-friendly: a stable prefix, a sensible breakpoint, and no volatile content. Confirm the real hit rate with `analyze` on your logs.", ""})
	}
	return f
}

// diff explains what broke byte-identity between two calls.
func diff(a, b *Request) []Finding {
	var f []Finding

	// tools: reordered? changed?
	an, bn := toolNames(a), toolNames(b)
	if !slices.Equal(an, bn) {
		if sameSet(an, bn) {
			f = append(f, Finding{"HIGH",
				"Tools reordered between calls",
				fmt.Sprintf("Same tools, different order:\n      call A: %s\n      call B: %s\n    Reordering the tools array changes the cached bytes — the cache misses.", strings.Join(an, ", "), strings.Join(bn, ", ")),
				"Emit tools in a stable, deterministic order every call."})
		} else {
			f = append(f, Finding{"HIGH",
				"Tools changed between calls",
				fmt.Sprintf("call A: %s\n    call B: %s", strings.Join(an, ", "), strings.Join(bn, ", ")),
				"Keep the tool set (and its serialization) identical for the cached prefix."})
		}
	} else {
		for i := range a.Tools {
			ta, tb := toolRepr(a.Tools[i]), toolRepr(b.Tools[i])
			if ta != tb {
				f = append(f, Finding{"HIGH",
					fmt.Sprintf("Tool %q definition differs between calls", a.Tools[i].Name),
					firstDiff(ta, tb),
					"Serialize tool definitions deterministically (stable key order, no volatile fields)."})
				break
			}
		}
	}

	// system: byte-identical?
	sa, sb := a.systemText(), b.systemText()
	if sa != sb {
		f = append(f, Finding{"HIGH",
			"System prompt differs between calls",
			firstDiff(sa, sb),
			"Move anything call-specific out of the system prompt (or below the cache breakpoint)."})
	}

	// breakpoints: placement and TTL are part of the cache contract — writes
	// happen only at breakpoints, so a moved, retimed, or vanished breakpoint
	// invalidates reuse even when every byte of content matches.
	ca, cb := cacheSig(a), cacheSig(b)
	if !slices.Equal(ca, cb) {
		f = append(f, Finding{"HIGH",
			"Cache breakpoints changed between calls",
			fmt.Sprintf("call A: %s\n    call B: %s\n    Cache entries are written only at breakpoint positions; matching content with different breakpoints does not reuse the cache.", sigString(ca), sigString(cb)),
			"Keep cache_control placement and TTLs identical across calls."})
	}

	if len(f) == 0 {
		f = append(f, Finding{"OK",
			"Cacheable prefix is byte-identical",
			"Tools, system prompt, and cache breakpoints match exactly between the two calls — the prefix should hit the cache. If you're still missing, check the TTL (see `check`) or call spacing. (Conversation-history divergence is not compared.)", ""})
	}
	return f
}

// cacheSig is the ordered list of breakpoint positions and TTLs — the part
// of the cache contract that byte-comparing content can't see. Tool
// breakpoints are keyed by tool NAME, not index, so a pure reorder doesn't
// double-report on top of the "Tools reordered" finding.
func cacheSig(r *Request) []string {
	var sig []string
	seen := map[string]int{}
	for _, t := range r.Tools {
		seen[t.Name]++
		if t.CacheControl != nil {
			name := t.Name
			if seen[t.Name] > 1 { // duplicate names: disambiguate by occurrence
				name = fmt.Sprintf("%s#%d", t.Name, seen[t.Name])
			}
			sig = append(sig, fmt.Sprintf("tools[%s] ttl=%s", name, ttlName(t.CacheControl.TTL)))
		}
	}
	for i, b := range r.systemBlocks() {
		if b.CacheControl != nil {
			sig = append(sig, fmt.Sprintf("system[%d] ttl=%s", i, ttlName(b.CacheControl.TTL)))
		}
	}
	for i, m := range r.Messages {
		if len(m.Content) > 0 && m.Content[0] == '[' {
			var blocks []Block
			json.Unmarshal(m.Content, &blocks)
			for j, b := range blocks {
				if b.CacheControl != nil {
					sig = append(sig, fmt.Sprintf("messages[%d].content[%d] ttl=%s", i, j, ttlName(b.CacheControl.TTL)))
				}
			}
		}
	}
	return sig
}

// ttlName normalizes the TTL: an absent ttl and an explicit "5m" are the
// same cache behavior and must not read as drift.
func ttlName(ttl string) string {
	if ttl == "" {
		return "5m"
	}
	return ttl
}

func sigString(sig []string) string {
	if len(sig) == 0 {
		return "(no cache_control)"
	}
	return strings.Join(sig, ", ")
}

func toolNames(r *Request) []string {
	out := make([]string, len(r.Tools))
	for i, t := range r.Tools {
		out[i] = t.Name
	}
	return out
}

func toolRepr(t Tool) string {
	return t.Name + "\x00" + t.Description + "\x00" + string(t.InputSchema)
}
