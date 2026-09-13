package main

// Shared detection primitives used by both providers' rules.

import (
	"fmt"
	"regexp"
	"strings"
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
	// epochs: 10 digits starting with 1 (seconds, 2001–2033) or 13 (ms) —
	// anchored on the leading 1 so phone numbers and ISBNs don't false-HIGH
	{"epoch timestamp", regexp.MustCompile(`\b1\d{9}\b|\b1\d{12}\b`)},
	{"phrase like \"today is\"", regexp.MustCompile(`(?i)\b(today is|current (date|time)|as of|right now)\b`)},
}

// tokenScanCap bounds how many bytes threshold rules tokenize. Every
// threshold in the tool is <= 4096 tokens; past 256KB the verdict cannot
// change, and unbounded BPE on a 32MB proxied body costs seconds and
// gigabytes (measured) for nothing.
const tokenScanCap = 256 << 10

// countTokensCapped is countTokensO200k over at most tokenScanCap bytes —
// a floor, which for >=-threshold checks is exactly as good.
func countTokensCapped(s string) int {
	if len(s) > tokenScanCap {
		s = s[:runeFloor(s, tokenScanCap)]
	}
	return countTokensO200k(s)
}

// estTokens estimates tokens for content with no exact tokenizer available
// (Anthropic's is unpublished; `check --exact` gets real counts from the
// count_tokens API). It is the exact o200k count with a 10% safety discount:
// both tokenizers are byte-level BPEs of similar density, and the discount
// keeps errors on the under-counting side — an extra borderline warning,
// never a silently-missed below-minimum prefix. (An earlier bytes-per-token
// heuristic over-counted whitespace/symbol runs by up to 25x, exactly the
// direction that hides an ignored prefix.)
func estTokens(s string) int {
	return countTokensCapped(s) * 9 / 10
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
	ell := "…"
	if start == 0 {
		ell = ""
	}
	// Caret alignment uses display columns: CJK and fullwidth glyphs
	// occupy two terminal cells.
	pre := strings.ReplaceAll(safe(a, start, i), "\n", "⏎")
	pad := min(displayWidth(ell)+displayWidth(pre), 60)
	return fmt.Sprintf("first divergence at byte %d:\n      A: %s%s\n      B: %s%s\n         %s^", i,
		ell, clip(safe(a, start, i+30), 60), ell, clip(safe(b, start, i+30), 60),
		strings.Repeat(" ", pad))
}

// displayWidth is the terminal-column width of s: wide (East Asian W/F)
// runes count 2, everything else 1. The ranges cover CJK, kana, hangul, and
// fullwidth forms — the cases that actually show up in prompts.
func displayWidth(s string) int {
	w := 0
	for _, r := range s {
		switch {
		case r >= 0x1100 && r <= 0x115F, // hangul jamo
			r >= 0x2E80 && r <= 0x303E,   // CJK radicals, punctuation
			r >= 0x3041 && r <= 0x33FF,   // kana, CJK symbols
			r >= 0x3400 && r <= 0x4DBF,   // CJK ext A
			r >= 0x4E00 && r <= 0x9FFF,   // CJK unified
			r >= 0xAC00 && r <= 0xD7A3,   // hangul syllables
			r >= 0xF900 && r <= 0xFAFF,   // CJK compat
			r >= 0xFE30 && r <= 0xFE4F,   // CJK compat forms
			r >= 0xFF00 && r <= 0xFF60,   // fullwidth forms
			r >= 0x20000 && r <= 0x2FFFD: // CJK ext B+
			w += 2
		default:
			w++
		}
	}
	return w
}

// runeFloor moves i left to the nearest rune boundary so byte slicing never
// splits a multi-byte character into invalid UTF-8.
func runeFloor(s string, i int) int {
	for i > 0 && i < len(s) && !utf8.RuneStart(s[i]) {
		i--
	}
	return i
}

func safe(s string, lo, hi int) string {
	if lo < 0 {
		lo = 0
	}
	if hi > len(s) {
		hi = len(s)
	}
	return s[runeFloor(s, lo):runeFloor(s, hi)]
}

func clip(s string, n int) string {
	s = strings.ReplaceAll(s, "\n", "⏎")
	if len(s) <= n {
		return s
	}
	return s[:runeFloor(s, n)] + "…"
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
