package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"math"
	"sort"
	"strings"
	"sync"
	"time"

	"contextcache/catalog"
	"contextcache/mcp"
	"contextcache/tokens"
)

func pctl(x []float64, p float64) float64 {
	if len(x) == 0 {
		return math.NaN()
	}
	s := append([]float64(nil), x...)
	sort.Float64s(s)
	return s[min(len(s)-1, int(math.Ceil(p/100*float64(len(s))))-1)]
}

type latRow struct {
	Path    string  `json:"path"`
	Op      string  `json:"op"`
	Conc    int     `json:"concurrency"`
	N       int     `json:"calls"`
	P50us   float64 `json:"p50_us"`
	P99us   float64 `json:"p99_us"`
	Tools   int     `json:"tools_behind"`
	ListTok int     `json:"tools_list_tokens,omitempty"`
}

func timeCalls(c *mcp.Client, n, conc int, f func(i int) error) ([]float64, error) {
	var mu sync.Mutex
	var lat []float64
	var wg sync.WaitGroup
	var first error
	per := n / conc
	for w := 0; w < conc; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < per; i++ {
				t := time.Now()
				err := f(w*per + i)
				d := float64(time.Since(t).Microseconds())
				mu.Lock()
				if err != nil && first == nil {
					first = err
				}
				lat = append(lat, d)
				mu.Unlock()
			}
		}(w)
	}
	wg.Wait()
	return lat, first
}

// latency measures what the gateway adds: the same tools/call straight to
// one upstream toolfarm, and through the gateway in front of 10 toolfarms.
func latency(args []string) error {
	fs := flag.NewFlagSet("latency", flag.ExitOnError)
	catPath := fs.String("catalog", "data/catalog.json", "catalog")
	out := fs.String("out", "results/latency.json", "output")
	n := fs.Int("calls", 4000, "calls per measurement")
	farms := fs.Int("farms", 10, "upstream servers behind the gateway")
	fs.Parse(args)
	c, err := catalog.Load(*catPath)
	if err != nil {
		return err
	}
	ctx := context.Background()
	var rows []latRow
	listTokens := func(cl *mcp.Client) (int, int, error) {
		ts, err := cl.ListTools(ctx)
		if err != nil {
			return 0, 0, err
		}
		b, _ := json.Marshal(ts)
		return len(ts), tokens.Count(string(b)), nil
	}
	of := fmt.Sprint(*farms)
	// direct to toolfarm 0
	direct, err := mcp.StartStdio("bin/toolfarm", "-catalog", *catPath, "-server", "0", "-of", of)
	if err != nil {
		return err
	}
	if err := direct.Initialize(ctx, "ccbench"); err != nil {
		return err
	}
	dts, err := direct.ListTools(ctx)
	if err != nil {
		return err
	}
	callDirect := func(i int) error {
		_, err := direct.CallTool(ctx, dts[i%len(dts)].Name, json.RawMessage(`{"x":1}`))
		return err
	}
	// gateway in "all" mode: same calls, namespaced
	gwAll, err := mcp.StartStdio("bin/ccgw", "-farms", of, "-catalog", *catPath, "-mode", "all")
	if err != nil {
		return err
	}
	if err := gwAll.Initialize(ctx, "ccbench"); err != nil {
		return err
	}
	nAll, tokAll, err := listTokens(gwAll)
	if err != nil {
		return err
	}
	callGw := func(i int) error {
		_, err := gwAll.CallTool(ctx, "farm0__"+dts[i%len(dts)].Name, json.RawMessage(`{"x":1}`))
		return err
	}
	for _, conc := range []int{1, 8} {
		for _, p := range []struct {
			name string
			f    func(int) error
			tool int
		}{{"direct to one MCP server", callDirect, len(dts)}, {"through gateway (all mode)", callGw, nAll}} {
			timeCalls(nil, 200, conc, p.f) // warm up
			lat, err := timeCalls(nil, *n, conc, p.f)
			if err != nil {
				return err
			}
			rows = append(rows, latRow{Path: p.name, Op: "tools/call", Conc: conc, N: len(lat), P50us: pctl(lat, 50), P99us: pctl(lat, 99), Tools: p.tool})
			log.Printf("%-28s conc %d p50 %.0f us p99 %.0f us", p.name, conc, pctl(lat, 50), pctl(lat, 99))
		}
	}
	gwAll.Close()
	// gateway in "meta" mode: search, load, call
	gw, err := mcp.StartStdio("bin/ccgw", "-farms", of, "-catalog", *catPath, "-mode", "meta", "-budget", "4000")
	if err != nil {
		return err
	}
	if err := gw.Initialize(ctx, "ccbench"); err != nil {
		return err
	}
	nMeta, tokMeta, err := listTokens(gw)
	if err != nil {
		return err
	}
	var qs []catalog.Query
	for _, q := range c.Queries {
		if q.Tool != "" {
			qs = append(qs, q)
		}
	}
	search := func(i int) error {
		_, err := gw.CallTool(ctx, "search_tools", mustJSON(map[string]any{"query": qs[i%len(qs)].Text, "k": 5}))
		return err
	}
	lat, err := timeCalls(nil, *n, 1, search)
	if err != nil {
		return err
	}
	rows = append(rows, latRow{Path: "gateway (meta mode)", Op: "search_tools over all tools", Conc: 1, N: len(lat), P50us: pctl(lat, 50), P99us: pctl(lat, 99), Tools: c.ToolsLen()})
	load := func(i int) error {
		_, err := gw.CallTool(ctx, "load_tool", mustJSON(map[string]any{"name": "farm0__" + dts[i%len(dts)].Name}))
		return err
	}
	lat, err = timeCalls(nil, *n, 1, load)
	if err != nil {
		return err
	}
	rows = append(rows, latRow{Path: "gateway (meta mode)", Op: "load_tool", Conc: 1, N: len(lat), P50us: pctl(lat, 50), P99us: pctl(lat, 99), Tools: c.ToolsLen()})
	gw.Close()
	direct.Close()
	return writeJSON(*out, map[string]any{"rows": rows, "tools_list_all_mode": map[string]int{"tools": nAll, "tokens": tokAll},
		"tools_list_meta_mode": map[string]int{"tools": nMeta, "tokens": tokMeta}, "farms": *farms})
}

func mustJSON(v any) json.RawMessage {
	b, _ := json.Marshal(v)
	return b
}

var _ = strings.TrimSpace
