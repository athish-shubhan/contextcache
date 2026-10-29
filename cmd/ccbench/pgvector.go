package main

import (
	"context"
	"database/sql"
	"flag"
	"log"
	"sort"
	"time"

	_ "github.com/lib/pq"

	"contextcache/catalog"
	"contextcache/retrieve"
)

// pgvectorBench: the whole catalog in Postgres with an HNSW index. Reports
// how often the approximate top 10 matches exact cosine search, recall@5 of
// the correct tool for both, and query latency.
func pgvectorBench(args []string) error {
	fs := flag.NewFlagSet("pgvector", flag.ExitOnError)
	catPath := fs.String("catalog", "data/catalog.json", "catalog")
	glove := fs.String("glove", "", "GloVe vectors (.gz)")
	dsn := fs.String("dsn", "postgres://postgres:postgres@127.0.0.1:5432/contextcache?sslmode=disable", "Postgres with pgvector")
	out := fs.String("out", "results/pgvector.json", "output")
	fs.Parse(args)
	c, err := catalog.Load(*catPath)
	if err != nil {
		return err
	}
	emb, err := loadEmbedder(c, *glove)
	if err != nil {
		return err
	}
	db, err := sql.Open("postgres", *dsn)
	if err != nil {
		return err
	}
	defer db.Close()
	ctx := context.Background()
	p := &retrieve.PGVector{DB: db, Table: "tool_vectors", Dim: emb.Dim}
	if err := p.Reset(ctx); err != nil {
		return err
	}
	var ids []string
	var vecs [][]float32
	var docs []retrieve.Doc
	for _, t := range c.Tools {
		ids = append(ids, t.ID)
		vecs = append(vecs, emb.Embed(t.RetrievalText()))
		docs = append(docs, retrieve.Doc{ID: t.ID, Text: t.RetrievalText()})
	}
	t0 := time.Now()
	if err := p.Insert(ctx, ids, vecs); err != nil {
		return err
	}
	loadSec := time.Since(t0).Seconds()
	t0 = time.Now()
	if err := p.Index(ctx); err != nil {
		return err
	}
	indexSec := time.Since(t0).Seconds()
	exact := retrieve.NewDense(emb, docs, nil)
	var latPG, latMem []float64
	var overlap, hitPG, hitMem, n float64
	for _, q := range c.Queries {
		if q.Tool == "" {
			continue
		}
		n++
		qv := emb.Embed(q.Text)
		t := time.Now()
		hp, err := p.Search(ctx, qv, 10)
		if err != nil {
			return err
		}
		latPG = append(latPG, float64(time.Since(t).Microseconds())/1000)
		t = time.Now()
		he := exact.Rank(q.Text, 10)
		latMem = append(latMem, float64(time.Since(t).Microseconds())/1000)
		set := map[string]bool{}
		for _, h := range he {
			set[h.ID] = true
		}
		for _, h := range hp {
			if set[h.ID] {
				overlap++
			}
		}
		for i := 0; i < 5 && i < len(hp); i++ {
			if hp[i].ID == q.Tool {
				hitPG++
			}
		}
		for i := 0; i < 5 && i < len(he); i++ {
			if he[i].ID == q.Tool {
				hitMem++
			}
		}
	}
	q := func(x []float64, p float64) float64 {
		s := append([]float64(nil), x...)
		sort.Float64s(s)
		return s[int(p*float64(len(s)-1))]
	}
	res := map[string]any{"tools": len(ids), "queries": n, "dim": emb.Dim, "load_sec": loadSec, "hnsw_build_sec": indexSec,
		"ann_top10_overlap_with_exact": overlap / (10 * n), "recall5_pgvector": hitPG / n, "recall5_exact": hitMem / n,
		"pgvector_ms":        map[string]float64{"p50": q(latPG, 0.5), "p99": q(latPG, 0.99)},
		"in_memory_exact_ms": map[string]float64{"p50": q(latMem, 0.5), "p99": q(latMem, 0.99)}}
	log.Printf("pgvector: overlap %.3f, R@5 %.3f (exact %.3f), p50 %.2f ms", overlap/(10*n), hitPG/n, hitMem/n, q(latPG, 0.5))
	return writeJSON(*out, res)
}
