// cachedoctor — find out why your LLM prompt cache isn't saving you money.
//
// Prompt caching fails silently: a misconfigured cache still returns a correct
// response, you just quietly pay full price. This CLI diagnoses why.
//
//	cachedoctor check <request.json>          static anti-pattern scan (no key)
//	cachedoctor diff  <callA.json> <callB.json>  what broke byte-identity
//
// An open-source diagnostic: it tells you what's wrong; you (or your gateway) fix it.
//
// Layout: main.go (CLI dispatch) · anthropic.go / openai.go (provider models +
// rules) · rules.go (shared primitives) · finding.go (report) · analyze.go
// (log analysis) · pricing.go (rates) · observe.go (live proxy).
package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
)

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	switch os.Args[1] {
	case "check":
		os.Exit(cmdCheck(os.Args[2:]))
	case "diff":
		if len(os.Args) != 4 {
			usage()
		}
		os.Exit(cmdDiff(os.Args[2], os.Args[3]))
	case "analyze":
		if len(os.Args) != 3 {
			usage()
		}
		os.Exit(cmdAnalyze(os.Args[2]))
	case "observe":
		os.Exit(cmdObserve(os.Args[2:]))
	case "-h", "--help", "help":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "cachedoctor: unknown command %q\n", os.Args[1])
		usage()
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `cachedoctor — why your LLM prompt cache isn't saving you money

usage:
  cachedoctor check [--exact] <request.json>    scan one Anthropic request for cache anti-patterns
  cachedoctor diff  <callA.json> <callB.json>   show what broke byte-identity between two calls
  cachedoctor analyze <logs.jsonl>              real hit rate + $/mo recoverable from a usage log
  cachedoctor observe [--port N] [--upstream URL]  live proxy: diagnose real traffic as it flows

Any path may be "-" to read from stdin (e.g. cat req.json | cachedoctor check -).
Token counts: OpenAI is exact (embedded o200k tokenizer); Anthropic is a
calibrated estimate, or exact with --exact + ANTHROPIC_API_KEY (two free
count_tokens metadata calls — nothing is billed, the key is never stored).
check exits 2 if any high-severity issue is found (useful in CI).
Anthropic and OpenAI are supported; other providers in a log are skipped.
`)
	os.Exit(64)
}

// readInput reads a file, or stdin when path is "-".
func readInput(path string) ([]byte, error) {
	if path == "-" {
		return io.ReadAll(os.Stdin)
	}
	return os.ReadFile(path)
}

// peekModel reads just the model field, to route a body to its provider.
func peekModel(b []byte) string {
	var m struct {
		Model string `json:"model"`
	}
	json.Unmarshal(b, &m)
	return m.Model
}

func providerOf(model string) string {
	m := strings.ToLower(model)
	switch {
	case strings.Contains(m, "gpt"), strings.Contains(m, "openai"),
		strings.Contains(m, "chatgpt"), strings.Contains(m, "luna"),
		strings.HasPrefix(m, "o1"), strings.HasPrefix(m, "o3"), strings.HasPrefix(m, "o4"):
		return "openai"
	}
	return "anthropic"
}

// checkBytes dispatches a request body to the right provider's checks.
func checkBytes(b []byte) ([]Finding, error) {
	if providerOf(peekModel(b)) == "openai" {
		var oa OARequest
		if err := json.Unmarshal(b, &oa); err != nil {
			return nil, fmt.Errorf("not valid JSON: %w", err)
		}
		return checkOpenAI(&oa), nil
	}
	var req Request
	if err := json.Unmarshal(b, &req); err != nil {
		return nil, fmt.Errorf("not valid JSON: %w", err)
	}
	return check(&req), nil
}

// diffBytes dispatches a pair of request bodies to the right provider's diff.
func diffBytes(ba, bb []byte) ([]Finding, error) {
	if providerOf(peekModel(ba)) == "openai" || providerOf(peekModel(bb)) == "openai" {
		var a, b OARequest
		if json.Unmarshal(ba, &a) != nil || json.Unmarshal(bb, &b) != nil {
			return nil, errors.New("invalid JSON")
		}
		return diffOpenAI(&a, &b), nil
	}
	var a, b Request
	if json.Unmarshal(ba, &a) != nil || json.Unmarshal(bb, &b) != nil {
		return nil, errors.New("invalid JSON")
	}
	return diff(&a, &b), nil
}

func cmdCheck(args []string) int {
	fs := flag.NewFlagSet("check", flag.ExitOnError)
	exact := fs.Bool("exact", false,
		"exact Anthropic token counts via the free count_tokens API (needs ANTHROPIC_API_KEY; two metadata calls, nothing billed, key never stored). OpenAI counts are always exact (embedded o200k tokenizer).")
	fs.Parse(args)
	if fs.NArg() != 1 {
		usage()
	}
	path := fs.Arg(0)
	b, err := readInput(path)
	if err != nil {
		fmt.Fprintln(os.Stderr, "cachedoctor:", err)
		return 1
	}
	if *exact && providerOf(peekModel(b)) == "anthropic" {
		key := os.Getenv("ANTHROPIC_API_KEY")
		if key == "" {
			fmt.Fprintln(os.Stderr, "cachedoctor: --exact needs ANTHROPIC_API_KEY (used for two free count_tokens calls, never stored)")
			return 1
		}
		if err := enableAnthropicExact(key, b); err != nil {
			fmt.Fprintf(os.Stderr, "cachedoctor: %s: %v\n", path, err)
			return 1
		}
	}
	findings, err := checkBytes(b)
	if err != nil {
		fmt.Fprintf(os.Stderr, "cachedoctor: %s: %v\n", path, err)
		return 1
	}
	return report("check "+path, findings)
}

func cmdDiff(pa, pb string) int {
	ba, err := readInput(pa)
	if err != nil {
		fmt.Fprintln(os.Stderr, "cachedoctor:", err)
		return 1
	}
	bb, err := readInput(pb)
	if err != nil {
		fmt.Fprintln(os.Stderr, "cachedoctor:", err)
		return 1
	}
	findings, err := diffBytes(ba, bb)
	if err != nil {
		fmt.Fprintln(os.Stderr, "cachedoctor:", err)
		return 1
	}
	return report(fmt.Sprintf("diff %s ⇄ %s", pa, pb), findings)
}
