// Package gateway is an MCP gateway that aggregates many MCP servers and
// treats the agent's tool context as a cache. In "all" mode it exposes every
// upstream tool (the baseline that floods the context). In "meta" mode it
// exposes three tools (search_tools, load_tool, call_tool) plus whatever this
// session has loaded; a token-budgeted W-TinyLFU cache decides which loaded
// tools stay listed, and tools/list_changed tells the client when that set
// changes. Every tool call is traced.
package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"contextcache/admit"
	"contextcache/mcp"
	"contextcache/retrieve"
)

type Upstream struct {
	Name   string
	Client *mcp.Client
	Tools  []mcp.Tool
}

type Config struct {
	Mode     string              // all | meta
	Budget   int                 // tokens of loaded tools kept per session (meta)
	TopK     int                 // default search_tools results
	Tenants  map[string][]string // tenant -> allowed upstreams; nil = everyone sees everything
	Trace    io.Writer           // JSON lines, one per tool call (nil = off)
	Count    func(string) int    // token counter
	PageSize int
}

type entry struct {
	up     *Upstream
	tool   mcp.Tool
	orig   string
	tokens int
}

type Gateway struct {
	cfg     Config
	entries map[string]*entry
	names   []string
	ranker  retrieve.Ranker
	tmu     sync.Mutex
}

type session struct {
	mu    sync.Mutex
	cache admit.Cache
}

const sep = "__"

func New(cfg Config, ups []*Upstream) *Gateway {
	if cfg.TopK == 0 {
		cfg.TopK = 5
	}
	if cfg.PageSize == 0 {
		cfg.PageSize = 100
	}
	if cfg.Count == nil {
		cfg.Count = func(s string) int { return (len(s) + 3) / 4 }
	}
	g := &Gateway{cfg: cfg, entries: map[string]*entry{}}
	var docs []retrieve.Doc
	for _, u := range ups {
		for _, t := range u.Tools {
			name := u.Name + sep + t.Name
			et := t
			et.Name = name
			b, _ := json.Marshal(et)
			e := &entry{up: u, tool: et, orig: t.Name, tokens: cfg.Count(string(b))}
			g.entries[name] = e
			g.names = append(g.names, name)
			docs = append(docs, retrieve.Doc{ID: name, Text: t.Name + " " + t.Description + " " + paramText(t.InputSchema)})
		}
	}
	sort.Strings(g.names)
	g.ranker = retrieve.Hybrid(docs)
	return g
}

