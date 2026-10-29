package main

import (
	"flag"
	"fmt"
	"log"
	"math"
	"math/rand/v2"
	"sort"

	"contextcache/admit"
	"contextcache/catalog"
	"contextcache/retrieve"
)

type cacheRow struct {
	Budget     int     `json:"budget_tokens"`
	Strategy   string  `json:"strategy"`
	HitRate    float64 `json:"needed_tool_in_context"`
	MeanTokens float64 `json:"mean_tool_tokens_in_context"`
	Turns      int     `json:"turns"`
}

// cacheBench replays a long agent session: each turn needs one tool (Zipf
// popularity over the tools that have queries, with popularity shifts) and
// comes with that tool's user query. A strategy decides which tool schemas
// are in context for the turn under a token budget; a hit means the needed
// tool was among them. Retrieval sees the query; caches see only history.
func cacheBench(args []string) error {
	fs := flag.NewFlagSet("cache", flag.ExitOnError)
	catPath := fs.String("catalog", "data/catalog.json", "catalog")
	out := fs.String("out", "results/cache.json", "output")
	turns := fs.Int("turns", 30000, "turns")
	zipf := fs.Float64("zipf", 1.0, "tool popularity skew")
	shift := fs.Int("shift-every", 5000, "turns between popularity shifts")
	seed := fs.Uint64("seed", 1, "seed")
	fs.Parse(args)
	c, err := catalog.Load(*catPath)
	if err != nil {
		return err
	}
	byTool := map[string][]catalog.Query{}
	for _, q := range c.Queries {
		if q.Tool != "" {
			byTool[q.Tool] = append(byTool[q.Tool], q)
		}
	}
	var tools []string
	for t := range byTool {
		tools = append(tools, t)
	}
	sort.Strings(tools)
	r := rand.New(rand.NewPCG(*seed, 2))
	r.Shuffle(len(tools), func(i, j int) { tools[i], tools[j] = tools[j], tools[i] })
	// popularity by rank
	w := make([]float64, len(tools))
	tot := 0.0
	for i := range w {
		w[i] = 1 / math.Pow(float64(i+1), *zipf)
		tot += w[i]
	}
	cum := make([]float64, len(w))
	acc := 0.0
	for i, x := range w {
		acc += x / tot
		cum[i] = acc
	}
	// the trace: (tool, query)
	type turn struct {
		tool string
		q    catalog.Query
	}
	trace := make([]turn, *turns)
	rank := append([]string(nil), tools...)
	for i := range trace {
		if i > 0 && i%*shift == 0 {
			// trending change: 10 of the top 50 swap with random tools
			for k := 0; k < 10; k++ {
				a, b := r.IntN(50), 50+r.IntN(len(rank)-50)
				rank[a], rank[b] = rank[b], rank[a]
			}
		}
		t := rank[sort.SearchFloat64s(cum, r.Float64())]
		qs := byTool[t]
		trace[i] = turn{t, qs[r.IntN(len(qs))]}
	}
	// retrieval over the whole catalog, cached per query
	docs := make([]retrieve.Doc, len(c.Tools))
	for i := range c.Tools {
		docs[i] = retrieve.Doc{ID: c.Tools[i].ID, Text: c.Tools[i].RetrievalText()}
	}
	rk := retrieve.Hybrid(docs)
	ranked := map[string][]retrieve.Hit{}
	hitsFor := func(q catalog.Query) []retrieve.Hit {
		if h, ok := ranked[q.ID]; ok {
			return h
		}
		h := rk.Rank(q.Text, 50)
		ranked[q.ID] = h
		return h
	}
	cost := func(id string) int { return c.Get(id).Tokens }
	fill := func(hits []retrieve.Hit, budget int, skip func(string) bool) (map[string]bool, int) {
		in, used := map[string]bool{}, 0
		for _, h := range hits {
			if skip != nil && skip(h.ID) {
				continue
			}
			t := cost(h.ID)
			if used+t > budget {
				continue // smaller tools further down may still fit
			}
			in[h.ID] = true
			used += t
		}
		return in, used
	}
	var rows []cacheRow
	for _, B := range []int{1000, 2000, 4000, 8000} {
		type strat struct {
			name  string
			cache admit.Cache
			share float64 // of the budget given to the cache; the rest to retrieval
		}
		strats := []strat{
			{"retrieval only", nil, 0},
			{"cache only: LRU", admit.NewLRU(B), 1},
			{"cache only: LFU", admit.NewLFU(B), 1},
			{"cache only: W-TinyLFU", admit.NewTinyLFU(B, len(c.Tools)), 1},
			{"retrieval + LRU (half each)", admit.NewLRU(B / 2), 0.5},
			{"retrieval + W-TinyLFU (half each)", admit.NewTinyLFU(B/2, len(c.Tools)), 0.5},
		}
		for _, s := range strats {
			hit := 0
			var toks float64
			for _, t := range trace {
				in := false
				used := 0
				if s.cache != nil {
					in = s.cache.Contains(t.tool)
					used = s.cache.Used()
				}
				if s.share < 1 {
					rb := B - int(float64(B)*s.share)
					skip := func(id string) bool { return s.cache != nil && s.cache.Contains(id) }
					got, u := fill(hitsFor(t.q), rb, skip)
					used += u
					in = in || got[t.tool]
				}
				if in {
					hit++
				}
				toks += float64(used)
				if s.cache != nil {
					s.cache.Access(t.tool, cost(t.tool)) // the agent used it this turn
				}
			}
			row := cacheRow{Budget: B, Strategy: s.name, HitRate: float64(hit) / float64(len(trace)), MeanTokens: toks / float64(len(trace)), Turns: len(trace)}
			rows = append(rows, row)
			log.Printf("B=%5d %-36s hit %.3f tokens %.0f", B, s.name, row.HitRate, row.MeanTokens)
		}
	}
	all := 0
	for _, t := range c.Tools {
		all += t.Tokens
	}
	return writeJSON(*out, map[string]any{"tools": len(c.Tools), "tools_with_queries": len(tools), "zipf": *zipf, "shift_every": *shift,
		"all_tools_tokens": all, "rows": rows, "note": fmt.Sprintf("%d turns", *turns)})
}
