package mcp

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Session is one connected client. Notify sends a server-to-client
// notification when the transport can carry one (stdio can; the HTTP JSON
// response mode cannot, so there it is a no-op and clients re-list).
type Session struct {
	ID     string
	Tenant string
	Notify func(method string, params any)
	State  any

	lastUsed time.Time // HTTP only: for idle expiry
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
		_ = enc.Encode(m) // a broken stdout ends the session when the reader sees EOF
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

// HTTPOptions bound the sessions an HTTP server keeps. Clients are not
// obliged to DELETE their session, so without a bound every initialize
// would stay in memory for the life of the process.
type HTTPOptions struct {
	IdleTimeout time.Duration // a session unused this long is closed (0 = never)
	MaxSessions int           // initialize beyond this many live sessions gets 503 (0 = no limit)
	now         func() time.Time
}

// DefaultHTTPOptions closes sessions after 30 idle minutes and keeps at most
// 10,000.
var DefaultHTTPOptions = HTTPOptions{IdleTimeout: 30 * time.Minute, MaxSessions: MaxSessions}

// HTTPHandler serves Streamable HTTP in JSON response mode: each POST
// carries one JSON-RPC message and gets one JSON response. Sessions are
// identified by the Mcp-Session-Id header issued at initialize. auth maps a
// bearer token to a tenant; nil means no authentication. Sessions are
// bounded by DefaultHTTPOptions.
func HTTPHandler(h Handler, auth func(token string) (string, bool)) http.Handler {
	return NewHTTPHandler(h, auth, DefaultHTTPOptions)
}

// MaxSessions is the default cap on concurrent HTTP sessions.
const MaxSessions = 10000

// maxBody bounds a single JSON-RPC request over HTTP.
const maxBody = 16 << 20

// HTTPHandlerLimit is HTTPHandler with an explicit cap on live sessions
// (idle expiry stays at the default); initialize beyond the cap gets 503.
func HTTPHandlerLimit(h Handler, auth func(token string) (string, bool), maxSessions int) http.Handler {
	o := DefaultHTTPOptions
	o.MaxSessions = maxSessions
	return NewHTTPHandler(h, auth, o)
}

// NewHTTPHandler is HTTPHandler with explicit session bounds.
func NewHTTPHandler(h Handler, auth func(token string) (string, bool), o HTTPOptions) http.Handler {
	if o.now == nil {
		o.now = time.Now
	}
	var mu sync.Mutex
	sessions := map[string]*Session{}
	var lastSweep time.Time
	// expire closes idle sessions; called with mu held, at most once per
	// tenth of the idle timeout so a busy server does not scan on every call.
	expire := func(now time.Time) []*Session {
		if o.IdleTimeout <= 0 || now.Sub(lastSweep) < o.IdleTimeout/10 {
			return nil
		}
		lastSweep = now
		var gone []*Session
		for id, s := range sessions {
			if now.Sub(s.lastUsed) >= o.IdleTimeout {
				delete(sessions, id)
				gone = append(gone, s)
			}
		}
		return gone
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		now := o.now()
		mu.Lock()
		gone := expire(now)
		mu.Unlock()
		for _, s := range gone {
			h.Closed(s)
		}
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
			s := sessions[r.Header.Get("Mcp-Session-Id")]
			if s != nil && s.Tenant != tenant {
				// one tenant must not be able to end another tenant's session
				mu.Unlock()
				http.Error(w, "session belongs to another tenant", http.StatusForbidden)
				return
			}
			if s != nil {
				delete(sessions, s.ID)
			}
			mu.Unlock()
			if s == nil {
				http.Error(w, "unknown or missing Mcp-Session-Id", http.StatusNotFound)
				return
			}
			h.Closed(s)
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
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBody)).Decode(&m); err != nil {
			code := http.StatusBadRequest
			var mbe *http.MaxBytesError
			if errors.As(err, &mbe) {
				code = http.StatusRequestEntityTooLarge
			}
			writeJSON(w, code, Message{JSONRPC: "2.0", ID: json.RawMessage("null"), Error: &Error{ParseError, err.Error()}})
			return
		}
		sid := r.Header.Get("Mcp-Session-Id")
		var s *Session
		if m.Method == "initialize" {
			b := make([]byte, 16)
			if _, err := rand.Read(b); err != nil {
				http.Error(w, "session id: "+err.Error(), http.StatusInternalServerError)
				return
			}
			s = &Session{ID: hex.EncodeToString(b), Tenant: tenant, Notify: func(string, any) {}, lastUsed: now}
			mu.Lock()
			full := o.MaxSessions > 0 && len(sessions) >= o.MaxSessions
			if !full {
				sessions[s.ID] = s
			}
			mu.Unlock()
			if full {
				http.Error(w, "too many sessions", http.StatusServiceUnavailable)
				return
			}
		} else {
			mu.Lock()
			s = sessions[sid]
			if s != nil && s.Tenant == tenant {
				s.lastUsed = now
			}
			mu.Unlock()
		}
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
	_ = json.NewEncoder(w).Encode(v)
}
