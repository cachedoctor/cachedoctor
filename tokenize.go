package main

// Exact OpenAI token counting: a stdlib-only implementation of tiktoken's
// o200k_base BPE (the encoding for gpt-4o, o1/o3/o4, and later). The vocab is
// embedded gzipped and loaded lazily on first use, so commands that never
// touch OpenAI content pay nothing.
//
// "Exact" means exact BPE of the content we measure (system + tools text) —
// per-message wire framing adds a few tokens per message on top, which only
// nudges counts *up*: our threshold checks can under-count, never over-count,
// so a "below the minimum" warning is never silently missed.

import (
	"bufio"
	"bytes"
	"compress/gzip"
	_ "embed"
	"encoding/base64"
	"fmt"
	"regexp"
	"sync"
	"unicode/utf8"
)

//go:embed o200k_base.tiktoken.gz
var o200kGz []byte

var (
	o200kOnce  sync.Once
	o200kRanks map[string]int
)

func loadO200k() {
	o200kOnce.Do(func() {
		zr, err := gzip.NewReader(bytes.NewReader(o200kGz))
		if err != nil {
			panic("cachedoctor: bad embedded o200k vocab: " + err.Error())
		}
		o200kRanks = make(map[string]int, 200_000)
		sc := bufio.NewScanner(zr)
		sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
		for sc.Scan() {
			line := sc.Bytes()
			sp := bytes.IndexByte(line, ' ')
			if sp <= 0 {
				continue
			}
			tok, err := base64.StdEncoding.DecodeString(string(line[:sp]))
			if err != nil {
				continue
			}
			var rank int
			fmt.Sscanf(string(line[sp+1:]), "%d", &rank)
			o200kRanks[string(tok)] = rank
		}
	})
}

// Unicode White_Space, spelled out because Go's \s is ASCII-only while
// tiktoken's Rust regex uses the Unicode class.
const wsClass = `\t-\r \x{85}\x{A0}\x{1680}\x{2000}-\x{200A}\x{2028}\x{2029}\x{202F}\x{205F}\x{3000}`

// o200kMain is tiktoken's o200k_base pre-tokenizer pattern, minus its last
// two whitespace alternatives (`\s+(?!\S)` needs lookahead, which RE2 lacks;
// pretokens emulates it below). Anchored: Go regexp preserves alternation
// order, matching the reference leftmost-first semantics.
var o200kMain = regexp.MustCompile(`^(?:` +
	`[^\r\n\p{L}\p{N}]?[\p{Lu}\p{Lt}\p{Lm}\p{Lo}\p{M}]*[\p{Ll}\p{Lm}\p{Lo}\p{M}]+(?i:'s|'t|'re|'ve|'m|'ll|'d)?` +
	`|[^\r\n\p{L}\p{N}]?[\p{Lu}\p{Lt}\p{Lm}\p{Lo}\p{M}]+[\p{Ll}\p{Lm}\p{Lo}\p{M}]*(?i:'s|'t|'re|'ve|'m|'ll|'d)?` +
	`|\p{N}{1,3}` +
	`| ?[^` + wsClass + `\p{L}\p{N}]+[\r\n/]*` +
	`|[` + wsClass + `]*[\r\n]+` +
	`)`)

var o200kWS = regexp.MustCompile(`^[` + wsClass + `]+`)

// pretokens splits s exactly as tiktoken's o200k pre-tokenizer does, feeding
// each piece to yield. The reference's `\s+(?!\S)` alternative (a whitespace
// run leaves its last character to lead the following token) is emulated:
// when a whitespace match is followed by non-whitespace and is longer than
// one character, the final whitespace character is given back.
func pretokens(s string, yield func(string)) {
	for i := 0; i < len(s); {
		if m := o200kMain.FindString(s[i:]); m != "" {
			yield(m)
			i += len(m)
			continue
		}
		m := o200kWS.FindString(s[i:])
		if m == "" { // unmatchable byte (invalid UTF-8): consume one rune
			_, sz := utf8.DecodeRuneInString(s[i:])
			yield(s[i : i+sz])
			i += sz
			continue
		}
		if i+len(m) < len(s) { // followed by non-whitespace
			_, lsz := utf8.DecodeLastRuneInString(m)
			if len(m) > lsz {
				yield(m[:len(m)-lsz])
				i += len(m) - lsz
				continue
			}
		}
		yield(m)
		i += len(m)
	}
}

// bpeCount returns how many tokens one pre-token piece merges down to.
func bpeCount(piece string) int {
	n := len(piece)
	if n <= 1 {
		return n
	}
	if _, ok := o200kRanks[piece]; ok {
		return 1
	}
	// starts are the current part boundaries into piece.
	starts := make([]int, n+1)
	for i := range starts {
		starts[i] = i
	}
	rankAt := func(i int) int { // rank of merging parts i and i+1
		if i+2 >= len(starts) {
			return -1
		}
		if r, ok := o200kRanks[piece[starts[i]:starts[i+2]]]; ok {
			return r
		}
		return -1
	}
	for len(starts) > 2 {
		best, bi := -1, -1
		for i := 0; i+2 < len(starts); i++ {
			if r := rankAt(i); r >= 0 && (best == -1 || r < best) {
				best, bi = r, i
			}
		}
		if bi < 0 {
			break
		}
		starts = append(starts[:bi+1], starts[bi+2:]...)
	}
	return len(starts) - 1
}

// countTokensO200k is the exact o200k_base token count of s.
func countTokensO200k(s string) int {
	loadO200k()
	n := 0
	pretokens(s, func(p string) { n += bpeCount(p) })
	return n
}
