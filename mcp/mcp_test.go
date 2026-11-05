package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

// fake is a minimal MCP server: n tools named t0..t{n-1}, pageSize per
// tools/list page, tools/call echoes the tool name and arguments.
type fake struct {
	n, pageSize int
	stuckCursor bool          // always answer nextCursor "1"
	hang        chan struct{} // tools/call of "hang" blocks until closed
	closed      chan *Session
}

func (f *fake) Closed(s *Session) {
	if f.closed != nil {
		f.closed <- s
	}
}

func (f *fake) Handle(ctx context.Context, s *Session, method string, params json.RawMessage) (any, *Error) {
	switch method {
	case "initialize":
		var p InitializeParams
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, &Error{InvalidParams, err.Error()}
		}
		return InitializeResult{ProtocolVersion: Negotiate(p.ProtocolVersion), ServerInfo: Implementation{Name: "fake", Version: "1"},
			Capabilities: map[string]any{"tools": map[string]any{}}}, nil
	case "tools/list":
		var p ListToolsParams
		_ = json.Unmarshal(params, &p)
		start, _ := strconv.Atoi(p.Cursor)
		end := min(f.n, start+f.pageSize)
		r := ListToolsResult{}
		for i := start; i < end; i++ {
			r.Tools = append(r.Tools, Tool{Name: fmt.Sprint("t", i), InputSchema: json.RawMessage(`{"type":"object"}`)})
		}
		if f.stuckCursor {
			r.NextCursor = "1"
		} else if end < f.n {
			r.NextCursor = strconv.Itoa(end)
		}
		return r, nil
	case "tools/call":
		var p CallToolParams
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, &Error{InvalidParams, err.Error()}
		}
		if p.Name == "hang" {
			<-f.hang
		}
		if p.Name == "notify" {
			s.Notify("notifications/tools/list_changed", struct{}{})
		}
		return TextResult(p.Name+" "+string(p.Arguments), false), nil
	case "tenant":
		return map[string]string{"tenant": s.Tenant, "session": s.ID}, nil
	}
	return nil, &Error{MethodNotFound, "method not found: " + method}
}

// pipeClient connects a Client to ServeStdio(h) over in-memory pipes.
func pipeClient(t *testing.T, h Handler) *Client {
	t.Helper()
	cr, sw := io.Pipe() // server -> client
	sr, cw := io.Pipe() // client -> server
	done := make(chan struct{})
	go func() {
		_ = ServeStdio(context.Background(), sr, sw, h, "stdio-test")
		_ = sw.Close()
		close(done)
	}()
	c := NewClient(cr, cw)
	t.Cleanup(func() {
		_ = cw.Close()
		<-done
	})
	return c
}

func TestNegotiate(t *testing.T) {
	if v := Negotiate("2025-06-18"); v != "2025-06-18" {
		t.Fatalf("known version: %s", v)
	}
	if v := Negotiate("1999-01-01"); v != Versions[0] {
		t.Fatalf("unknown version should get the newest, got %s", v)
	}
}

