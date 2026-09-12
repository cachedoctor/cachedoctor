package main

// Shared detection primitives used by both providers' rules.

import (
	"fmt"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

// volatile matches content that breaks byte-identity between calls when it
// appears in the cacheable prefix.
var volatile = []struct {
	name string
	re   *regexp.Regexp
}{
	{"ISO timestamp", regexp.MustCompile(`\d{4}-\d{2}-\d{2}[T ]\d{2}:\d{2}`)},
	{"date", regexp.MustCompile(`\b\d{4}-\d{2}-\d{2}\b`)},
	{"clock time", regexp.MustCompile(`\b\d{1,2}:\d{2}:\d{2}\b`)},
	{"UUID", regexp.MustCompile(`\b[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}\b`)},
	{"epoch/long number", regexp.MustCompile(`\b\d{10,13}\b`)},
	{"phrase like \"today is\"", regexp.MustCompile(`(?i)\b(today is|current (date|time)|as of|right now)\b`)},
}

// estTokens estimates tokens for content with no exact tokenizer available
// (Anthropic's is unpublished; `check --exact` gets real counts from the
// count_tokens API). Script-aware, calibrated against o200k measurements
// (prose ≈5.2 bytes/token, symbol-heavy ≈3.3; CJK counted per rune:
// Han ≈1.45 runes/token, kana ≈1.5, hangul ≈1.85). Divisors lean toward
// under-counting: the failure mode is an extra borderline warning, never a
// silently-missed below-minimum prefix.
func estTokens(s string) int {
	var han, kana, hangul float64
	var latinBytes, proseBytes int
	for _, r := range s {
		switch {
		case unicode.Is(unicode.Han, r):
			han++
		case unicode.Is(unicode.Hiragana, r), unicode.Is(unicode.Katakana, r):
			kana++
		case unicode.Is(unicode.Hangul, r):
			hangul++
		default:
			sz := utf8.RuneLen(r)
			latinBytes += sz
			if r == ' ' || unicode.IsLetter(r) {
				proseBytes += sz
			}
		}
	}
	tok := han/1.45 + kana/1.5 + hangul/1.85
	if latinBytes > 0 {
		proseFrac := float64(proseBytes) / float64(latinBytes)
		tok += float64(latinBytes) / (3.3 + 1.9*proseFrac)
	}
	return int(tok)
}

// firstDiff shows the first point where two strings diverge, with context.
func firstDiff(a, b string) string {
	n := len(a)
	if len(b) < n {
		n = len(b)
	}
	i := 0
	for i < n && a[i] == b[i] {
		i++
	}
	if i == len(a) && i == len(b) {
		return "(identical)"
	}
	start := i - 30
	if start < 0 {
		start = 0
	}
	return fmt.Sprintf("first divergence at byte %d:\n      A: …%s\n      B: …%s\n         %s^", i,
		clip(safe(a, start, i+30), 60), clip(safe(b, start, i+30), 60),
		strings.Repeat(" ", min(i-start, 60)))
}

func safe(s string, lo, hi int) string {
	if lo < 0 {
		lo = 0
	}
	if hi > len(s) {
		hi = len(s)
	}
	return s[lo:hi]
}

func clip(s string, n int) string {
	s = strings.ReplaceAll(s, "\n", "⏎")
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

func sameSet(a, b []string) bool {
	m := map[string]int{}
	for _, x := range a {
		m[x]++
	}
	for _, x := range b {
		m[x]--
	}
	for _, v := range m {
		if v != 0 {
			return false
		}
	}
	return true
}
