package main

// OpenAI (automatic prefix caching): request model and detection rules.
//
// OpenAI auto-caches the longest byte-identical prefix of prompts >=1024 tokens
// — no cache_control, TTL, or breakpoints. So the checks that matter are:
// prefix size, prefix stability (volatile content near the front), and whether
// there's a stable leading system prefix at all.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
)

type OARequest struct {
	Model         string           `json:"model"`
	Messages      []OAMsg          `json:"messages"`
	Tools         json.RawMessage  `json:"tools"`
	Stream        bool             `json:"stream"`
	StreamOptions *OAStreamOptions `json:"stream_options"`
	// Responses API shape: instructions is the system-equivalent, input is
	// a string or a message-item array. Without these, a Responses body
	// parsed as all-empty and diff returned a wrong "byte-identical" OK.
	Instructions string          `json:"instructions"`
	Input        json.RawMessage `json:"input"`
}

type OAStreamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

func (r *OARequest) reportsUsage() bool {
	// "input": null is not a Responses request — clients that serialize
	// unused fields as null must keep their Chat Completions WARN.
	if rawPresent(r.Input) || r.Instructions != "" {
		return true // Responses API streams always include usage
	}
	return !r.Stream || (r.StreamOptions != nil && r.StreamOptions.IncludeUsage)
}

type OAMsg struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

func (m OAMsg) text() string { return contentText(m.Content) }

// contentText flattens a content value (string OR part array with text
// fields — both Chat Completions and Responses parts carry "text").
func contentText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	if raw[0] == '"' {
		var s string
		json.Unmarshal(raw, &s)
		return s
	}
	var parts []struct {
		Text string `json:"text"`
	}
	json.Unmarshal(raw, &parts)
	var sb strings.Builder
	for _, p := range parts {
		sb.WriteString(p.Text)
	}
	return sb.String()
}

// leadingTurnText is the first message's text — the stable-leading-prefix
// stand-in when a request has no tools, instructions, or leading system.
func (r *OARequest) leadingTurnText() string {
	if len(r.Messages) > 0 {
		return r.Messages[0].text()
	}
	if items := r.inputItems(); len(items) > 0 {
		return items[0].text()
	}
	return ""
}

// rawPresent reports a RawMessage that is present and meaningful — clients
// serializing unused fields as null or [] must read as "absent" (a literal
// "[]" prefix would otherwise suppress the leading-turn volatile fallback).
func rawPresent(raw json.RawMessage) bool {
	t := bytes.TrimSpace(raw)
	return len(t) > 0 && !bytes.Equal(t, []byte("null")) && !bytes.Equal(t, []byte("[]"))
}

// inputItems returns Responses-API input as messages; a bare string input
// becomes one user message.
func (r *OARequest) inputItems() []OAMsg {
	if len(r.Input) == 0 {
		return nil
	}
	if r.Input[0] == '"' {
		return []OAMsg{{Role: "user", Content: r.Input}}
	}
	var items []OAMsg
	json.Unmarshal(r.Input, &items)
	return items
}

// prefixText is the intended-stable prefix OpenAI would cache: tools +
// instructions + the LEADING run of system/developer content. A system
// message appearing after variable user turns is not part of the stable
// leading prefix and must not be credited to it.
func (r *OARequest) prefixText() string {
	var sb strings.Builder
	if rawPresent(r.Tools) {
		sb.Write(r.Tools)
	}
	sb.WriteString(r.Instructions)
	for _, m := range r.Messages {
		if m.Role != "system" && m.Role != "developer" {
			break
		}
		sb.WriteString(m.text())
	}
	for _, m := range r.inputItems() {
		if m.Role != "system" && m.Role != "developer" {
			break
		}
		sb.WriteString(m.text())
	}
	return sb.String()
}

func (r *OARequest) promptText() string {
	var sb strings.Builder
	if rawPresent(r.Tools) {
		sb.Write(r.Tools)
	}
	sb.WriteString(r.Instructions)
	for _, m := range r.Messages {
		sb.WriteString(m.text())
	}
	for _, m := range r.inputItems() {
		sb.WriteString(m.text())
	}
	return sb.String()
}

