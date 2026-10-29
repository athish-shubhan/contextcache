package mcp

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
)

// Session is one connected client. Notify sends a server-to-client
// notification when the transport can carry one (stdio can; the HTTP JSON
// response mode cannot, so there it is a no-op and clients re-list).
type Session struct {
	ID     string
	Tenant string
	Notify func(method string, params any)
	State  any
}

type Handler interface {
	// Handle answers a request. A nil error and nil result is an empty object.
	Handle(ctx context.Context, s *Session, method string, params json.RawMessage) (any, *Error)
	Closed(s *Session)
}

func respond(id json.RawMessage, res any, e *Error) Message {
	m := Message{JSONRPC: "2.0", ID: id}
	if e != nil {
		m.Error = e
		return m
	}
	if res == nil {
		res = struct{}{}
	}
	b, err := json.Marshal(res)
	if err != nil {
		m.Error = &Error{InternalError, err.Error()}
		return m
	}
	m.Result = b
	return m
}

// ServeStdio serves one session on a newline-delimited JSON stream. Requests
// are handled concurrently; responses may come back out of order, as
// JSON-RPC allows.
func ServeStdio(ctx context.Context, r io.Reader, w io.Writer, h Handler, sessionID string) error {
	var wmu sync.Mutex
	enc := json.NewEncoder(w) // Encode writes one line per message
	write := func(m any) {
		wmu.Lock()
		enc.Encode(m)
		wmu.Unlock()
	}
	s := &Session{ID: sessionID}
	s.Notify = func(method string, params any) {
		b, _ := json.Marshal(params)
		write(Message{JSONRPC: "2.0", Method: method, Params: b})
	}
	defer h.Closed(s)
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 1<<20), 64<<20)
	var wg sync.WaitGroup
	for sc.Scan() {
		line := sc.Bytes()
		if len(strings.TrimSpace(string(line))) == 0 {
			continue
		}
		var m Message
		if err := json.Unmarshal(line, &m); err != nil {
			write(Message{JSONRPC: "2.0", ID: json.RawMessage("null"), Error: &Error{ParseError, err.Error()}})
			continue
		}
		if !m.IsRequest() {
			continue // notifications (initialized, cancelled) and stray responses
		}
		wg.Add(1)
		go func(m Message) {
			defer wg.Done()
			res, e := h.Handle(ctx, s, m.Method, m.Params)
			write(respond(m.ID, res, e))
		}(m)
	}
	wg.Wait()
	return sc.Err()
}

// HTTPHandler serves Streamable HTTP in JSON response mode: each POST
// carries one JSON-RPC message and gets one JSON response. Sessions are
// identified by the Mcp-Session-Id header issued at initialize. auth maps a
// bearer token to a tenant; nil means no authentication.
func HTTPHandler(h Handler, auth func(token string) (string, bool)) http.Handler {
	var mu sync.Mutex
	sessions := map[string]*Session{}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tenant := ""
		if auth != nil {
			tok := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
			t, ok := auth(tok)
			if !ok {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			tenant = t
		}
		switch r.Method {
		case http.MethodDelete:
			mu.Lock()
			if s := sessions[r.Header.Get("Mcp-Session-Id")]; s != nil {
				delete(sessions, s.ID)
				h.Closed(s)
			}
			mu.Unlock()
			w.WriteHeader(http.StatusOK)
			return
		case http.MethodGet:
			// no server-initiated stream in JSON mode
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		case http.MethodPost:
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		var m Message
		if err := json.NewDecoder(io.LimitReader(r.Body, 16<<20)).Decode(&m); err != nil {
			writeJSON(w, http.StatusBadRequest, Message{JSONRPC: "2.0", ID: json.RawMessage("null"), Error: &Error{ParseError, err.Error()}})
			return
		}
		sid := r.Header.Get("Mcp-Session-Id")
		mu.Lock()
		s := sessions[sid]
		if m.Method == "initialize" {
			b := make([]byte, 16)
			rand.Read(b)
			s = &Session{ID: hex.EncodeToString(b), Tenant: tenant, Notify: func(string, any) {}}
			sessions[s.ID] = s
		}
		mu.Unlock()
		if s == nil {
			http.Error(w, "unknown or missing Mcp-Session-Id", http.StatusNotFound)
			return
		}
		if s.Tenant != tenant {
			http.Error(w, "session belongs to another tenant", http.StatusForbidden)
			return
		}
		w.Header().Set("Mcp-Session-Id", s.ID)
		if !m.IsRequest() {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		res, e := h.Handle(r.Context(), s, m.Method, m.Params)
		writeJSON(w, http.StatusOK, respond(m.ID, res, e))
	})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}
