package main

// Shared detection primitives used by both providers' rules.

import (
	"fmt"
	"regexp"
	"strings"
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

// estTokens is a rough (chars/4) estimate — good enough to flag threshold risk.
func estTokens(s string) int { return len(s) / 4 }

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
