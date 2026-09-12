package main

import (
	"encoding/json"
	"os"
	"testing"
)

func TestO200kFuzzVectors(t *testing.T) {
	raw, err := os.ReadFile("testdata/o200k_fuzz.json")
	if err != nil {
		t.Skip("no fuzz vectors")
	}
	var cases []struct {
		Text   string `json:"text"`
		Tokens int    `json:"tokens"`
	}
	if err := json.Unmarshal(raw, &cases); err != nil {
		t.Fatal(err)
	}
	bad := 0
	for i, c := range cases {
		if got := countTokensO200k(c.Text); got != c.Tokens {
			bad++
			if bad <= 5 {
				t.Errorf("case %d %q: got %d, want %d", i, c.Text, got, c.Tokens)
			}
		}
	}
	if bad > 0 {
		t.Errorf("%d/%d mismatches", bad, len(cases))
	}
}
