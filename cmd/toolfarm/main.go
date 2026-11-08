// Command toolfarm is an upstream MCP server (stdio) that serves a slice of
// the tool catalog and returns canned results. Many toolfarms behind the
// gateway stand in for many real MCP servers.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"strconv"
	"time"

	"contextcache/catalog"
	"contextcache/mcp"
)

type farm struct {
	tools []mcp.Tool
	delay time.Duration
}

func (f *farm) Closed(*mcp.Session) {}

func (f *farm) Handle(ctx context.Context, s *mcp.Session, method string, params json.RawMessage) (any, *mcp.Error) {
	switch method {
	case "initialize":
		var p mcp.InitializeParams
		_ = json.Unmarshal(params, &p) // a missing version negotiates the newest
		return mcp.InitializeResult{ProtocolVersion: mcp.Negotiate(p.ProtocolVersion), Capabilities: map[string]any{"tools": map[string]any{}},
			ServerInfo: mcp.Implementation{Name: "toolfarm", Version: "0.1"}}, nil
	case "ping":
		return struct{}{}, nil
	case "tools/list":
		var p mcp.ListToolsParams
		_ = json.Unmarshal(params, &p)
		start := 0
		if p.Cursor != "" {
			var err error
			// an out-of-range cursor used to panic on the slice below
			if start, err = strconv.Atoi(p.Cursor); err != nil || start < 0 || start > len(f.tools) {
				return nil, &mcp.Error{Code: mcp.InvalidParams, Message: "invalid cursor"}
			}
		}
		end := min(len(f.tools), start+100)
		r := mcp.ListToolsResult{Tools: f.tools[start:end]}
		if end < len(f.tools) {
			r.NextCursor = strconv.Itoa(end)
		}
		return r, nil
	case "tools/call":
		var p mcp.CallToolParams
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, &mcp.Error{Code: mcp.InvalidParams, Message: err.Error()}
		}
		if f.delay > 0 {
			time.Sleep(f.delay)
		}
		for _, t := range f.tools {
			if t.Name == p.Name {
				return mcp.TextResult(fmt.Sprintf(`{"tool":%q,"ok":true,"arguments":%s}`, p.Name, orEmpty(p.Arguments)), false), nil
			}
		}
		return mcp.TextResult("no such tool: "+p.Name, true), nil
	}
	return nil, &mcp.Error{Code: mcp.MethodNotFound, Message: method}
}

func orEmpty(b json.RawMessage) string {
	if len(b) == 0 {
		return "{}"
	}
	return string(b)
}

func main() {
	cat := flag.String("catalog", "data/catalog.json", "catalog (ccbench prepare)")
	idx := flag.Int("server", 0, "this server's index")
	of := flag.Int("of", 1, "number of servers the catalog is split over")
	limit := flag.Int("limit", 0, "use only the first N catalog tools (0 = all)")
	delay := flag.Duration("delay", 0, "simulated tool latency")
	flag.Parse()
	c, err := catalog.Load(*cat)
	if err != nil {
		log.Fatal(err)
	}
	f := &farm{delay: *delay}
	seen := map[string]bool{}
	for i, t := range c.Tools {
		if *limit > 0 && i >= *limit {
			break
		}
		if i%*of != *idx {
			continue
		}
		name := t.ID // unique within the catalog; '#' is replaced for MCP tool-name rules
		name = sanitize(name)
		if seen[name] {
			continue
		}
		seen[name] = true
		f.tools = append(f.tools, mcp.Tool{Name: name, Description: t.Description, InputSchema: t.Schema})
	}
	if err := mcp.ServeStdio(context.Background(), os.Stdin, os.Stdout, f, "farm"); err != nil {
		log.Fatal(err)
	}
}

// sanitize keeps tool names within [A-Za-z0-9_.-].
func sanitize(s string) string {
	b := []byte(s)
	for i, c := range b {
		ok := c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '-' || c == '.'
		if !ok {
			b[i] = '_'
		}
	}
	return string(b)
}
