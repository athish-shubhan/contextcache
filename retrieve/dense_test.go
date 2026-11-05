package retrieve

import (
	"compress/gzip"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A 4-d toy vocabulary: weather words point one way, money words another.
const toyGloVe = `9 4
the 0.1 0.1 0.1 0.1
weather 1 0 0 0.1
forecast 0.9 0.1 0 0.1
rain 0.8 0.2 0 0.1
city 0.3 0.3 0.3 0.1
stock 0 0 1 0.1
price 0 0.1 0.9 0.1
money 0 0 0.8 0.2
convert 0.1 0 0.7 0.1
`

func writeGloVe(t *testing.T, body string, gz bool) string {
	t.Helper()
	dir := t.TempDir()
	if !gz {
		p := filepath.Join(dir, "g.txt")
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	p := filepath.Join(dir, "g.gz")
	f, err := os.Create(p)
	if err != nil {
		t.Fatal(err)
	}
	w := gzip.NewWriter(f)
	_, _ = w.Write([]byte(body))
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoadGloVe(t *testing.T) {
	e, err := LoadGloVe(writeGloVe(t, toyGloVe, true), nil)
	if err != nil {
		t.Fatal(err)
	}
	if e.Dim != 4 || len(e.vec) != 9 || e.nv != 9 {
		t.Fatalf("dim %d, %d vectors, nv %d", e.Dim, len(e.vec), e.nv)
	}
	if e.rank["the"] != 1 || e.rank["convert"] != 9 {
		t.Fatalf("ranks are file positions after the header: %v", e.rank)
	}
	// vocabulary filtering keeps ranks from the full file
	f, err := LoadGloVe(writeGloVe(t, toyGloVe, false), map[string]bool{"stock": true})
	if err != nil || len(f.vec) != 1 || f.rank["stock"] != 6 || f.nv != 9 {
		t.Fatalf("filtered: %v %v", f, err)
	}
	if _, err := LoadGloVe(writeGloVe(t, "word 0.1 x 0.3\n", false), nil); err == nil || !strings.Contains(err.Error(), "line 1") {
		t.Fatalf("a malformed number must be an error naming the line: %v", err)
	}
	if _, err := LoadGloVe(writeGloVe(t, "\n\n", false), nil); err == nil {
		t.Fatal("a file without vectors must be an error")
	}
}

func TestDenseRanking(t *testing.T) {
	e, err := LoadGloVe(writeGloVe(t, toyGloVe, false), nil)
	if err != nil {
		t.Fatal(err)
	}
	docs := []Doc{
		{"weather.forecast", "weather forecast rain"},
		{"finance.convert", "convert money stock price"},
		{"empty", "zzz unknownword"}, // no known word: zero vector
	}
	e.Fit([]string{docs[0].Text, docs[1].Text})
	d := NewDense(e, docs, nil)
	h := d.Rank("rain in the city", 0)
	if len(h) != 2 || h[0].ID != "weather.forecast" {
		t.Fatalf("hits %v", h)
	}
	for _, x := range h {
		if x.ID == "empty" {
			t.Fatalf("a document with no known word must not be returned: %v", h)
		}
	}
	if h := d.Rank("stock price", 1); len(h) != 1 || h[0].ID != "finance.convert" {
		t.Fatalf("hits %v", h)
	}
	// no known word in the query: no information, no hits (it used to return
	// every document with score 0 in ID order)
	if h := d.Rank("qwerty asdf", 0); len(h) != 0 {
		t.Fatalf("unknown-word query returned %v", h)
	}
	// embeddings are unit length, with the fitted direction removed
	v := e.Embed("weather forecast")
	var n, pc float64
	for i := range v {
		n += float64(v[i]) * float64(v[i])
		pc += float64(v[i]) * float64(e.pc[i])
	}
	if math.Abs(n-1) > 1e-5 || math.Abs(pc) > 1e-5 {
		t.Fatalf("norm^2 %v, component along pc %v", n, pc)
	}
	// the cache is reused by ID
	cache := map[string][]float32{}
	NewDense(e, docs, cache)
	if len(cache) != 3 {
		t.Fatalf("cache %d entries", len(cache))
	}
}

func TestFitDegenerate(t *testing.T) {
	// one known word whose vector is orthogonal to the power iteration's
	// starting direction: the second moment times u is zero
	// (two words: with one, its SIF weight is 0 and Fit has nothing to fit)
	e, err := LoadGloVe(writeGloVe(t, "bb 0 0\nxy 1 -1\n", false), nil)
	if err != nil {
		t.Fatal(err)
	}
	e.Fit([]string{"xy"})
	for _, x := range e.Embed("xy") {
		if math.IsNaN(float64(x)) {
			t.Fatal("Fit produced NaN embeddings")
		}
	}
}

func TestHybridDense(t *testing.T) {
	e, _ := LoadGloVe(writeGloVe(t, toyGloVe, false), nil)
	docs := []Doc{{"weather.forecast", "weather forecast rain"}, {"finance.convert", "convert money stock price"}}
	r := HybridDense(docs, e, nil)
	if h := r.Rank("forecast for rain", 1); len(h) != 1 || h[0].ID != "weather.forecast" {
		t.Fatalf("%s: %v", r.Name(), h)
	}
}
