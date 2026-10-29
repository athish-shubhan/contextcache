package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"math"
	"math/rand/v2"
	"os"
	"sort"
	"strings"

	"contextcache/catalog"
	"contextcache/retrieve"
	"contextcache/tokens"
)

func prepare(args []string) error {
	fs := flag.NewFlagSet("prepare", flag.ExitOnError)
	dir := fs.String("bfcl", "data/bfcl", "BFCL data directory (bench/prepare.sh)")
	out := fs.String("out", "data/catalog.json", "catalog output")
	fs.Parse(args)
	c, err := catalog.LoadBFCL(*dir, []string{"simple", "multiple", "live_simple", "live_multiple", "irrelevance", "live_irrelevance"})
	if err != nil {
		// newer BFCL releases name the Python categories simple_python etc.
		c, err = catalog.LoadBFCL(*dir, []string{"simple_python", "multiple", "live_simple", "live_multiple", "irrelevance", "live_irrelevance"})
		if err != nil {
			return err
		}
	}
	tot := 0
	for i := range c.Tools {
		c.Tools[i].Tokens = tokens.Count(string(c.Tools[i].PromptJSON()))
		tot += c.Tools[i].Tokens
	}
	log.Printf("%d tools (%d tokens total, %.0f per tool, exact tokenizer %v), %d queries", len(c.Tools), tot,
		float64(tot)/float64(len(c.Tools)), tokens.Exact(), len(c.Queries))
	by := map[string]int{}
	for _, q := range c.Queries {
		by[q.Category]++
	}
	log.Printf("queries by category: %v", by)
	return c.Save(*out)
}

type overloadRow struct {
	N             int                `json:"n_tools"`
	Ranker        string             `json:"ranker"`
	Queries       int                `json:"queries"`
	RecallAt      map[string]float64 `json:"recall_at"`
	MRR           float64            `json:"mrr"`
	TokensAll     float64            `json:"mean_tokens_all_tools"`
	TokensTopK    map[string]float64 `json:"mean_tokens_top_k"`
	MetaOverhead  int                `json:"meta_tool_tokens"`
	SearchResults float64            `json:"mean_tokens_search_result_k5"`
}