func TestStdioRoundTrip(t *testing.T) {
	c := pipeClient(t, &fake{n: 250, pageSize: 100})
	ctx := context.Background()
	if err := c.Initialize(ctx, "test"); err != nil {
		t.Fatal(err)
	}
	if c.Info.ProtocolVersion != Versions[0] || c.Info.ServerInfo.Name != "fake" {
		t.Fatalf("initialize: %+v", c.Info)
	}
	tools, err := c.ListTools(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(tools) != 250 || tools[0].Name != "t0" || tools[249].Name != "t249" {
		t.Fatalf("pagination: got %d tools", len(tools))
	}
	res, err := c.CallTool(ctx, "t7", json.RawMessage(`{"x":1}`))
	if err != nil || res.IsError || len(res.Content) != 1 || res.Content[0].Text != `t7 {"x":1}` {
		t.Fatalf("call: %+v %v", res, err)
	}
	err = c.Call(ctx, "nope/method", nil, nil)
	var e *Error
	if !errors.As(err, &e) || e.Code != MethodNotFound {
		t.Fatalf("unknown method: %v", err)
	}
}

// Requests are handled concurrently and multiplexed by id.
func TestStdioConcurrentCalls(t *testing.T) {
	c := pipeClient(t, &fake{n: 1, pageSize: 1})
	ctx := context.Background()
	errs := make(chan error, 50)
	for i := 0; i < 50; i++ {
		go func(i int) {
			name := fmt.Sprint("t", i)
			res, err := c.CallTool(ctx, name, json.RawMessage(`{}`))
			if err == nil && res.Content[0].Text != name+" {}" {
				err = fmt.Errorf("call %d got %q", i, res.Content[0].Text)
			}
			errs <- err
		}(i)
	}
	for i := 0; i < 50; i++ {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
}

func TestStdioNotificationReachesClient(t *testing.T) {
	c := pipeClient(t, &fake{n: 1, pageSize: 1})
	got := make(chan string, 1)
	c.OnNote = func(m Message) { got <- m.Method }
	if _, err := c.CallTool(context.Background(), "notify", nil); err != nil {
		t.Fatal(err)
	}
	select {
	case m := <-got:
		if m != "notifications/tools/list_changed" {
			t.Fatalf("got %s", m)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no notification")
	}
}

func TestStdioParseErrorAndNotificationsIgnored(t *testing.T) {
	in := strings.NewReader("this is not json\n" +
		`{"jsonrpc":"2.0","method":"notifications/initialized"}` + "\n\n" +
		`{"jsonrpc":"2.0","id":7,"method":"ping-unknown"}` + "\n")
	var out bytes.Buffer
	if err := ServeStdio(context.Background(), in, &out, &fake{}, "s"); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("want 2 responses (parse error, method not found), got %q", out.String())
	}
	byID := map[string]Message{}
	for _, l := range lines {
		var m Message
		if err := json.Unmarshal([]byte(l), &m); err != nil {
			t.Fatal(err)
		}
		byID[string(m.ID)] = m
	}
	if m := byID["null"]; m.Error == nil || m.Error.Code != ParseError {
		t.Fatalf("parse error response: %+v", m)
	}
	if m := byID["7"]; m.Error == nil || m.Error.Code != MethodNotFound {
		t.Fatalf("unknown method response: %+v", m)
	}
}

func TestListToolsRejectsRepeatedCursor(t *testing.T) {
	c := pipeClient(t, &fake{n: 5, pageSize: 1, stuckCursor: true})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := c.ListTools(ctx)
	if err == nil || !strings.Contains(err.Error(), "twice") {
		t.Fatalf("a server repeating its cursor must not loop forever: %v", err)
	}
}

func TestCallHonoursContext(t *testing.T) {
	f := &fake{n: 1, pageSize: 1, hang: make(chan struct{})}
	c := pipeClient(t, f)
	t.Cleanup(func() { close(f.hang) }) // runs before the pipe cleanup
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := c.CallTool(ctx, "hang", nil); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("got %v", err)
	}
}

// ---- Streamable HTTP

type httpClient struct {
	t   *testing.T
	url string
}

func (h httpClient) do(method, token, session, body string) (*http.Response, Message) {
	h.t.Helper()
	req, err := http.NewRequest(method, h.url, strings.NewReader(body))
	if err != nil {
		h.t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if session != "" {
		req.Header.Set("Mcp-Session-Id", session)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	var m Message
	b, _ := io.ReadAll(resp.Body)
	_ = json.Unmarshal(b, &m)
	return resp, m
}

const initBody = `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"t","version":"1"}}}`

func TestHTTPSessionsAndTenants(t *testing.T) {
	f := &fake{n: 3, pageSize: 10, closed: make(chan *Session, 4)}
	auth := func(tok string) (string, bool) {
		t, ok := map[string]string{"tokA": "acme", "tokB": "beta"}[tok]
		return t, ok
	}
	srv := httptest.NewServer(HTTPHandler(f, auth))
	defer srv.Close()
	c := httpClient{t, srv.URL}

	if resp, _ := c.do("POST", "wrong", "", initBody); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("bad token: %d", resp.StatusCode)
	}
	if resp, _ := c.do("POST", "", "", initBody); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("no token: %d", resp.StatusCode)
	}
	resp, m := c.do("POST", "tokA", "", initBody)
	sidA := resp.Header.Get("Mcp-Session-Id")
	if resp.StatusCode != 200 || sidA == "" || m.Error != nil {
		t.Fatalf("initialize: %d %q %+v", resp.StatusCode, sidA, m.Error)
	}
	var ir InitializeResult
	if err := json.Unmarshal(m.Result, &ir); err != nil || ir.ProtocolVersion != "2025-06-18" {
		t.Fatalf("negotiated %+v %v", ir, err)
	}
	resp, m = c.do("POST", "tokA", sidA, `{"jsonrpc":"2.0","id":2,"method":"tenant"}`)
	if resp.StatusCode != 200 || !strings.Contains(string(m.Result), `"tenant":"acme"`) {
		t.Fatalf("tenant on session: %d %s", resp.StatusCode, m.Result)
	}
	if resp, _ := c.do("POST", "tokA", "", `{"jsonrpc":"2.0","id":3,"method":"tools/list"}`); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("missing session id: %d", resp.StatusCode)
	}
	if resp, _ := c.do("POST", "tokB", sidA, `{"jsonrpc":"2.0","id":4,"method":"tools/list"}`); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("other tenant using the session: %d", resp.StatusCode)
	}
	if resp, _ := c.do("POST", "tokA", sidA, `{"jsonrpc":"2.0","method":"notifications/initialized"}`); resp.StatusCode != http.StatusAccepted {
		t.Fatalf("notification: %d", resp.StatusCode)
	}
	if resp, m := c.do("POST", "tokA", sidA, `{not json`); resp.StatusCode != http.StatusBadRequest || m.Error == nil || m.Error.Code != ParseError {
		t.Fatalf("bad json: %d %+v", resp.StatusCode, m.Error)
	}
	if resp, _ := c.do("GET", "tokA", sidA, ""); resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("GET: %d", resp.StatusCode)
	}

	// Another tenant must not be able to end this session.
	if resp, _ := c.do("DELETE", "tokB", sidA, ""); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("cross-tenant DELETE: %d", resp.StatusCode)
	}
	if resp, _ := c.do("POST", "tokA", sidA, `{"jsonrpc":"2.0","id":5,"method":"tools/list"}`); resp.StatusCode != 200 {
		t.Fatalf("session should survive a cross-tenant DELETE: %d", resp.StatusCode)
	}
	if resp, _ := c.do("DELETE", "tokA", sidA, ""); resp.StatusCode != 200 {
		t.Fatalf("DELETE: %d", resp.StatusCode)
	}
	select {
	case s := <-f.closed:
		if s.ID != sidA {
			t.Fatalf("closed %s, want %s", s.ID, sidA)
		}
	case <-time.After(time.Second):
		t.Fatal("handler not told the session closed")
	}
	if resp, _ := c.do("POST", "tokA", sidA, `{"jsonrpc":"2.0","id":6,"method":"tools/list"}`); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("deleted session still usable: %d", resp.StatusCode)
	}
	if resp, _ := c.do("DELETE", "tokA", sidA, ""); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("DELETE of unknown session: %d", resp.StatusCode)
	}
}

func TestHTTPWithoutAuth(t *testing.T) {
	srv := httptest.NewServer(HTTPHandler(&fake{}, nil))
	defer srv.Close()
	c := httpClient{t, srv.URL}
	resp, _ := c.do("POST", "", "", initBody)
	sid := resp.Header.Get("Mcp-Session-Id")
	if resp.StatusCode != 200 || sid == "" {
		t.Fatalf("initialize: %d", resp.StatusCode)
	}
	resp2, _ := c.do("POST", "", "", initBody)
	if resp2.Header.Get("Mcp-Session-Id") == sid {
		t.Fatal("two initializes got the same session id")
	}
}
