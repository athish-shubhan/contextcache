package retrieve

import (
	"math"
	"reflect"
	"testing"
)

func TestTokens(t *testing.T) {
	got := Tokens("getWeatherForecast(city_name) for the US-East region")
	want := []string{"get", "weather", "forecast", "city", "name", "us", "east", "region"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("%v", got)
	}
}

func TestRankersFindTheRightTool(t *testing.T) {
	docs := []Doc{
		{"weather.get_forecast", "get_forecast: weather forecast for a city over the next days"},
		{"finance.convert_currency", "convert_currency: convert an amount between currencies using exchange rates"},
		{"maps.route", "route: driving directions between two places"},
		{"math.factorial", "factorial: compute n factorial"},
	}
	for _, r := range []Ranker{NewBM25(docs), NewTrigram(docs), Hybrid(docs)} {
		if h := r.Rank("how many euros is 300 dollars at today's exchange rate", 1); len(h) == 0 || h[0].ID != "finance.convert_currency" {
			t.Fatalf("%s: %v", r.Name(), h)
		}
		if h := r.Rank("what's the forecasted weather in Sydney", 1); len(h) == 0 || h[0].ID != "weather.get_forecast" {
			t.Fatalf("%s: %v", r.Name(), h)
		}
	}
	// trigrams survive word-form changes BM25 misses
	if h := NewTrigram(docs).Rank("factorials", 1); len(h) == 0 || h[0].ID != "math.factorial" {
		t.Fatalf("trigram: %v", h)
	}
}

func TestTokensEdgeCases(t *testing.T) {
	cases := map[string][]string{
		"":                         nil,
		"the a of":                 nil, // only stop words
		"HTTPServer":               {"httpserver"},
		"getUserID v2":             {"get", "user", "id", "v2"},
		"Café_Straße":              {"café", "straße"},
		"  find   flights!! now  ": {"find", "flights", "now"},
	}
	for in, want := range cases {
		if got := Tokens(in); !reflect.DeepEqual(got, want) {
			t.Errorf("Tokens(%q) = %v, want %v", in, got, want)
		}
	}
}

// BM25 with k1 = 1.2, b = 0.75 and idf = ln(1 + (N - df + 0.5)/(df + 0.5)),
// checked against a hand calculation on a three-document corpus.
func TestBM25ExactScores(t *testing.T) {
	docs := []Doc{{"d1", "weather weather city"}, {"d2", "weather"}, {"d3", "stock price"}}
	b := NewBM25(docs)
	hits := b.Rank("weather", 0)
	if len(hits) != 2 {
		t.Fatalf("hits %v", hits)
	}
	// N = 3, df(weather) = 2, avgdl = (3 + 1 + 2) / 3 = 2
	idf := math.Log(1 + (3-2+0.5)/(2+0.5))
	want := map[string]float64{
		"d1": idf * 2 * 2.2 / (2 + 1.2*(1-0.75+0.75*3.0/2)),
		"d2": idf * 1 * 2.2 / (1 + 1.2*(1-0.75+0.75*1.0/2)),
	}
	for _, h := range hits {
		if math.Abs(h.Score-want[h.ID]) > 1e-12 {
			t.Fatalf("%s: score %v, want %v", h.ID, h.Score, want[h.ID])
		}
	}
	// length normalisation: the one-word document beats the longer one
	// despite half the term frequency (0.591 vs 0.567)
	if hits[0].ID != "d2" {
		t.Fatalf("order %v", hits)
	}
	// repeated query terms count once
	if again := b.Rank("weather weather weather", 0); again[0].Score != hits[0].Score {
		t.Fatalf("duplicate query terms changed the score: %v vs %v", again, hits)
	}
	if h := b.Rank("unknownword", 5); len(h) != 0 {
		t.Fatalf("no shared term should give no hits: %v", h)
	}
}

func TestTrigramCosine(t *testing.T) {
	docs := []Doc{{"a", "convert currency"}, {"b", "currency exchange rates"}, {"c", "driving directions"}}
	tr := NewTrigram(docs)
	h := tr.Rank("convert currency", 0)
	if h[0].ID != "a" || math.Abs(h[0].Score-1) > 1e-9 {
		t.Fatalf("a query equal to a document has cosine 1 with it: %v", h)
	}
	for _, x := range h {
		if x.Score <= 0 || x.Score > 1+1e-9 {
			t.Fatalf("cosine out of (0, 1]: %v", h)
		}
	}
	for _, x := range h {
		if x.ID == "c" {
			t.Fatalf("no shared trigram, should not be returned: %v", h)
		}
	}
}

type fixed struct {
	name string
	ids  []string
}

func (f fixed) Name() string { return f.name }
func (f fixed) Rank(_ string, k int) []Hit {
	var h []Hit
	for i, id := range f.ids {
		if k > 0 && i == k {
			break
		}
		h = append(h, Hit{id, float64(len(f.ids) - i)})
	}
	return h
}

func TestRRF(t *testing.T) {
	r := RRF{Parts: []Ranker{fixed{"p", []string{"x", "y", "z"}}, fixed{"q", []string{"y", "w"}}}, K: 60, Depth: 10}
	if r.Name() != "rrf(p+q)" {
		t.Fatal(r.Name())
	}
	h := r.Rank("", 0)
	want := map[string]float64{"x": 1.0 / 61, "y": 1.0/62 + 1.0/61, "z": 1.0 / 63, "w": 1.0 / 62}
	if len(h) != 4 || h[0].ID != "y" {
		t.Fatalf("y is in both lists and should lead: %v", h)
	}
	for _, x := range h {
		if math.Abs(x.Score-want[x.ID]) > 1e-15 {
			t.Fatalf("%s: %v want %v", x.ID, x.Score, want[x.ID])
		}
	}
	// equal scores are ordered by ID so results are deterministic
	tie := RRF{Parts: []Ranker{fixed{"p", []string{"b"}}, fixed{"q", []string{"a"}}}, K: 60, Depth: 10}.Rank("", 0)
	if tie[0].ID != "a" || tie[1].ID != "b" {
		t.Fatalf("tie order %v", tie)
	}
	if got := r.Rank("", 2); len(got) != 2 {
		t.Fatalf("k = 2 returned %d", len(got))
	}
	// Depth limits how far each part is read
	shallow := RRF{Parts: r.Parts, K: 60, Depth: 1}.Rank("", 0)
	if len(shallow) != 2 {
		t.Fatalf("depth 1 should see only each part's first hit: %v", shallow)
	}
}

func TestEmptyIndex(t *testing.T) {
	for _, r := range []Ranker{NewBM25(nil), NewTrigram(nil), Hybrid(nil)} {
		if h := r.Rank("anything", 5); len(h) != 0 {
			t.Fatalf("%s on an empty index: %v", r.Name(), h)
		}
	}
}
