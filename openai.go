package main

// OpenAI (automatic prefix caching): request model and detection rules.
//
// OpenAI auto-caches the longest byte-identical prefix of prompts >=1024 tokens
// — no cache_control, TTL, or breakpoints. So the checks that matter are:
// prefix size, prefix stability (volatile content near the front), and whether
// there's a stable leading system prefix at all.

import (
	"encoding/json"
	"fmt"
	"strings"
)

type OARequest struct {
	Model    string          `json:"model"`
	Messages []OAMsg         `json:"messages"`
	Tools    json.RawMessage `json:"tools"`
}

type OAMsg struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

func (m OAMsg) text() string {
	if len(m.Content) == 0 {
		return ""
	}
	if m.Content[0] == '"' {
		var s string
		json.Unmarshal(m.Content, &s)
		return s
	}
	var parts []struct {
		Text string `json:"text"`
	}
	json.Unmarshal(m.Content, &parts)
	var sb strings.Builder
	for _, p := range parts {
		sb.WriteString(p.Text)
	}
	return sb.String()
}

// prefixText is the intended-stable prefix OpenAI would cache: tools + system.
func (r *OARequest) prefixText() string {
	var sb strings.Builder
	sb.Write(r.Tools)
	for _, m := range r.Messages {
		if m.Role == "system" || m.Role == "developer" {
			sb.WriteString(m.text())
		}
	}
	return sb.String()
}

func (r *OARequest) promptText() string {
	var sb strings.Builder
	sb.Write(r.Tools)
	for _, m := range r.Messages {
		sb.WriteString(m.text())
	}
	return sb.String()
}

func (r *OARequest) hasSystemFirst() bool {
	return len(r.Messages) > 0 &&
		(r.Messages[0].Role == "system" || r.Messages[0].Role == "developer")
}

func checkOpenAI(r *OARequest) []Finding {
	var f []Finding
	prompt := r.promptText()
	prefix := r.prefixText()
	if strings.TrimSpace(prefix) == "" {
		prefix = prompt
	}
	tok := estTokens(prompt)

	if tok < 1024 {
		f = append(f, Finding{"WARN",
			"Prompt below OpenAI's cache minimum",
			fmt.Sprintf("The prompt is ~%d tokens; OpenAI only auto-caches prompts of 1024+ tokens, so none of it is cached.", tok),
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
		"The stable prefix (tools + system) matches between the two calls — OpenAI should cache it.", ""}}
}
