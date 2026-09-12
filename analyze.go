package main

// analyze: real hit rate + $/mo recoverable from a usage log (JSONL).

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
)

// urec is one usage record extracted from a log line or API response.
type urec struct {
	model                          string
	in, out, cacheRead, cacheWrite float64
	ts                             float64 // unix seconds, 0 if unknown
}

// agg accumulates usage and cost per model.
type agg struct {
	calls                     int
	in, cacheRead, cacheWrite float64
	spent, recoverable        float64
}

const targetHit = 0.80 // the hit rate the "recoverable" number lifts you to

// add folds one usage record into the aggregate at the given $/1M rates.
func (a *agg) add(u urec, inR, readR, writeR float64) {
	a.calls++
	a.in += u.in
	a.cacheRead += u.cacheRead
	a.cacheWrite += u.cacheWrite
	a.spent += (u.in*inR + u.cacheRead*readR + u.cacheWrite*writeR) / 1e6
	total := u.in + u.cacheRead + u.cacheWrite
	if want := targetHit*total - u.cacheRead; want > 0 {
		if want > u.in {
			want = u.in // can only recover currently-full-price tokens
		}
		a.recoverable += want * (inR - readR) / 1e6
	}
}

func getAgg(byModel map[string]*agg, model string) *agg {
	a := byModel[model]
	if a == nil {
		a = &agg{}
		byModel[model] = a
	}
	return a
}

// hitPct is the cache hit rate as a percentage of all input-side tokens.
func hitPct(in, read, write float64) float64 {
	if t := in + read + write; t > 0 {
		return 100 * read / t
	}
	return 0
}

func cmdAnalyze(path string) int {
	var rdr io.Reader = os.Stdin
	if path != "-" {
		fh, err := os.Open(path)
		if err != nil {
			fmt.Fprintln(os.Stderr, "cachedoctor:", err)
			return 1
		}
		defer fh.Close()
		rdr = fh
	}

	byModel := map[string]*agg{}
	var parsed, skipped int
	var gMin, gMax float64
	sc := bufio.NewScanner(rdr)
	sc.Buffer(make([]byte, 0, 64*1024), 8<<20)
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		var m map[string]any
		if json.Unmarshal(line, &m) != nil {
			continue
		}
		u := extractUsage(m)
		if u.model == "" || (u.in == 0 && u.cacheRead == 0) {
			continue
		}
		inR, readR, writeR, ok := rateFor(u.model)
		if !ok { // Anthropic + OpenAI only; skip anything else
			skipped++
			continue
		}
		parsed++
		getAgg(byModel, u.model).add(u, inR, readR, writeR)
		if u.ts > 0 {
			if gMin == 0 || u.ts < gMin {
				gMin = u.ts
			}
			if u.ts > gMax {
				gMax = u.ts
			}
		}
	}

	if parsed == 0 {
		fmt.Println("cachedoctor: no usable Anthropic/OpenAI usage records found (need model + token counts).")
		if skipped > 0 {
			fmt.Printf("  (%d records skipped — only Anthropic and OpenAI are supported)\n", skipped)
		}
		return 1
	}

	span := gMax - gMin
	monthly := func(v float64) float64 {
		if span > 0 {
			return v * (30 * 86400 / span)
		}
		return v
	}
	proj := "/mo"
	if span <= 0 {
		proj = ""
	}

	models := make([]string, 0, len(byModel))
	var tCalls int
	var tSpent, tRecover, tIn, tRead, tWrite float64
	for name, a := range byModel {
		models = append(models, name)
		tCalls += a.calls
		tSpent += a.spent
		tRecover += a.recoverable
		tIn += a.in
		tRead += a.cacheRead
		tWrite += a.cacheWrite
	}
	sort.Slice(models, func(i, j int) bool {
		return byModel[models[i]].recoverable > byModel[models[j]].recoverable
	})

	fmt.Printf("cachedoctor · analyze %s\n\n", path)
	win := ""
	if span > 0 {
		win = " over " + humanDur(span)
	}
	fmt.Printf("analyzed %d calls across %d model(s)%s", tCalls, len(models), win)
	if skipped > 0 {
		fmt.Printf("  (skipped %d unsupported)", skipped)
	}
	fmt.Print("\n\n")
	fmt.Printf("  spent ~$%.2f  ·  cache hit rate %.0f%%  ·  recoverable ~$%.2f%s (at %.0f%% target)\n\n",
		tSpent, hitPct(tIn, tRead, tWrite), monthly(tRecover), proj, targetHit*100)

	fmt.Printf("  %-24s %6s %6s %10s %16s\n", "model", "calls", "hit%", "spent", "recoverable"+proj)
	for _, name := range models {
		a := byModel[name]
		fmt.Printf("  %-24s %6d %5.0f%% %10.2f %16.2f\n",
			clip(name, 24), a.calls, hitPct(a.in, a.cacheRead, a.cacheWrite), a.spent, monthly(a.recoverable))
	}
	fmt.Printf("\n  (cost from LiteLLM model prices; recoverable = lifting the hit rate to\n")
	fmt.Printf("   %.0f%% — tokens that move from full price to cache-read price)\n\n", targetHit*100)
	if monthly(tRecover) >= 0.01 {
		fmt.Printf("  %s\n\n", strings.ReplaceAll(funnelLine(), "\n", "\n  "))
	}
	return 0
}

