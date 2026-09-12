package main

// Anthropic Messages API: request model and detection rules. Caching is
// explicit (cache_control breakpoints, TTLs, per-model prefix minimums).

import (
	"encoding/json"
	"fmt"
	"slices"
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
	Type         string        `json:"type"`
	Text         string        `json:"text"`
	CacheControl *CacheControl `json:"cache_control"`
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
func (r *Request) breakpoints() int {
	n := 0
	for _, t := range r.Tools {
		if t.CacheControl != nil {
			n++
		}
	}
	for _, b := range r.systemBlocks() {
		if b.CacheControl != nil {
			n++
		}
	}
	for _, m := range r.Messages {
		if len(m.Content) > 0 && m.Content[0] == '[' {
			var blocks []Block
			json.Unmarshal(m.Content, &blocks)
			for _, b := range blocks {
				if b.CacheControl != nil {
					n++
				}
			}
		}
	}
	return n
}

// ttls returns every cache_control TTL found (empty string = 5m default).
func (r *Request) ttls() []string {
	var out []string
	for _, t := range r.Tools {
		if t.CacheControl != nil {
			out = append(out, t.CacheControl.TTL)
		}
	}
	for _, b := range r.systemBlocks() {
		if b.CacheControl != nil {
			out = append(out, b.CacheControl.TTL)
		}
	}
	return out
}

func (r *Request) toolsText() string {
	var sb strings.Builder
	for _, t := range r.Tools {
		sb.WriteString(t.Name)
		sb.WriteString(t.Description)
		sb.Write(t.InputSchema)
	}
	return sb.String()
}

func (r *Request) minPrefixTokens() int {
	if strings.Contains(strings.ToLower(r.Model), "haiku") {
		return 2048
	}
	return 1024
}

// prefixRepr is a stable representation of the cacheable prefix, used by
// observe to detect drift between consecutive calls.
func prefixRepr(r *Request) string {
	return strings.Join(toolNames(r), ",") + "\x00" + r.systemText()
}

func check(r *Request) []Finding {
	var f []Finding
	bp := r.breakpoints()
	prefix := r.toolsText() + r.systemText()
	prefixTok := estTokens(prefix)

	// R1 — no caching at all
	if bp == 0 {
		if prefixTok >= 500 {
			f = append(f, Finding{"HIGH",
				"No prompt caching enabled",
				fmt.Sprintf("There is no cache_control anywhere, but your tools+system prefix is ~%d tokens of stable, reusable content — currently billed at full price on every call.", prefixTok),
				"Add cache_control (ephemeral, ttl:\"1h\") to the last tool and/or the end of the system prompt."})
		} else {
			f = append(f, Finding{"INFO",
				"No prompt caching enabled",
				"No cache_control found. Your stable prefix is small, so caching may not help much yet.", ""})
		}
		// with nothing cached, the rest of the prefix rules are moot
	} else {
		// R6 — too many breakpoints
		if bp > 4 {
			f = append(f, Finding{"HIGH",
				fmt.Sprintf("%d cache breakpoints (max is 4)", bp),
				"Anthropic honors at most 4 cache_control breakpoints; extras are ignored.",
				"Keep 4 or fewer, at the stable/volatile boundaries."})
		}
		// R3 — sub-threshold prefix
		if prefixTok < r.minPrefixTokens() {
			f = append(f, Finding{"WARN",
				"Cached prefix may be below the minimum",
				fmt.Sprintf("The cacheable prefix is ~%d tokens; below ~%d, cache_control is silently ignored for this model.", prefixTok, r.minPrefixTokens()),
				"Cache a longer stable prefix, or accept that caching won't engage here."})
		}
		// R2 — default 5-minute TTL
		for _, ttl := range r.ttls() {
			if ttl == "" {
				f = append(f, Finding{"WARN",
					"Using the default 5-minute cache TTL",
					"cache_control without an explicit ttl expires after 5 minutes (changed from 1h on 2026-03-06). If your calls are spaced further apart, every one misses.",
					"Set ttl:\"1h\" for spaced-out reuse, or keep the cache warm."})
				break
			}
		}
	}

	// R7 — volatility in the (would-be) cached prefix: breaks byte-identity
	for _, v := range volatile {
		if m := v.re.FindString(prefix); m != "" {
			f = append(f, Finding{"HIGH",
				"Volatile content in the cached prefix",
				fmt.Sprintf("Your tools/system prefix contains a volatile value (%s: %q). If it changes between calls, the prefix is no longer byte-identical and every call misses — silently, at full price.", v.name, clip(m, 40)),
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

	if len(f) == 0 {
		f = append(f, Finding{"OK",
			"Cacheable prefix is byte-identical",
			"Tools and system prompt match exactly between the two calls — the prefix should hit the cache. If you're still missing, check the TTL (see `check`) or call spacing.", ""})
	}
	return f
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
