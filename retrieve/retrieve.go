// Package retrieve ranks tools (or skills, or memories) for a query: BM25
// over words, TF-IDF over character trigrams (robust to snake_case,
// camelCase and partial words), and reciprocal-rank fusion of the two.
//
// There is no dense retriever here on purpose: the build environment has no
// embedding model. The Ranker interface is where one would plug in.
package retrieve

import (
	"math"
	"sort"
	"strings"
	"unicode"
)

type Doc struct {
	ID   string
	Text string
}

type Hit struct {
	ID    string
	Score float64
}

type Ranker interface {
	Name() string
	Rank(query string, k int) []Hit
}

// Tokens lowercases and splits on non-alphanumerics and on camelCase and
// snake_case boundaries, dropping a few stop words.
func Tokens(s string) []string {
	var out []string
	var cur []rune
	flush := func() {
		if len(cur) > 0 {
			w := string(cur)
			if !stop[w] {
				out = append(out, w)
			}
			cur = cur[:0]
		}
	}
	rs := []rune(s)
	for i, r := range rs {
		switch {
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			if unicode.IsUpper(r) && i > 0 && unicode.IsLower(rs[i-1]) {
				flush()
			}
			cur = append(cur, unicode.ToLower(r))
		default:
			flush()
		}
	}
	flush()
	return out
}

var stop = map[string]bool{"the": true, "a": true, "an": true, "of": true, "to": true, "and": true, "or": true, "in": true,
	"for": true, "is": true, "on": true, "with": true, "by": true, "be": true, "it": true, "this": true, "that": true,
	"from": true, "as": true, "at": true, "are": true, "i": true, "me": true, "my": true, "you": true, "can": true, "what": true,
	"please": true, "will": true, "if": true, "s": true}

// ---- BM25

type BM25 struct {
	K1, B   float64
	ids     []string
	tf      []map[string]float64
	dl      []float64
	avgdl   float64
	df      map[string]float64
	posting map[string][]int
}

func NewBM25(docs []Doc) *BM25 {
	b := &BM25{K1: 1.2, B: 0.75, df: map[string]float64{}, posting: map[string][]int{}}
	var tot float64
	for i, d := range docs {
		tf := map[string]float64{}
		ts := Tokens(d.Text)
		for _, t := range ts {
			tf[t]++
		}
		for t := range tf {
			b.df[t]++
			b.posting[t] = append(b.posting[t], i)
		}
		b.ids = append(b.ids, d.ID)
		b.tf = append(b.tf, tf)
		b.dl = append(b.dl, float64(len(ts)))
		tot += float64(len(ts))
	}
	if len(docs) > 0 {
		b.avgdl = tot / float64(len(docs))
	}
	return b
}

func (b *BM25) Name() string { return "bm25" }

func (b *BM25) Rank(q string, k int) []Hit {
	n := float64(len(b.ids))
	sc := map[int]float64{}
	seen := map[string]bool{}
	for _, t := range Tokens(q) {
		if seen[t] {
			continue
		}
		seen[t] = true
		df := b.df[t]
		if df == 0 {
			continue
		}
		idf := math.Log(1 + (n-df+0.5)/(df+0.5))
		for _, i := range b.posting[t] {
			f := b.tf[i][t]
			sc[i] += idf * f * (b.K1 + 1) / (f + b.K1*(1-b.B+b.B*b.dl[i]/b.avgdl))
		}
	}
	return top(sc, b.ids, k)
}

func top(sc map[int]float64, ids []string, k int) []Hit {
	hs := make([]Hit, 0, len(sc))
	for i, s := range sc {
		hs = append(hs, Hit{ids[i], s})
	}
	sort.Slice(hs, func(i, j int) bool {
		if hs[i].Score != hs[j].Score {
			return hs[i].Score > hs[j].Score
		}
		return hs[i].ID < hs[j].ID
	})
	if k > 0 && len(hs) > k {
		hs = hs[:k]
	}
	return hs
}

// ---- character trigram TF-IDF (cosine)

type Trigram struct {
	ids     []string
	vec     []map[string]float64
	idf     map[string]float64
	posting map[string][]int
}

func grams(s string) map[string]float64 {
	m := map[string]float64{}
	for _, w := range Tokens(s) {
		w = "#" + w + "#"
		r := []rune(w)
		for i := 0; i+3 <= len(r); i++ {
			m[string(r[i:i+3])]++
		}
	}
	return m
}

func NewTrigram(docs []Doc) *Trigram {
	t := &Trigram{idf: map[string]float64{}, posting: map[string][]int{}}
	df := map[string]float64{}
	var raw []map[string]float64
	for i, d := range docs {
		g := grams(d.Text)
		raw = append(raw, g)
		for x := range g {
			df[x]++
			t.posting[x] = append(t.posting[x], i)
		}
		t.ids = append(t.ids, d.ID)
	}
	n := float64(len(docs))
	for x, v := range df {
		t.idf[x] = math.Log(1 + n/v)
	}
	for _, g := range raw {
		t.vec = append(t.vec, norm(g, t.idf))
	}
	return t
}

func norm(g map[string]float64, idf map[string]float64) map[string]float64 {
	var s float64
	out := make(map[string]float64, len(g))
	for x, f := range g {
		w := (1 + math.Log(f)) * idf[x]
		out[x] = w
		s += w * w
	}
	s = math.Sqrt(s)
	if s > 0 {
		for x := range out {
			out[x] /= s
		}
	}
	return out
}

func (t *Trigram) Name() string { return "trigram" }

func (t *Trigram) Rank(q string, k int) []Hit {
	qv := norm(grams(q), t.idf)
	sc := map[int]float64{}
	for x, w := range qv {
		for _, i := range t.posting[x] {
			sc[i] += w * t.vec[i][x]
		}
	}
	return top(sc, t.ids, k)
}

// ---- reciprocal rank fusion

type RRF struct {
	Parts []Ranker
	K     float64 // 60 in the original paper
	Depth int     // how deep to read each part
}

func (r RRF) Name() string {
	var n []string
	for _, p := range r.Parts {
		n = append(n, p.Name())
	}
	return "rrf(" + strings.Join(n, "+") + ")"
}

func (r RRF) Rank(q string, k int) []Hit {
	sc := map[string]float64{}
	for _, p := range r.Parts {
		for i, h := range p.Rank(q, r.Depth) {
			sc[h.ID] += 1 / (r.K + float64(i+1))
		}
	}
	hs := make([]Hit, 0, len(sc))
	for id, s := range sc {
		hs = append(hs, Hit{id, s})
	}
	sort.Slice(hs, func(i, j int) bool {
		if hs[i].Score != hs[j].Score {
			return hs[i].Score > hs[j].Score
		}
		return hs[i].ID < hs[j].ID
	})
	if k > 0 && len(hs) > k {
		hs = hs[:k]
	}
	return hs
}

func Hybrid(docs []Doc) Ranker {
	return RRF{Parts: []Ranker{NewBM25(docs), NewTrigram(docs)}, K: 60, Depth: 100}
}