func paramText(schema json.RawMessage) string {
	var s struct {
		Properties map[string]struct {
			Description string `json:"description"`
		} `json:"properties"`
	}
	if json.Unmarshal(schema, &s) != nil {
		return ""
	}
	keys := make([]string, 0, len(s.Properties))
	for k := range s.Properties {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for _, k := range keys {
		b.WriteString(k + " " + s.Properties[k].Description + " ")
	}
	return b.String()
}

func (g *Gateway) allowed(tenant string, e *entry) bool {
	if g.cfg.Tenants == nil || e == nil {
		return e != nil
	}
	for _, u := range g.cfg.Tenants[tenant] {
		if u == e.up.Name {
			return true
		}
	}
	return false
}

var metaTools = []mcp.Tool{
	{Name: "search_tools", Description: "Search the tool catalog. Returns tool names with one-line descriptions; load a tool before calling it.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"query":{"type":"string","description":"what you need to do"},"k":{"type":"integer","description":"number of results (default 5)"}},"required":["query"]}`)},
	{Name: "load_tool", Description: "Load a tool's full schema into this session. Returns the schema; frequently used tools stay listed.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"name":{"type":"string"}},"required":["name"]}`)},
	{Name: "call_tool", Description: "Call any catalog tool by name with its arguments.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"name":{"type":"string"},"arguments":{"type":"object"}},"required":["name"]}`)},
}

func (g *Gateway) sess(s *mcp.Session) *session {
	if st, ok := s.State.(*session); ok {
		return st
	}
	st := &session{cache: admit.NewTinyLFU(max(g.cfg.Budget, 1), len(g.entries)+1)}
	s.State = st
	return st
}

func bad(msg string) *mcp.Error { return &mcp.Error{Code: mcp.InvalidParams, Message: msg} }

func (g *Gateway) Closed(*mcp.Session) {}

func (g *Gateway) Handle(ctx context.Context, s *mcp.Session, method string, params json.RawMessage) (any, *mcp.Error) {
	switch method {
	case "initialize":
		var p mcp.InitializeParams
		json.Unmarshal(params, &p)
		g.sess(s)
		return mcp.InitializeResult{ProtocolVersion: mcp.Negotiate(p.ProtocolVersion),
			Capabilities: map[string]any{"tools": map[string]any{"listChanged": true}},
			ServerInfo:   mcp.Implementation{Name: "contextcache-gateway", Version: "0.1"},
			Instructions: g.instructions()}, nil
	case "ping":
		return struct{}{}, nil
	case "tools/list":
		var p mcp.ListToolsParams
		json.Unmarshal(params, &p)
		return g.list(s, p.Cursor)
	case "tools/call":
		var p mcp.CallToolParams
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, bad(err.Error())
		}
		return g.call(ctx, s, p)
	}
	return nil, &mcp.Error{Code: mcp.MethodNotFound, Message: "method not found: " + method}
}

func (g *Gateway) instructions() string {
	if g.cfg.Mode == "meta" {
		return fmt.Sprintf("This gateway fronts %d tools. Use search_tools to find one, load_tool to see its schema, then call it.", len(g.entries))
	}
	return ""
}

func (g *Gateway) list(s *mcp.Session, cursor string) (any, *mcp.Error) {
	var tools []mcp.Tool
	if g.cfg.Mode == "meta" {
		tools = append(tools, metaTools...)
		st := g.sess(s)
		st.mu.Lock()
		items := st.cache.Items()
		st.mu.Unlock()
		sort.Strings(items)
		for _, n := range items {
			if e := g.entries[n]; g.allowed(s.Tenant, e) {
				tools = append(tools, e.tool)
			}
		}
	} else {
		for _, n := range g.names {
			if e := g.entries[n]; g.allowed(s.Tenant, e) {
				tools = append(tools, e.tool)
			}
		}
	}
	start := 0
	if cursor != "" {
		v, err := strconv.Atoi(cursor)
		if err != nil || v < 0 || v > len(tools) {
			return nil, bad("invalid cursor")
		}
		start = v
	}
	end := min(len(tools), start+g.cfg.PageSize)
	res := mcp.ListToolsResult{Tools: tools[start:end]}
	if end < len(tools) {
		res.NextCursor = strconv.Itoa(end)
	}
	return res, nil
}

type searchHit struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Tokens      int    `json:"schema_tokens"`
}

func short(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

func jsonResult(v any) mcp.CallToolResult {
	b, _ := json.Marshal(v)
	return mcp.CallToolResult{Content: []mcp.Content{{Type: "text", Text: string(b)}}, StructuredContent: b}
}

func (g *Gateway) call(ctx context.Context, s *mcp.Session, p mcp.CallToolParams) (any, *mcp.Error) {
	if g.cfg.Mode == "meta" {
		switch p.Name {
		case "search_tools":
			var a struct {
				Query string `json:"query"`
				K     int    `json:"k"`
			}
			if json.Unmarshal(p.Arguments, &a) != nil || a.Query == "" {
				return nil, bad("search_tools needs a query")
			}
			if a.K <= 0 {
				a.K = g.cfg.TopK
			}
			var hits []searchHit
			for _, h := range g.ranker.Rank(a.Query, 4*a.K) {
				e := g.entries[h.ID]
				if !g.allowed(s.Tenant, e) {
					continue
				}
				hits = append(hits, searchHit{h.ID, short(e.tool.Description, 160), e.tokens})
				if len(hits) == a.K {
					break
				}
			}
			return jsonResult(map[string]any{"results": hits}), nil
		case "load_tool":
			var a struct {
				Name string `json:"name"`
			}
			json.Unmarshal(p.Arguments, &a)
			e := g.entries[a.Name]
			if !g.allowed(s.Tenant, e) {
				return mcp.TextResult("unknown tool "+a.Name, true), nil
			}
			st := g.sess(s)
			st.mu.Lock()
			before := st.cache.Items()
			st.cache.Access(a.Name, e.tokens)
			kept := st.cache.Contains(a.Name)
			changed := !sameSet(before, st.cache.Items())
			st.mu.Unlock()
			if changed && s.Notify != nil {
				s.Notify("notifications/tools/list_changed", struct{}{})
			}
			return jsonResult(map[string]any{"tool": e.tool, "listed": kept}), nil
		case "call_tool":
			var a struct {
				Name      string          `json:"name"`
				Arguments json.RawMessage `json:"arguments"`
			}
			if json.Unmarshal(p.Arguments, &a) != nil {
				return nil, bad("call_tool needs name and arguments")
			}
			return g.proxy(ctx, s, a.Name, a.Arguments)
		}
	}
	return g.proxy(ctx, s, p.Name, p.Arguments)
}

func sameSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	m := map[string]bool{}
	for _, x := range a {
		m[x] = true
	}
	for _, x := range b {
		if !m[x] {
			return false
		}
	}
	return true
}

func (g *Gateway) proxy(ctx context.Context, s *mcp.Session, name string, args json.RawMessage) (any, *mcp.Error) {
	e := g.entries[name]
	if !g.allowed(s.Tenant, e) {
		return mcp.TextResult("unknown tool "+name, true), nil
	}
	if len(args) == 0 {
		args = json.RawMessage(`{}`)
	}
	t := time.Now()
	res, err := e.up.Client.CallTool(ctx, e.orig, args)
	dur := time.Since(t)
	if g.cfg.Mode == "meta" {
		st := g.sess(s)
		st.mu.Lock()
		st.cache.Access(name, e.tokens) // usage is the frequency signal
		st.mu.Unlock()
	}
	g.trace(s, name, e.up.Name, dur, err == nil && !res.IsError)
	if err != nil {
		return mcp.TextResult("upstream error: "+err.Error(), true), nil
	}
	return res, nil
}

func (g *Gateway) trace(s *mcp.Session, tool, up string, d time.Duration, ok bool) {
	if g.cfg.Trace == nil {
		return
	}
	b, _ := json.Marshal(map[string]any{"ts": time.Now().UTC().Format(time.RFC3339Nano), "session": s.ID, "tenant": s.Tenant,
		"tool": tool, "upstream": up, "upstream_us": d.Microseconds(), "ok": ok})
	g.tmu.Lock()
	g.cfg.Trace.Write(append(b, '\n'))
	g.tmu.Unlock()
}

// ToolCount is the number of aggregated tools.
func (g *Gateway) ToolCount() int { return len(g.entries) }
