package mcp

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type echo struct{}

func (echo) Handle(_ context.Context, s *Session, method string, _ json.RawMessage) (any, *Error) {
	if method == "boom" {
		return nil, &Error{Code: MethodNotFound, Message: "no"}
	}
	return map[string]string{"tenant": s.Tenant}, nil
}
func (echo) Closed(*Session) {}

func post(t *testing.T, url, tok, sid, body string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest("POST", url, strings.NewReader(body))
	if tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	if sid != "" {
		req.Header.Set("Mcp-Session-Id", sid)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return resp
}

const initMsg = `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`

func TestHTTPAuthAndTenantIsolation(t *testing.T) {
	auth := func(tok string) (string, bool) {
		switch tok {
		case "a":
			return "acme", true
		case "b":
			return "bolt", true
		}
		return "", false
	}
	srv := httptest.NewServer(HTTPHandler(echo{}, auth))
	defer srv.Close()
	if c := post(t, srv.URL, "", "", initMsg).StatusCode; c != http.StatusUnauthorized {
		t.Fatalf("no token: %d", c)
	}
	if c := post(t, srv.URL, "bad", "", initMsg).StatusCode; c != http.StatusUnauthorized {
		t.Fatalf("bad token: %d", c)
	}
	r := post(t, srv.URL, "a", "", initMsg)
	sid := r.Header.Get("Mcp-Session-Id")
	if r.StatusCode != 200 || sid == "" {
		t.Fatalf("initialize: %d %q", r.StatusCode, sid)
	}
	call := `{"jsonrpc":"2.0","id":2,"method":"ping"}`
	if c := post(t, srv.URL, "a", sid, call).StatusCode; c != 200 {
		t.Fatalf("own session: %d", c)
	}
	if c := post(t, srv.URL, "b", sid, call).StatusCode; c != http.StatusForbidden {
		t.Fatalf("other tenant: %d", c)
	}
	if c := post(t, srv.URL, "a", "nope", call).StatusCode; c != http.StatusNotFound {
		t.Fatalf("unknown session: %d", c)
	}
}

func TestHTTPBadBodyAndSessionCap(t *testing.T) {
	srv := httptest.NewServer(HTTPHandlerLimit(echo{}, nil, 1))
	defer srv.Close()
	if c := post(t, srv.URL, "", "", "{not json").StatusCode; c != http.StatusBadRequest {
		t.Fatalf("bad json: %d", c)
	}
	if c := post(t, srv.URL, "", "", initMsg).StatusCode; c != 200 {
		t.Fatalf("first session: %d", c)
	}
	if c := post(t, srv.URL, "", "", initMsg).StatusCode; c != http.StatusServiceUnavailable {
		t.Fatalf("session cap: %d", c)
	}
}
