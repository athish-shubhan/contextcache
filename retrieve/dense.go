package retrieve

// Dense retrieval with static word vectors (GloVe, 100-d, Wikipedia +
// Gigaword) and smooth inverse frequency weighting (Arora, Liang and Ma,
// "A simple but tough-to-beat baseline for sentence embeddings", ICLR 2017):
// a text is the weighted mean of its words' vectors, weight a / (a + p(w)),
// with the corpus' first principal component removed. No neural encoder was
// available in the build environment; this is the strongest dense baseline
// that needs only a vector file, and it plugs into the same Ranker
// interface (and into pgvector, see PGVector) as a modern embedding model
// would.

import (
	"bufio"
	"compress/gzip"
	"fmt"
	"io"
	"math"
	"os"
	"sort"
	"strconv"
	"strings"
)

type Embedder struct {
	Dim  int
	vec  map[string][]float32
	rank map[string]int
	nv   int
	a    float64
	pc   []float32 // first principal component of the fitted corpus (nil = none)
}

// LoadGloVe reads a GloVe text file (optionally gzipped, optionally with a
// "count dim" header line). Only words in vocab are kept when vocab is not
// nil; ranks are file positions (GloVe files are sorted by frequency).
func LoadGloVe(path string, vocab map[string]bool) (*Embedder, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var r io.Reader = f
	if strings.HasSuffix(path, ".gz") {
		gz, err := gzip.NewReader(f)
		if err != nil {
			return nil, err
		}
		defer gz.Close()
		r = gz
	}
	e := &Embedder{vec: map[string][]float32{}, rank: map[string]int{}, a: 1e-3}
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	line := 0
	for sc.Scan() {
		s := sc.Text()
		sp := strings.IndexByte(s, ' ')
		if sp < 0 {
			continue
		}
		w := s[:sp]
		if line == 0 {
			if _, err := strconv.Atoi(w); err == nil { // header
				line++
				continue
			}
		}
		line++
		e.nv++
		if vocab != nil && !vocab[w] {
			continue
		}
		fs := strings.Fields(s[sp+1:])
		if e.Dim == 0 {
			e.Dim = len(fs)
		}
		if len(fs) != e.Dim {
			continue
		}
		v := make([]float32, e.Dim)
		for i, x := range fs {
			f, _ := strconv.ParseFloat(x, 32)
			v[i] = float32(f)
		}
		e.vec[w] = v
		e.rank[w] = e.nv
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	if e.Dim == 0 {
		return nil, fmt.Errorf("%s: no vectors", path)
	}
	return e, nil
}

// Vocab collects the tokens of some texts (to load only what is needed).
func Vocab(texts ...[]string) map[string]bool {
	v := map[string]bool{}
	for _, ts := range texts {
		for _, t := range ts {
			for _, w := range Tokens(t) {
				v[w] = true
			}
		}
	}
	return v
}

// weight is SIF's a / (a + p(w)), with p(w) from the Zipf law on the
// word's frequency rank.
func (e *Embedder) weight(w string) float64 {
	r := e.rank[w]
	if r == 0 {
		return 1
	}
	p := 1 / (float64(r) * math.Log(float64(e.nv)))
	return e.a / (e.a + p)
}

// raw is the SIF-weighted mean, before principal component removal.
func (e *Embedder) raw(text string) ([]float32, bool) {
	out := make([]float32, e.Dim)
	var tw float64
	for _, w := range Tokens(text) {
		v, ok := e.vec[w]
		if !ok {
			continue
		}
		wt := e.weight(w)
		for i := range out {
			out[i] += float32(wt) * v[i]
		}
		tw += wt
	}
	if tw == 0 {
		return out, false
	}
	for i := range out {
		out[i] /= float32(tw)
	}
	return out, true
}

// Fit estimates the first principal component of a corpus (power
// iteration on the uncentered second-moment matrix, as in SIF).
func (e *Embedder) Fit(texts []string) {
	var X [][]float32
	for _, t := range texts {
		if v, ok := e.raw(t); ok {
			X = append(X, v)
		}
	}
	if len(X) == 0 {
		return
	}
	u := make([]float64, e.Dim)
	for i := range u {
		u[i] = 1 / math.Sqrt(float64(e.Dim))
	}
	for it := 0; it < 50; it++ {
		nu := make([]float64, e.Dim)
		for _, x := range X {
			var d float64
			for i := range x {
				d += float64(x[i]) * u[i]
			}
			for i := range x {
				nu[i] += d * float64(x[i])
			}
		}
		var n float64
		for _, v := range nu {
			n += v * v
		}
		n = math.Sqrt(n)
		for i := range nu {
			nu[i] /= n
		}
		u = nu
	}
	e.pc = make([]float32, e.Dim)
	for i := range u {
		e.pc[i] = float32(u[i])
	}
}

// Embed returns a unit vector (zero vector when no word is known).
func (e *Embedder) Embed(text string) []float32 {
	v, _ := e.raw(text)
	if e.pc != nil {
		var d float32
		for i := range v {
			d += v[i] * e.pc[i]
		}
		for i := range v {
			v[i] -= d * e.pc[i]
		}
	}
	var n float64
	for _, x := range v {
		n += float64(x) * float64(x)
	}
	if n > 0 {
		s := float32(1 / math.Sqrt(n))
		for i := range v {
			v[i] *= s
		}
	}
	return v
}

func dot(a, b []float32) float64 {
	var s float32
	for i := range a {
		s += a[i] * b[i]
	}
	return float64(s)
}

// Dense ranks documents by cosine similarity, brute force.
type Dense struct {
	e    *Embedder
	ids  []string
	vecs [][]float32
}

// NewDense embeds docs; cache (may be nil) reuses embeddings by doc ID.
func NewDense(e *Embedder, docs []Doc, cache map[string][]float32) *Dense {
	d := &Dense{e: e}
	for _, doc := range docs {
		v, ok := cache[doc.ID]
		if !ok {
			v = e.Embed(doc.Text)
			if cache != nil {
				cache[doc.ID] = v
			}
		}
		d.ids = append(d.ids, doc.ID)
		d.vecs = append(d.vecs, v)
	}
	return d
}

func (d *Dense) Name() string { return "dense" }

func (d *Dense) Rank(q string, k int) []Hit {
	qv := d.e.Embed(q)
	hs := make([]Hit, len(d.ids))
	for i, v := range d.vecs {
		hs[i] = Hit{d.ids[i], dot(qv, v)}
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

// HybridDense fuses BM25, trigrams and dense vectors.
func HybridDense(docs []Doc, e *Embedder, cache map[string][]float32) Ranker {
	return RRF{Parts: []Ranker{NewBM25(docs), NewTrigram(docs), NewDense(e, docs, cache)}, K: 60, Depth: 100}
}
