package main

// Pricing: per-model $/1M-token rates from a trimmed snapshot of LiteLLM's
// model_prices_and_context_window (Anthropic + OpenAI only):
// model -> [input, cache_read, cache_write]. Refresh with scripts/update-pricing.sh.

import (
	_ "embed"
	"encoding/json"
	"regexp"
	"strings"
)

//go:embed pricing.json
var pricingJSON []byte

var prices map[string][3]float64

func init() {
	if err := json.Unmarshal(pricingJSON, &prices); err != nil {
		panic("cachedoctor: bad embedded pricing.json: " + err.Error())
	}
}

var dateSuffix = regexp.MustCompile(`(-\d{8}|-\d{4}-\d{2}-\d{2}|-latest|-v\d+:\d+)$`)

// familyRep maps a model-family token to a representative LiteLLM key, so a
// model not priced exactly (a newer or private name) still gets its family's
// real rate. Ordered specific-first. Representatives should track each
// family's NEWEST priced entry — revisit after scripts/update-pricing.sh
// refreshes the snapshot, or unknown names get priced a generation stale.
var familyRep = []struct{ token, key string }{
	{"fable", "claude-fable-5-1"},
	{"mythos", "claude-mythos-5-1"},
	{"opus", "claude-opus-5"},
	{"sonnet", "claude-sonnet-5"},
	{"haiku", "claude-haiku-4-5"},
	{"gpt-4o-mini", "gpt-4o-mini"},
	{"o4", "o4-mini"}, {"o3", "o3"}, {"o1", "o1"},
	{"chatgpt", "chatgpt-4o-latest"},
	{"codex", "gpt-5.3-codex"}, {"nano", "gpt-5.4-nano"},
	{"cyber", "gpt-5.6-cyber"}, {"sol", "gpt-5.6-sol"},
	{"terra", "gpt-5.6-terra"}, {"luna", "gpt-5.6-luna"},
	{"astra", "gpt-6-astra"}, {"gpt", "gpt-6-astra"},
}

// rateFor returns per-1M-token USD rates (input, cache-read, cache-write) from
// the embedded LiteLLM pricing, and ok=false for anything not Anthropic/OpenAI.
func rateFor(model string) (in, read, write float64, ok bool) {
	m := strings.ToLower(strings.TrimSpace(model))
	if i := strings.LastIndex(m, "/"); i >= 0 {
		m = m[i+1:] // strip a provider prefix like "anthropic/"
	}
	keys := []string{m}
	if t := dateSuffix.ReplaceAllString(m, ""); t != m {
		keys = append(keys, t)
	}
	for _, fr := range familyRep {
		if strings.Contains(m, fr.token) {
			keys = append(keys, fr.key)
		}
	}
	for _, k := range keys {
		if p, found := prices[k]; found {
			return p[0], p[1], p[2], true
		}
	}
	return 0, 0, 0, false
}