func (r *OARequest) hasSystemFirst() bool {
	if r.Instructions != "" {
		return true
	}
	if len(r.Messages) > 0 {
		return r.Messages[0].Role == "system" || r.Messages[0].Role == "developer"
	}
	if items := r.inputItems(); len(items) > 0 {
		return items[0].Role == "system" || items[0].Role == "developer"
	}
	return false
}

func checkOpenAI(r *OARequest) []Finding {
	var f []Finding
	prompt := r.promptText()
	prefix := r.prefixText()
	if strings.TrimSpace(prefix) == "" {
		// No tools/instructions/leading-system: OpenAI's cacheable prefix is
		// the leading turn — scanning the WHOLE prompt would flag volatile
		// content at the end, exactly where the fix text says to put it.
		prefix = r.leadingTurnText()
	}
	// Exact o200k BPE, not an estimate (framing tokens add a little on top,
	// so this can only under-count — a below-minimum warning is never
	// missed). Capped: past 256KB the 1024 threshold is long since settled.
	tok := countTokensCapped(prompt)

	if tok < 1024 {
		f = append(f, Finding{"WARN",
			"Prompt below OpenAI's cache minimum",
			fmt.Sprintf("The prompt is %d tokens (o200k count); OpenAI only auto-caches prompts of 1024+ tokens, so none of it is cached.", tok),
			"Caching engages automatically once your stable prefix crosses ~1024 tokens."})
	}
	for _, v := range volatile {
		if m := v.re.FindString(prefix); m != "" {
			f = append(f, Finding{"HIGH",
				"Volatile content in the cached prefix",
				fmt.Sprintf("Your prompt prefix contains a volatile value (%s: %q). OpenAI caches the longest byte-identical prefix, so anything that changes near the front means the cache never engages — silently.", v.name, clip(m, 40)),
				"Move changing values (timestamps, IDs, dates) toward the end of the prompt; keep the system message and tools byte-stable."})
			break
		}
	}
	if !r.reportsUsage() {
		f = append(f, Finding{"WARN",
			"Streaming without usage reporting",
			"This request streams but doesn't set stream_options.include_usage, so OpenAI omits token usage from the stream — your logs (and cachedoctor) can't see whether the cache is hitting.",
			"Add \"stream_options\": {\"include_usage\": true}, or run observe with --include-usage to inject it."})
	}
	if tok >= 1024 && !r.hasSystemFirst() {
		f = append(f, Finding{"WARN",
			"No stable leading system prefix",
			"Your messages don't start with a system/developer message, so OpenAI's cacheable prefix begins at your first (often variable) user turn — shrinking what gets cached.",
			"Put stable instructions and tools in a leading system message; keep variable content last."})
	}
	if len(f) == 0 {
		f = append(f, Finding{"OK",
			"No cache anti-patterns found",
			"Cache-friendly for OpenAI: a stable, 1024+ token prefix with no volatile content — OpenAI caches it automatically.", ""})
	}
	return f
}

// injectIncludeUsage returns body with stream_options.include_usage set, so a
// streamed OpenAI response reports token usage. The body comes back unchanged
// if it isn't a streaming request, already reports usage, or can't be parsed.
func injectIncludeUsage(body []byte) []byte {
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber() // preserve large integers (seeds, ids) exactly on re-marshal
	var m map[string]any
	if dec.Decode(&m) != nil {
		return body
	}
	if stream, _ := m["stream"].(bool); !stream {
		return body
	}
	so, _ := m["stream_options"].(map[string]any)
	if iu, _ := so["include_usage"].(bool); iu {
		return body
	}
	if so == nil {
		so = map[string]any{}
	}
	so["include_usage"] = true
	m["stream_options"] = so
	out, err := json.Marshal(m)
	if err != nil {
		return body
	}
	return out
}

func diffOpenAI(a, b *OARequest) []Finding {
	pa, pb := a.prefixText(), b.prefixText()
	if pa != pb {
		return []Finding{{"HIGH",
			"Cacheable prefix differs between calls",
			firstDiff(pa, pb),
			"Keep the system message + tools byte-identical across calls; OpenAI caches only the stable leading prefix."}}
	}
	return []Finding{{"OK",
		"Cacheable prefix is byte-identical",
		"The stable leading prefix (tools + instructions + leading system) matches between the two calls — OpenAI should cache it. (Conversation-history divergence is not compared; for multi-turn traffic the history itself is part of OpenAI's cache.)", ""}}
}
