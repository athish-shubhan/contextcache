package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"contextcache/mcp"
)

// upstreamServer answers tools/call with "<name> <arguments>"; the tool
// "hang" blocks until the channel is closed.
type upstreamServer struct {
	tools []mcp.Tool
	hang  chan struct{}
}

func (u *upstreamServer) Closed(*mcp.Session) {}

func (u *upstreamServer) Handle(ctx context.Context, s *mcp.Session, method string, params json.RawMessage) (any, *mcp.Error) {
	switch method {
	case "initialize":
		return mcp.InitializeResult{ProtocolVersion: mcp.Versions[0], ServerInfo: mcp.Implementation{Name: "up", Version: "1"}}, nil
	case "tools/list":
		return mcp.ListToolsResult{Tools: u.tools}, nil
	case "tools/call":
		var p mcp.CallToolParams
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, &mcp.Error{Code: mcp.InvalidParams, Message: err.Error()}
		}
		if p.Name == "hang" {
			<-u.hang
		}
		return mcp.TextResult(p.Name+" "+string(p.Arguments), false), nil
	}
	return nil, &mcp.Error{Code: mcp.MethodNotFound, Message: method}
}

func tool(name, desc string) mcp.Tool {
	return mcp.Tool{Name: name, Description: desc,
		InputSchema: json.RawMessage(`{"type":"object","properties":{"city":{"type":"string","description":"city name"}}}`)}
}

