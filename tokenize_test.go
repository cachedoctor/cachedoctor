package main

import (
	"encoding/json"
	"os"
	"testing"
)

// TestO200kVectors validates the stdlib BPE against reference counts
// generated with Python tiktoken (o200k_base) — see testdata/o200k_vectors.json.
func TestO200kVectors(t *testing.T) {
	raw, err := os.ReadFile("testdata/o200k_vectors.json")
	if err != nil {
		t.Fatal(err)
	}
	var cases []struct {
		Text   string `json:"text"`
		Tokens int    `json:"tokens"`
	}
	if err := json.Unmarshal(raw, &cases); err != nil {
		t.Fatal(err)
	}
	for i, c := range cases {
		if got := countTokensO200k(c.Text); got != c.Tokens {
			preview := c.Text
			if len(preview) > 60 {
				preview = preview[:60] + "…"
			}
			t.Errorf("case %d %q: got %d tokens, reference says %d", i, preview, got, c.Tokens)
		}
	}
}
