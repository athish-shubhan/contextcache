// Command memrevise generates the MemRevise dataset and scores memory
// stores on it.
//
//	memrevise -out results/memrevise.json -data data/memrevise [-dsn postgres://...]
package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"flag"
	"log"
	"math"
	"os"
	"path/filepath"
	"sort"

	"contextcache/memory"
	"contextcache/retrieve"
	"contextcache/tokens"
)

func pct(x []float64, p float64) float64 {
	if len(x) == 0 {
		return math.NaN()
	}
	s := append([]float64(nil), x...)
	sort.Float64s(s)
	return s[min(len(s)-1, int(math.Ceil(p/100*float64(len(s))))-1)]
}

func main() {
	out := flag.String("out", "results/memrevise.json", "scores")
	data := flag.String("data", "data/memrevise", "write the dataset here (events.jsonl, queries.jsonl)")
	dsn := flag.String("dsn", "", "Postgres DSN for the SQL bi-temporal store (empty = skip)")
	users := flag.Int("users", 20, "users")
	evs := flag.Int("events", 3000, "events per user")
	qs := flag.Int("queries", 100, "queries per user")
	seed := flag.Uint64("seed", 2026, "seed")
	glove := flag.String("glove", "", "GloVe vectors (.gz): add dense similarity baselines")
	flag.Parse()
	ds := memory.Generate(*seed, *users, *evs, *qs)
	os.MkdirAll(*data, 0o755)
	writeLines(filepath.Join(*data, "events.jsonl"), ds.Events)
	writeLines(filepath.Join(*data, "queries.jsonl"), ds.Queries)

	// how long is one user's history, in tokens?
	perUser := map[string]int{}
	for _, e := range ds.Events {
		perUser[e.User] += tokens.Count(e.Text) + 8 // + a date stamp per line
	}
	var toks []float64
	for _, v := range perUser {
		toks = append(toks, float64(v))
	}
	kinds := map[memory.QKind]int{}
	for _, q := range ds.Queries {
		kinds[q.Kind]++
	}
	ops := map[memory.Op]int{}
	for _, e := range ds.Events {
		ops[e.Op]++
	}
	log.Printf("%d events, %d queries %v, ops %v, history tokens per user median %.0f", len(ds.Events), len(ds.Queries), kinds, ops, pct(toks, 50))

	stores := []memory.Store{memory.NewBiTemporal(), memory.NewKVLatest(), memory.NewAppendLog(), memory.NewSimilarity(false), memory.NewSimilarity(true)}
	if *glove != "" {
		var texts, qtexts []string
		for _, e := range ds.Events {
			texts = append(texts, e.Text)
		}
		for _, q := range ds.Queries {
			qtexts = append(qtexts, q.Text)
		}
		emb, err := retrieve.LoadGloVe(*glove, retrieve.Vocab(texts, qtexts))
		if err != nil {
			log.Fatal(err)
		}
		emb.Fit(texts)
		stores = append(stores, memory.NewSimilarityDense(emb, false), memory.NewSimilarityDense(emb, true))
	}
	var pg *memory.PGBiTemporal
	if *dsn != "" {
		db, err := sql.Open("postgres", *dsn)
		if err != nil {
			log.Fatal(err)
		}
		if pg, err = memory.NewPGBiTemporal(context.Background(), db); err != nil {
			log.Fatal(err)
		}
		stores = append(stores, pg)
	}
	var scores []memory.Score
	for _, s := range stores {
		sc, err := memory.Run(ds, s)
		if err != nil {
			log.Fatalf("%s: %v", s.Name(), err)
		}
		log.Printf("%-36s overall %.3f %v", sc.Store, sc.Overall, sc.ByKind)
		scores = append(scores, sc)
	}
	res := map[string]any{"seed": *seed, "users": *users, "events_per_user": *evs, "queries": len(ds.Queries), "queries_by_kind": kinds,
		"events_by_op": ops, "history_tokens_per_user_median": pct(toks, 50), "exact_tokenizer": tokens.Exact(), "scores": scores}
	if pg != nil {
		res["postgres_latency_ms"] = map[string]float64{"write_p50": pct(pg.WriteMs, 50), "write_p99": pct(pg.WriteMs, 99),
			"read_p50": pct(pg.ReadMs, 50), "read_p99": pct(pg.ReadMs, 99), "writes": float64(len(pg.WriteMs)), "reads": float64(len(pg.ReadMs))}
	}
	b, _ := json.MarshalIndent(res, "", "  ")
	if err := os.WriteFile(*out, b, 0o644); err != nil {
		log.Fatal(err)
	}
}

func writeLines[T any](path string, xs []T) {
	f, err := os.Create(path)
	if err != nil {
		log.Fatal(err)
	}
	defer f.Close()
	enc := json.NewEncoder(f)
	for _, x := range xs {
		enc.Encode(x)
	}
}