// startUpstream serves u over in-memory pipes and returns a connected client.
func startUpstream(t *testing.T, name string, u *upstreamServer) *Upstream {
	t.Helper()
	cr, sw := io.Pipe()
	sr, cw := io.Pipe()
	done := make(chan struct{})
	go func() {
		_ = mcp.ServeStdio(context.Background(), sr, sw, u, name)
		_ = sw.Close()
		close(done)
	}()
	c := mcp.NewClient(cr, cw)
	t.Cleanup(func() {
		_ = cw.Close()
		<-done
	})
	if err := c.Initialize(context.Background(), "test"); err != nil {
		t.Fatal(err)
	}
	tools, err := c.ListTools(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return &Upstream{Name: name, Client: c, Tools: tools}
}

type notes struct {
	mu sync.Mutex
	n  []string
}

func (n *notes) session(id, tenant string) *mcp.Session {
	return &mcp.Session{ID: id, Tenant: tenant, Notify: func(m string, _ any) {
		n.mu.Lock()
		n.n = append(n.n, m)
		n.mu.Unlock()
	}}
}

func (n *notes) count() int {
	n.mu.Lock()
	defer n.mu.Unlock()
	return len(n.n)
}

func newTestGateway(t *testing.T, cfg Config) (*Gateway, *upstreamServer) {
	t.Helper()
	weather := &upstreamServer{tools: []mcp.Tool{
		tool("get_weather", "Get the current weather forecast for a city"),
		tool("get_air_quality", "Air quality index for a city"),
		tool("hang", "A tool that never answers"),
	}, hang: make(chan struct{})}
	finance := &upstreamServer{tools: []mcp.Tool{
		tool("get_stock_price", "Latest stock price for a ticker symbol"),
		tool("convert_currency", "Convert an amount between currencies"),
	}}
	ups := []*Upstream{startUpstream(t, "weather", weather), startUpstream(t, "finance", finance)}
	// Cleanups run last-registered first: release a hung call before the
	// pipe teardown waits for the upstream's handlers to finish.
	t.Cleanup(func() { close(weather.hang) })
	return New(cfg, ups), weather
}

func call(t *testing.T, g *Gateway, s *mcp.Session, name string, args any) (mcp.CallToolResult, *mcp.Error) {
	t.Helper()
	b, _ := json.Marshal(map[string]any{"name": name, "arguments": args})
	res, e := g.Handle(context.Background(), s, "tools/call", b)
	if e != nil {
		return mcp.CallToolResult{}, e
	}
	return res.(mcp.CallToolResult), nil
}

func listNames(t *testing.T, g *Gateway, s *mcp.Session, cursor string) ([]string, string, *mcp.Error) {
	t.Helper()
	b, _ := json.Marshal(mcp.ListToolsParams{Cursor: cursor})
	res, e := g.Handle(context.Background(), s, "tools/list", b)
	if e != nil {
		return nil, "", e
	}
	r := res.(mcp.ListToolsResult)
	var names []string
	for _, x := range r.Tools {
		names = append(names, x.Name)
	}
	return names, r.NextCursor, nil
}

func TestMetaModeSearchLoadCall(t *testing.T) {
	var trace bytes.Buffer
	g, _ := newTestGateway(t, Config{Mode: "meta", Budget: 10000, Trace: &trace})
	if g.ToolCount() != 5 {
		t.Fatalf("tool count %d", g.ToolCount())
	}
	var n notes
	s := n.session("s1", "")
	res, e := g.Handle(context.Background(), s, "initialize", json.RawMessage(`{"protocolVersion":"2025-06-18"}`))
	if e != nil || res.(mcp.InitializeResult).ProtocolVersion != "2025-06-18" {
		t.Fatalf("initialize: %+v %v", res, e)
	}

	names, _, _ := listNames(t, g, s, "")
	if strings.Join(names, ",") != "search_tools,load_tool,call_tool" {
		t.Fatalf("meta mode should list only the meta-tools before any load: %v", names)
	}

	r, e := call(t, g, s, "search_tools", map[string]any{"query": "weather forecast city", "k": 2})
	if e != nil || r.IsError {
		t.Fatalf("search: %+v %v", r, e)
	}
	var hits struct {
		Results []searchHit `json:"results"`
	}
	if err := json.Unmarshal(r.StructuredContent, &hits); err != nil {
		t.Fatal(err)
	}
	if len(hits.Results) != 2 || hits.Results[0].Name != "weather__get_weather" || hits.Results[0].Tokens <= 0 {
		t.Fatalf("search hits: %+v", hits.Results)
	}

	r, e = call(t, g, s, "load_tool", map[string]any{"name": "weather__get_weather"})
	if e != nil || !strings.Contains(r.Content[0].Text, `"listed":true`) {
		t.Fatalf("load: %+v %v", r, e)
	}
	if n.count() != 1 {
		t.Fatalf("loading a new tool should send one list_changed, got %d", n.count())
	}
	names, _, _ = listNames(t, g, s, "")
	if len(names) != 4 || names[3] != "weather__get_weather" {
		t.Fatalf("loaded tool should be listed: %v", names)
	}
	// loading it again changes nothing, so no second notification
	_, _ = call(t, g, s, "load_tool", map[string]any{"name": "weather__get_weather"})
	if n.count() != 1 {
		t.Fatalf("reloading should not notify, got %d", n.count())
	}

	r, e = call(t, g, s, "call_tool", map[string]any{"name": "weather__get_weather", "arguments": map[string]string{"city": "Goa"}})
	if e != nil || r.IsError || r.Content[0].Text != `get_weather {"city":"Goa"}` {
		t.Fatalf("call_tool should reach the upstream with the original name: %+v %v", r, e)
	}
	var tr map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(trace.Bytes()), &tr); err != nil {
		t.Fatalf("trace line: %v %q", err, trace.String())
	}
	if tr["tool"] != "weather__get_weather" || tr["upstream"] != "weather" || tr["ok"] != true || tr["session"] != "s1" {
		t.Fatalf("trace: %v", tr)
	}
}

func TestMetaModeBadArguments(t *testing.T) {
	g, _ := newTestGateway(t, Config{Mode: "meta", Budget: 10000})
	s := (&notes{}).session("s", "")
	if _, e := call(t, g, s, "search_tools", map[string]any{}); e == nil || e.Code != mcp.InvalidParams {
		t.Fatalf("search without query: %v", e)
	}
	if _, e := call(t, g, s, "load_tool", map[string]any{}); e == nil || e.Code != mcp.InvalidParams {
		t.Fatalf("load without name: %v", e)
	}
	if r, e := call(t, g, s, "load_tool", map[string]any{"name": "nope__x"}); e != nil || !r.IsError {
		t.Fatalf("load of unknown tool should be a tool error: %+v %v", r, e)
	}
	if r, e := call(t, g, s, "call_tool", map[string]any{"name": "nope__x"}); e != nil || !r.IsError {
		t.Fatalf("call of unknown tool should be a tool error: %+v %v", r, e)
	}
	if _, e := g.Handle(context.Background(), s, "resources/list", nil); e == nil || e.Code != mcp.MethodNotFound {
		t.Fatalf("unknown method: %v", e)
	}
	if _, e := g.Handle(context.Background(), s, "tools/list", json.RawMessage(`[1]`)); e == nil || e.Code != mcp.InvalidParams {
		t.Fatalf("malformed tools/list params: %v", e)
	}
}

