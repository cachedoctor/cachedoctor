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
	"container/heap"
	_ "embed"
	"encoding/base64"
	"fmt"
	"regexp"
	"strconv"
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
			rank, err := strconv.Atoi(string(line[sp+1:]))
			if err != nil {
				panic("cachedoctor: bad embedded o200k vocab line: " + string(line))
			}
			o200kRanks[string(tok)] = rank
		}
		// A truncated deflate stream surfaces here, not at NewReader — an
		// unchecked error would mean a silent partial vocab and quietly
		// wrong counts.
		if err := sc.Err(); err != nil {
			panic("cachedoctor: bad embedded o200k vocab: " + err.Error())
		}
		if len(o200kRanks) < 150_000 {
			panic(fmt.Sprintf("cachedoctor: embedded o200k vocab incomplete (%d entries)", len(o200kRanks)))
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
// O(n log n): a doubly-linked part list plus a min-heap of candidate merges,
// tie-broken by position (leftmost-first, matching the reference for
// overlapping same-rank pairs). The naive scan-for-minimum loop is O(n²) and
// took minutes on a 200KB symbol run — a DoS for observe, which tokenizes
// every proxied request.
func bpeCount(piece string) int {
	n := len(piece)
	if n <= 1 {
		return n
	}
	if _, ok := o200kRanks[piece]; ok {
		return 1
	}
	// Doubly-linked list over part start offsets: the part starting at i
	// covers piece[i:end[i]] while alive[i].
	prev := make([]int, n)
	next := make([]int, n)
	end := make([]int, n)
	alive := make([]bool, n)
	for i := 0; i < n; i++ {
		prev[i], next[i], end[i], alive[i] = i-1, i+1, i+1, true
	}
	rankOf := func(s, e int) (int, bool) {
		r, ok := o200kRanks[piece[s:e]]
		return r, ok
	}
	h := &mergeHeap{}
	for i := 0; i+1 < n; i++ {
		if r, ok := rankOf(i, i+2); ok {
			*h = append(*h, mergeCand{rank: r, left: i, right: i + 1})
		}
	}
	heap.Init(h)
	parts := n
	for h.Len() > 0 && parts > 1 {
		c := heap.Pop(h).(mergeCand)
		// Stale entries: a side was merged away, adjacency broke, or the
		// parts grew since push. Equal rank ⇒ identical pair bytes (ranks
		// are unique per byte string), so the candidate is still valid.
		if !alive[c.left] || !alive[c.right] || next[c.left] != c.right {
			continue
		}
		if r, ok := rankOf(c.left, end[c.right]); !ok || r != c.rank {
			continue
		}
		// Merge right into left.
		alive[c.right] = false
		end[c.left] = end[c.right]
		nx := next[c.right]
		next[c.left] = nx
		if nx < n {
			prev[nx] = c.left
		}
		parts--
		if p := prev[c.left]; p >= 0 {
			if r, ok := rankOf(p, end[c.left]); ok {
				heap.Push(h, mergeCand{rank: r, left: p, right: c.left})
			}
		}
		if nx < n {
			if r, ok := rankOf(c.left, end[nx]); ok {
				heap.Push(h, mergeCand{rank: r, left: c.left, right: nx})
			}
		}
	}
	return parts
}

// mergeCand is one candidate merge: the parts starting at left and right are
// adjacent and their concatenated bytes have this vocab rank.
type mergeCand struct{ rank, left, right int }

// mergeHeap orders by rank, then position — leftmost-first among equal ranks,
// matching the reference merge order for overlapping pairs (e.g. "aaa").
type mergeHeap []mergeCand

func (h mergeHeap) Len() int { return len(h) }
func (h mergeHeap) Less(i, j int) bool {
	if h[i].rank != h[j].rank {
		return h[i].rank < h[j].rank
	}
	return h[i].left < h[j].left
}
func (h mergeHeap) Swap(i, j int) { h[i], h[j] = h[j], h[i] }
func (h *mergeHeap) Push(x any)   { *h = append(*h, x.(mergeCand)) }
func (h *mergeHeap) Pop() any {
	old := *h
	n := len(old)
	x := old[n-1]
	*h = old[:n-1]
	return x
}

// countTokensO200k is the exact o200k_base token count of s.
func countTokensO200k(s string) int {
	loadO200k()
	n := 0
	pretokens(s, func(p string) { n += bpeCount(p) })
	return n
}