func extractUsage(m map[string]any) urec {
	var u urec
	u.model = str(m, "model")
	u.cacheRead = num(m, "cache_read_input_tokens", "cache_read_tokens", "cached_tokens")
	u.cacheWrite = num(m, "cache_creation_input_tokens", "cache_write_tokens")
	u.in = num(m, "input_tokens")
	if u.in == 0 { // OpenAI: prompt_tokens includes the cached portion
		if pt := num(m, "prompt_tokens"); pt > 0 {
			if u.in = pt - u.cacheRead; u.in < 0 {
				u.in = 0
			}
		}
	}
	u.out = num(m, "output_tokens", "completion_tokens")
	u.ts = parseTS(m)
	return u
}

// num finds a numeric field by any of the candidate keys, at the top level or
// nested under usage / prompt_tokens_details / message / response.
func num(m map[string]any, keys ...string) float64 {
	try := func(mm map[string]any) (float64, bool) {
		for _, k := range keys {
			if v, ok := mm[k]; ok {
				if f, ok := toF(v); ok {
					return f, true
				}
			}
		}
		return 0, false
	}
	if f, ok := try(m); ok {
		return f
	}
	for _, nest := range []string{"usage", "prompt_tokens_details", "message", "response"} {
		if sub, ok := m[nest].(map[string]any); ok {
			if f, ok := try(sub); ok {
				return f
			}
			if sub2, ok := sub["prompt_tokens_details"].(map[string]any); ok {
				if f, ok := try(sub2); ok {
					return f
				}
			}
		}
	}
	return 0
}

func str(m map[string]any, key string) string {
	if s, ok := m[key].(string); ok {
		return s
	}
	for _, nest := range []string{"usage", "response", "message"} {
		if sub, ok := m[nest].(map[string]any); ok {
			if s, ok := sub[key].(string); ok {
				return s
			}
		}
	}
	return ""
}

func toF(v any) (float64, bool) {
	switch x := v.(type) {
	case float64:
		return x, true
	case json.Number:
		f, err := x.Float64()
		return f, err == nil
	case string:
		f, err := strconv.ParseFloat(x, 64)
		return f, err == nil
	}
	return 0, false
}

func parseTS(m map[string]any) float64 {
	for _, k := range []string{"ts", "timestamp", "created", "created_at"} {
		if v, ok := m[k]; ok {
			switch x := v.(type) {
			case float64:
				return x
			case string:
				if t, err := time.Parse(time.RFC3339, x); err == nil {
					return float64(t.Unix())
				}
				if f, err := strconv.ParseFloat(x, 64); err == nil {
					return f
				}
			}
		}
	}
	return 0
}

func humanDur(sec float64) string {
	switch {
	case sec >= 86400:
		return fmt.Sprintf("%.1fd", sec/86400)
	case sec >= 3600:
		return fmt.Sprintf("%.1fh", sec/3600)
	case sec >= 60:
		return fmt.Sprintf("%.0fm", sec/60)
	}
	return fmt.Sprintf("%.0fs", sec)
}