// overload is the RAG-MCP-style needle test: for each query, the correct
// tool plus N-1 random distractors from the catalog; rank all N; report
// whether the correct one is in the top k and how many prompt tokens each
// strategy puts in context.
func overload(args []string) error {
	fs := flag.NewFlagSet("overload", flag.ExitOnError)
	catPath := fs.String("catalog", "data/catalog.json", "catalog")
	out := fs.String("out", "results/overload.json", "output")
	maxQ := fs.Int("queries", 0, "queries to use (0 = all)")
	seed := fs.Uint64("seed", 1, "seed")
	glove := fs.String("glove", "", "GloVe vectors (.gz) for the dense and three-way hybrid rankers")
	fs.Parse(args)
	c, err := catalog.Load(*catPath)
	if err != nil {
		return err
	}
	var emb *retrieve.Embedder
	embCache := map[string][]float32{}
	if *glove != "" {
		emb, err = loadEmbedder(c, *glove)
		if err != nil {
			return err
		}
	}
	var qs []catalog.Query
	for _, q := range c.Queries {
		if q.Tool != "" {
			qs = append(qs, q)
		}
	}
	r := rand.New(rand.NewPCG(*seed, 1))
	r.Shuffle(len(qs), func(i, j int) { qs[i], qs[j] = qs[j], qs[i] })
	if *maxQ > 0 && len(qs) > *maxQ {
		qs = qs[:*maxQ]
	}
	meta := tokens.Count(metaToolsJSON)
	var rows []overloadRow
	ns := []int{10, 50, 100, 200, 500, 1000, len(c.Tools)}
	for _, n := range ns {
		if n > len(c.Tools) {
			continue
		}
		type acc struct {
			hit       map[int]int
			rr        float64
			tokTop    map[int]float64
			tokSearch float64
		}
		names := []string{"bm25", "trigram", "hybrid"}
		if emb != nil {
			names = append(names, "dense", "hybrid+dense")
		}
		A := map[string]*acc{}
		for _, nm := range names {
			A[nm] = &acc{hit: map[int]int{}, tokTop: map[int]float64{}}
		}
		var tokAll float64
		// a fixed distractor pool per query; at full N everything is in
		for qi, q := range qs {
			ids := pick(c, q.Tool, n, rand.New(rand.NewPCG(*seed, uint64(qi))))
			docs := make([]retrieve.Doc, len(ids))
			all := 0
			for i, id := range ids {
				t := c.Get(id)
				docs[i] = retrieve.Doc{ID: id, Text: t.RetrievalText()}
				all += t.Tokens
			}
			tokAll += float64(all)
			bm := retrieve.NewBM25(docs)
			tg := retrieve.NewTrigram(docs)
			rk := map[string]retrieve.Ranker{"bm25": bm, "trigram": tg, "hybrid": retrieve.RRF{Parts: []retrieve.Ranker{bm, tg}, K: 60, Depth: 100}}
			if emb != nil {
				dn := retrieve.NewDense(emb, docs, embCache)
				rk["dense"] = dn
				rk["hybrid+dense"] = retrieve.RRF{Parts: []retrieve.Ranker{bm, tg, dn}, K: 60, Depth: 100}
			}
			for _, nm := range names {
				hits := rk[nm].Rank(q.Text, 20)
				a := A[nm]
				pos := -1
				for i, h := range hits {
					if h.ID == q.Tool {
						pos = i
						break
					}
				}
				if pos >= 0 {
					a.rr += 1 / float64(pos+1)
				}
				for _, k := range []int{1, 3, 5, 10} {
					if pos >= 0 && pos < k {
						a.hit[k]++
					}
					tk := 0
					for i := 0; i < k && i < len(hits); i++ {
						tk += c.Get(hits[i].ID).Tokens
					}
					a.tokTop[k] += float64(tk)
				}
				// what search_tools returns for k=5: names + 160-char descriptions
				var sb strings.Builder
				for i := 0; i < 5 && i < len(hits); i++ {
					t := c.Get(hits[i].ID)
					d := t.Description
					if len(d) > 160 {
						d = d[:160]
					}
					fmt.Fprintf(&sb, `{"name":%q,"description":%q},`, t.Name, d)
				}
				a.tokSearch += float64(tokens.Count(sb.String()))
			}
		}
		nq := float64(len(qs))
		for _, nm := range names {
			a := A[nm]
			row := overloadRow{N: n, Ranker: nm, Queries: len(qs), RecallAt: map[string]float64{}, MRR: a.rr / nq, TokensAll: tokAll / nq,
				TokensTopK: map[string]float64{}, MetaOverhead: meta, SearchResults: a.tokSearch / nq}
			for _, k := range []int{1, 3, 5, 10} {
				row.RecallAt[fmt.Sprint(k)] = float64(a.hit[k]) / nq
				row.TokensTopK[fmt.Sprint(k)] = a.tokTop[k] / nq
			}
			rows = append(rows, row)
			log.Printf("N=%4d %-8s R@1 %.3f R@5 %.3f MRR %.3f tokens all %.0f top5 %.0f", n, nm, row.RecallAt["1"], row.RecallAt["5"], row.MRR, row.TokensAll, row.TokensTopK["5"])
		}
	}
	return writeJSON(*out, map[string]any{"tools": len(c.Tools), "queries": len(qs), "exact_tokenizer": tokens.Exact(), "rows": rows})
}

func pick(c *catalog.Catalog, correct string, n int, r *rand.Rand) []string {
	if n >= len(c.Tools) {
		ids := make([]string, len(c.Tools))
		for i, t := range c.Tools {
			ids[i] = t.ID
		}
		return ids
	}
	ids := []string{correct}
	seen := map[string]bool{correct: true}
	for len(ids) < n {
		t := c.Tools[r.IntN(len(c.Tools))]
		if !seen[t.ID] {
			seen[t.ID] = true
			ids = append(ids, t.ID)
		}
	}
	sort.Strings(ids)
	return ids
}

const metaToolsJSON = `[{"name":"search_tools","description":"Search the tool catalog. Returns tool names with one-line descriptions; load a tool before calling it.","input_schema":{"type":"object","properties":{"query":{"type":"string","description":"what you need to do"},"k":{"type":"integer","description":"number of results (default 5)"}},"required":["query"]}},{"name":"load_tool","description":"Load a tool's full schema into this session. Returns the schema; frequently used tools stay listed.","input_schema":{"type":"object","properties":{"name":{"type":"string"}},"required":["name"]}},{"name":"call_tool","description":"Call any catalog tool by name with its arguments.","input_schema":{"type":"object","properties":{"name":{"type":"string"},"arguments":{"type":"object"}},"required":["name"]}}]`

func writeJSON(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o644)
}

var _ = math.Inf

// loadEmbedder loads only the GloVe words the catalog and queries use and
// fits SIF's principal component on the tool texts.
func loadEmbedder(c *catalog.Catalog, path string) (*retrieve.Embedder, error) {
	var texts, qtexts []string
	for _, t := range c.Tools {
		texts = append(texts, t.RetrievalText())
	}
	for _, q := range c.Queries {
		qtexts = append(qtexts, q.Text)
	}
	e, err := retrieve.LoadGloVe(path, retrieve.Vocab(texts, qtexts))
	if err != nil {
		return nil, err
	}
	e.Fit(texts)
	return e, nil
}
