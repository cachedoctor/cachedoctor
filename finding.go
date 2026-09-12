package main

import "fmt"

// A Finding is one diagnosed cache issue (or the all-clear).
type Finding struct {
	Sev    string // "HIGH", "WARN", "INFO", "OK"
	Title  string
	Detail string
	Fix    string
}

var sevIcon = map[string]string{"HIGH": "🔴", "WARN": "🟡", "INFO": "⚪", "OK": "🟢"}

// funnelURL is where the "want it fixed automatically?" footer points.
// cachedoctor diagnoses; cachedoctord (the paid proxy by the same team)
// fixes in-flight. Swap for a product page when one exists.
const funnelURL = "https://github.com/cachedoctor"

// funnelLine is the single tasteful footer printed where a money number
// appears (analyze, observe summary) — never on check/diff, and only when
// something is actually recoverable.
func funnelLine() string {
	return "cachedoctord (same team) plugs these leaks automatically — cache injection,\nkeep-warm, request normalization → " + funnelURL
}

// report prints findings and returns the exit code: 2 if any HIGH, else 0.
func report(title string, f []Finding) int {
	fmt.Printf("cachedoctor · %s\n\n", title)
	high := 0
	for _, x := range f {
		if x.Sev == "HIGH" {
			high++
		}
		fmt.Printf("%s %s — %s\n", sevIcon[x.Sev], x.Sev, x.Title)
		if x.Detail != "" {
			fmt.Printf("    %s\n", x.Detail)
		}
		if x.Fix != "" {
			fmt.Printf("    fix: %s\n", x.Fix)
		}
		fmt.Println()
	}
	if high > 0 {
		return 2
	}
	return 0
}

func filterSev(fs []Finding) []Finding {
	var out []Finding
	for _, f := range fs {
		if f.Sev == "HIGH" || f.Sev == "WARN" {
			out = append(out, f)
		}
	}
	return out
}
