package main

import (
	"flag"
	"log"
	"math/rand/v2"
	"sort"

	"contextcache/catalog"
	"contextcache/retrieve"
	"contextcache/tokens"
)

type skillRow struct {
	N            int     `json:"n_skills"`
	Threshold    float64 `json:"threshold"`
	Precision    float64 `json:"precision"`
	Recall       float64 `json:"recall"`
	FalseTrigger float64 `json:"false_trigger_rate_irrelevant"`
	Relevant     int     `json:"relevant_queries"`
	Irrelevant   int     `json:"irrelevant_queries"`
	Tier1Tokens  float64 `json:"tier1_tokens_all_descriptions"`
}

// skills measures description-based skill triggering as the skill count
// grows. Agent Skills put every skill's name and description (tier 1) in
// context and load a body only when the task matches. Here each catalog tool
// plays a skill (its name and description are the tier-1 text), relevant
// queries have exactly one matching skill, and BFCL irrelevance queries have
// none: a trigger on them is a false positive. The trigger is retrieval: load
// the top skill if its trigram cosine similarity clears a threshold chosen on
// a training half of the queries at N = 100 and kept fixed for every N.
func skills(args []string) error {
	fs := flag.NewFlagSet("skills", flag.ExitOnError)
	catPath := fs.String("catalog", "data/catalog.json", "catalog")
	out := fs.String("out", "results/skills.json", "output")
	seed := fs.Uint64("seed", 3, "seed")
	fs.Parse(args)
	c, err := catalog.Load(*catPath)
	if err != nil {
		return err
	}
	qs := append([]catalog.Query(nil), c.Queries...)
	r := rand.New(rand.NewPCG(*seed, 1))
	r.Shuffle(len(qs), func(i, j int) { qs[i], qs[j] = qs[j], qs[i] })
	train, test := qs[:len(qs)/2], qs[len(qs)/2:]
	tier1 := map[string]int{}
	for _, t := range c.Tools {
		tier1[t.ID] = tokens.Count(t.Name + ": " + t.Description)
	}
	type res struct {
		correct string
		top     string
		score   float64
		tier1   int
	}
	run := func(set []catalog.Query, n int) []res {
		out := make([]res, len(set))
		for i, q := range set {
			rr := rand.New(rand.NewPCG(*seed, uint64(i)+uint64(n)<<32))
			var ids []string
			if q.Tool != "" {
				ids = pick(c, q.Tool, n, rr)
			} else {
				ids = pick(c, c.Tools[rr.IntN(len(c.Tools))].ID, n, rr)
			}
			docs := make([]retrieve.Doc, len(ids))
			t1 := 0
			for k, id := range ids {
				t := c.Get(id)
				docs[k] = retrieve.Doc{ID: id, Text: t.Name + " " + t.Description}
				t1 += tier1[id]
			}
			hits := retrieve.NewTrigram(docs).Rank(q.Text, 1)
			out[i] = res{correct: q.Tool, tier1: t1}
			if len(hits) > 0 {
				out[i].top, out[i].score = hits[0].ID, hits[0].Score
			}
		}
		return out
	}
	score := func(rs []res, th float64) (p, rec, fp float64, nrel, nirr int) {
		var trig, good, irrTrig int
		for _, x := range rs {
			fire := x.score >= th
			if x.correct == "" {
				nirr++
				if fire {
					irrTrig++
					trig++
				}
				continue
			}
			nrel++
			if fire {
				trig++
				if x.top == x.correct {
					good++
				}
			}
		}
		if trig > 0 {
			p = float64(good) / float64(trig)
		}
		if nrel > 0 {
			rec = float64(good) / float64(nrel)
		}
		if nirr > 0 {
			fp = float64(irrTrig) / float64(nirr)
		}
		return
	}
	// choose the threshold that maximises F1 on the training half at N = 100
	tr := run(train, 100)
	var cands []float64
	for _, x := range tr {
		cands = append(cands, x.score)
	}
	sort.Float64s(cands)
	best, bestF := 0.0, -1.0
	for _, th := range cands {
		p, rec, _, _, _ := score(tr, th)
		if p+rec > 0 {
			if f := 2 * p * rec / (p + rec); f > bestF {
				best, bestF = th, f
			}
		}
	}
	log.Printf("threshold %.3f (train F1 %.3f at N=100)", best, bestF)
	var rows []skillRow
	for _, n := range []int{10, 50, 100, 200, 500, 1000, len(c.Tools)} {
		if n > len(c.Tools) {
			continue
		}
		rs := run(test, n)
		p, rec, fp, nrel, nirr := score(rs, best)
		var t1 float64
		for _, x := range rs {
			t1 += float64(x.tier1)
		}
		row := skillRow{N: n, Threshold: best, Precision: p, Recall: rec, FalseTrigger: fp, Relevant: nrel, Irrelevant: nirr, Tier1Tokens: t1 / float64(len(rs))}
		rows = append(rows, row)
		log.Printf("N=%4d precision %.3f recall %.3f false-trigger %.3f tier-1 tokens %.0f", n, p, rec, fp, row.Tier1Tokens)
	}
	return writeJSON(*out, map[string]any{"rows": rows, "train_f1_at_100": bestF})
}
