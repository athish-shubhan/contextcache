// Command ccgw is the ContextCache MCP gateway. It starts or connects to
// upstream MCP servers over stdio and serves clients over stdio or
// Streamable HTTP (JSON response mode).
//
//	ccgw -farms 10 -catalog data/catalog.json -mode meta           # stdio
//	ccgw -farms 10 -http 127.0.0.1:8090 -tenants 'tokA=acme:farm0,farm1'
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"contextcache/gateway"
	"contextcache/mcp"
	"contextcache/tokens"
)

func main() {
	farms := flag.Int("farms", 4, "start this many toolfarm upstreams")
	farmBin := flag.String("farm-bin", "bin/toolfarm", "toolfarm binary")
	cat := flag.String("catalog", "data/catalog.json", "catalog for the toolfarms")
	limit := flag.Int("limit", 0, "catalog tools to serve (0 = all)")
	upArg := flag.String("upstream", "", "extra upstreams: name=command arg arg;name2=...")
	mode := flag.String("mode", "meta", "all | meta")
	budget := flag.Int("budget", 4000, "tokens of loaded tools kept per session")
	httpAddr := flag.String("http", "", "serve Streamable HTTP here instead of stdio")
	tenants := flag.String("tenants", "", "token=tenant:up1,up2;token2=... (HTTP only)")
	trace := flag.String("trace", "", "write one JSON line per tool call here")
	callTimeout := flag.Duration("call-timeout", 60*time.Second, "limit on each upstream tools/call (0 = none)")
	flag.Parse()
	log.SetOutput(os.Stderr)
	ctx := context.Background()

	type spec struct {
		name string
		argv []string
	}
	var specs []spec
	for i := 0; i < *farms; i++ {
		specs = append(specs, spec{fmt.Sprintf("farm%d", i), []string{*farmBin, "-catalog", *cat, "-server", fmt.Sprint(i), "-of", fmt.Sprint(*farms), "-limit", fmt.Sprint(*limit)}})
	}
	for _, u := range strings.Split(*upArg, ";") {
		if u = strings.TrimSpace(u); u == "" {
			continue
		}
		name, cmd, _ := strings.Cut(u, "=")
		specs = append(specs, spec{name, strings.Fields(cmd)})
	}
	ups := make([]*gateway.Upstream, len(specs))
	var wg sync.WaitGroup
	for i, s := range specs {
		wg.Add(1)
		go func(i int, s spec) {
			defer wg.Done()
			c, err := mcp.StartStdio(s.argv[0], s.argv[1:]...)
			if err != nil {
				log.Fatalf("%s: %v", s.name, err)
			}
			if err := c.Initialize(ctx, "contextcache-gateway"); err != nil {
				log.Fatalf("%s initialize: %v", s.name, err)
			}
			tools, err := c.ListTools(ctx)
			if err != nil {
				log.Fatalf("%s tools/list: %v", s.name, err)
			}
			ups[i] = &gateway.Upstream{Name: s.name, Client: c, Tools: tools}
		}(i, s)
	}
	wg.Wait()
	if *mode != "all" && *mode != "meta" {
		log.Fatalf("unknown -mode %q (want all or meta)", *mode)
	}
	cfg := gateway.Config{Mode: *mode, Budget: *budget, Count: tokens.Count, CallTimeout: *callTimeout}
	if *trace != "" {
		f, err := os.Create(*trace)
		if err != nil {
			log.Fatal(err)
		}
		defer func() { _ = f.Close() }()
		cfg.Trace = f
	}
	var auth func(string) (string, bool)
	if *tenants != "" {
		toks, allowed, err := parseTenants(*tenants)
		if err != nil {
			log.Fatal(err)
		}
		cfg.Tenants = allowed
		auth = func(tok string) (string, bool) { t, ok := toks[tok]; return t, ok }
	}
	g := gateway.New(cfg, ups)
	log.Printf("gateway: %d upstreams, %d tools, mode %s", len(ups), g.ToolCount(), *mode)
	if *httpAddr != "" {
		srv := &http.Server{Addr: *httpAddr, Handler: mcp.HTTPHandler(g, auth),
			ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 2 * time.Minute}
		log.Fatal(srv.ListenAndServe())
	}
	if err := mcp.ServeStdio(ctx, os.Stdin, os.Stdout, g, "stdio"); err != nil {
		log.Fatal(err)
	}
}

// parseTenants reads "token=tenant:up1,up2;token2=tenant2:up3". It returns
// token -> tenant and tenant -> allowed upstreams. Empty entries (a trailing
// ";") are skipped; an entry without a token, a tenant or an upstream is an
// error, so a typo cannot authorise requests that carry no token at all.
func parseTenants(spec string) (map[string]string, map[string][]string, error) {
	toks := map[string]string{}
	allowed := map[string][]string{}
	for i, t := range strings.Split(spec, ";") {
		if t = strings.TrimSpace(t); t == "" {
			continue
		}
		tok, rest, ok1 := strings.Cut(t, "=")
		ten, us, ok2 := strings.Cut(rest, ":")
		tok, ten = strings.TrimSpace(tok), strings.TrimSpace(ten)
		if !ok1 || !ok2 || tok == "" || ten == "" {
			// the entry may hold a token, so report its position, not its text
			return nil, nil, fmt.Errorf("-tenants: entry %d is malformed (want token=tenant:up1,up2)", i+1)
		}
		if _, dup := toks[tok]; dup {
			return nil, nil, fmt.Errorf("-tenants: token for %q given twice", ten)
		}
		var ups []string
		for _, u := range strings.Split(us, ",") {
			if u = strings.TrimSpace(u); u != "" {
				ups = append(ups, u)
			}
		}
		if len(ups) == 0 {
			return nil, nil, fmt.Errorf("-tenants: tenant %q has no upstreams", ten)
		}
		toks[tok] = ten
		allowed[ten] = append(allowed[ten], ups...)
	}
	if len(toks) == 0 {
		return nil, nil, fmt.Errorf("-tenants: no entries in %q", spec)
	}
	return toks, allowed, nil
}
