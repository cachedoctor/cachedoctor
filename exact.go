package main

// check --exact: exact Anthropic prefix token counts via the free
// /v1/messages/count_tokens endpoint. BYOK and zero-retention: the key is
// read from ANTHROPIC_API_KEY, used for two small metadata calls (no message
// content is generated, nothing is billed), and never stored or logged.
//
// The prefix count is measured by difference: count(request with tools+system)
// minus count(request without them) — exactly the cacheable-prefix tokens the
// threshold rules care about.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"
)

// anthropicExactCount, when non-nil, returns the exact cacheable-prefix token
// count for the request under check. Set only by `check --exact`.
var anthropicExactCount func() (int, error)

func anthropicBaseURL() string {
	if u := os.Getenv("ANTHROPIC_BASE_URL"); u != "" {
		return u
	}
	return "https://api.anthropic.com"
}

// enableAnthropicExact arms the exact counter for one raw Anthropic request
// body. Returns an error only for an unusable body; API errors surface later,
// at counting time, and fall back to the estimate.
func enableAnthropicExact(key string, raw []byte) error {
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		return fmt.Errorf("not valid JSON: %w", err)
	}
	anthropicExactCount = func() (int, error) {
		return anthropicPrefixExact(key, anthropicBaseURL(), doc)
	}
	return nil
}

func anthropicPrefixExact(key, baseURL string, doc map[string]any) (int, error) {
	if doc["system"] == nil && doc["tools"] == nil {
		return 0, nil // no cacheable prefix; nothing to count
	}
	messages, _ := doc["messages"].([]any)
	if len(messages) == 0 {
		// count_tokens requires messages; a fixed stand-in cancels out in the
		// subtraction below.
		messages = []any{map[string]any{"role": "user", "content": "."}}
	}
	base := map[string]any{"model": doc["model"], "messages": messages}
	full := map[string]any{"model": doc["model"], "messages": messages}
	for _, k := range []string{"system", "tools"} {
		if v, ok := doc[k]; ok {
			full[k] = v
		}
	}
	nFull, err := countTokensAPI(key, baseURL, full)
	if err != nil {
		return 0, err
	}
	nBase, err := countTokensAPI(key, baseURL, base)
	if err != nil {
		return 0, err
	}
	if nFull < nBase {
		return 0, fmt.Errorf("inconsistent counts (%d < %d)", nFull, nBase)
	}
	return nFull - nBase, nil
}

func countTokensAPI(key, baseURL string, payload map[string]any) (int, error) {
	b, err := json.Marshal(payload)
	if err != nil {
		return 0, err
	}
	req, err := http.NewRequest("POST", baseURL+"/v1/messages/count_tokens", bytes.NewReader(b))
	if err != nil {
		return 0, err
	}
	req.Header.Set("x-api-key", key)
	req.Header.Set("anthropic-version", "2023-06-01")
	req.Header.Set("content-type", "application/json")
	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != 200 {
		return 0, fmt.Errorf("count_tokens: HTTP %d: %s", resp.StatusCode, clip(string(body), 120))
	}
	var out struct {
		InputTokens int `json:"input_tokens"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return 0, fmt.Errorf("count_tokens: bad response: %w", err)
	}
	return out.InputTokens, nil
}
