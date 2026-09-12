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