// A tool larger than the whole budget is returned by load_tool but not kept listed.
func TestBudgetKeepsLargeToolOut(t *testing.T) {
	g, _ := newTestGateway(t, Config{Mode: "meta", Budget: 5})
	var n notes
	s := n.session("s", "")
	r, _ := call(t, g, s, "load_tool", map[string]any{"name": "finance__get_stock_price"})
	if !strings.Contains(r.Content[0].Text, `"listed":false`) || !strings.Contains(r.Content[0].Text, "get_stock_price") {
		t.Fatalf("load over budget: %s", r.Content[0].Text)
	}
	names, _, _ := listNames(t, g, s, "")
	if len(names) != 3 || n.count() != 0 {
		t.Fatalf("nothing should be listed or notified: %v, %d notes", names, n.count())
	}
}

func TestTenantIsolation(t *testing.T) {
	g, _ := newTestGateway(t, Config{Mode: "meta", Budget: 10000, Tenants: map[string][]string{"acme": {"weather"}}})
	s := (&notes{}).session("s", "acme")
	r, _ := call(t, g, s, "search_tools", map[string]any{"query": "stock price ticker currency", "k": 5})
	if strings.Contains(r.Content[0].Text, "finance__") {
		t.Fatalf("tenant acme must not see finance tools in search: %s", r.Content[0].Text)
	}
	for _, name := range []string{"load_tool", "call_tool"} {
		r, e := call(t, g, s, name, map[string]any{"name": "finance__get_stock_price"})
		if e != nil || !r.IsError {
			t.Fatalf("%s of another tenant's tool must fail: %+v %v", name, r, e)
		}
	}
	if r, e := call(t, g, s, "finance__get_stock_price", map[string]any{}); e != nil || !r.IsError {
		t.Fatalf("direct tools/call of another tenant's tool must fail: %+v %v", r, e)
	}
	if r, _ := call(t, g, s, "call_tool", map[string]any{"name": "weather__get_air_quality"}); r.IsError {
		t.Fatalf("own tool: %+v", r)
	}
	gAll, _ := newTestGateway(t, Config{Mode: "all", Tenants: map[string][]string{"acme": {"weather"}}})
	if names, _, _ := listNames(t, gAll, s, ""); len(names) != 3 || !strings.HasPrefix(names[0], "weather__") {
		t.Fatalf("all mode lists only the tenant's upstreams: %v", names)
	}
	other := (&notes{}).session("s2", "unknown-tenant")
	if names, _, _ := listNames(t, gAll, other, ""); len(names) != 0 {
		t.Fatalf("a tenant with no upstreams should see nothing: %v", names)
	}
}

func TestAllModePagination(t *testing.T) {
	g, _ := newTestGateway(t, Config{Mode: "all", PageSize: 2})
	s := (&notes{}).session("s", "")
	var all []string
	cursor := ""
	for i := 0; i < 10; i++ {
		names, next, e := listNames(t, g, s, cursor)
		if e != nil {
			t.Fatal(e)
		}
		all = append(all, names...)
		if next == "" {
			break
		}
		cursor = next
	}
	want := "finance__convert_currency,finance__get_stock_price,weather__get_air_quality,weather__get_weather,weather__hang"
	if strings.Join(all, ",") != want {
		t.Fatalf("got %v", all)
	}
	for _, c := range []string{"x", "-1", "99"} {
		if _, _, e := listNames(t, g, s, c); e == nil || e.Code != mcp.InvalidParams {
			t.Fatalf("cursor %q: %v", c, e)
		}
	}
	r, e := call(t, g, s, "finance__convert_currency", map[string]any{"amount": 3})
	if e != nil || r.Content[0].Text != `convert_currency {"amount":3}` {
		t.Fatalf("direct call in all mode: %+v %v", r, e)
	}
}

// A hung upstream tool becomes a tool error after CallTimeout instead of
// blocking the caller forever.
func TestCallTimeout(t *testing.T) {
	g, _ := newTestGateway(t, Config{Mode: "all", CallTimeout: 50 * time.Millisecond})
	s := (&notes{}).session("s", "")
	start := time.Now()
	r, e := call(t, g, s, "weather__hang", map[string]any{})
	if e != nil || !r.IsError || !strings.Contains(r.Content[0].Text, "deadline exceeded") {
		t.Fatalf("got %+v %v", r, e)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("took %v", d)
	}
}
