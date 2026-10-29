package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"sync"
	"sync/atomic"
)

// Client speaks to one upstream MCP server over stdio. Safe for concurrent
// use: requests are multiplexed by id.
type Client struct {
	cmd     *exec.Cmd
	w       io.WriteCloser
	wmu     sync.Mutex
	next    atomic.Int64
	mu      sync.Mutex
	pending map[string]chan Message
	OnNote  func(Message)
	Info    InitializeResult
	done    chan struct{}
	err     error
}

func StartStdio(name string, args ...string) (*Client, error) {
	cmd := exec.Command(name, args...)
	in, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	c := NewClient(out, in)
	c.cmd = cmd
	return c, nil
}

// NewClient runs the protocol over any stream pair (pipes in tests).
func NewClient(r io.Reader, w io.WriteCloser) *Client {
	c := &Client{w: w, pending: map[string]chan Message{}, done: make(chan struct{})}
	go c.read(r)
	return c
}

func (c *Client) read(r io.Reader) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 1<<20), 64<<20)
	for sc.Scan() {
		var m Message
		if json.Unmarshal(sc.Bytes(), &m) != nil {
			continue
		}
		if m.Method != "" {
			if c.OnNote != nil && len(m.ID) == 0 {
				c.OnNote(m)
			}
			continue
		}
		c.mu.Lock()
		ch := c.pending[string(m.ID)]
		delete(c.pending, string(m.ID))
		c.mu.Unlock()
		if ch != nil {
			ch <- m
		}
	}
	c.err = sc.Err()
	if c.err == nil {
		c.err = io.EOF
	}
	close(c.done)
}

func (c *Client) send(m Message) error {
	b, err := json.Marshal(m)
	if err != nil {
		return err
	}
	c.wmu.Lock()
	defer c.wmu.Unlock()
	_, err = c.w.Write(append(b, '\n'))
	return err
}

// Call sends a request and decodes the result into out (may be nil).
func (c *Client) Call(ctx context.Context, method string, params, out any) error {
	id := strconv.FormatInt(c.next.Add(1), 10)
	var p json.RawMessage
	if params != nil {
		b, err := json.Marshal(params)
		if err != nil {
			return err
		}
		p = b
	}
	ch := make(chan Message, 1)
	c.mu.Lock()
	c.pending[id] = ch
	c.mu.Unlock()
	if err := c.send(Message{JSONRPC: "2.0", ID: json.RawMessage(id), Method: method, Params: p}); err != nil {
		return err
	}
	select {
	case m := <-ch:
		if m.Error != nil {
			return m.Error
		}
		if out != nil {
			return json.Unmarshal(m.Result, out)
		}
		return nil
	case <-c.done:
		return fmt.Errorf("mcp: connection closed: %v", c.err)
	case <-ctx.Done():
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
		return ctx.Err()
	}
}

func (c *Client) Notify(method string, params any) error {
	var p json.RawMessage
	if params != nil {
		p, _ = json.Marshal(params)
	}
	return c.send(Message{JSONRPC: "2.0", Method: method, Params: p})
}

func (c *Client) Initialize(ctx context.Context, name string) error {
	err := c.Call(ctx, "initialize", InitializeParams{ProtocolVersion: Versions[0], Capabilities: json.RawMessage(`{}`),
		ClientInfo: Implementation{Name: name, Version: "0.1"}}, &c.Info)
	if err != nil {
		return err
	}
	return c.Notify("notifications/initialized", nil)
}

// ListTools follows nextCursor until the server has listed everything.
func (c *Client) ListTools(ctx context.Context) ([]Tool, error) {
	var all []Tool
	cursor := ""
	for {
		var r ListToolsResult
		if err := c.Call(ctx, "tools/list", ListToolsParams{Cursor: cursor}, &r); err != nil {
			return nil, err
		}
		all = append(all, r.Tools...)
		if r.NextCursor == "" {
			return all, nil
		}
		cursor = r.NextCursor
	}
}

func (c *Client) CallTool(ctx context.Context, name string, args json.RawMessage) (CallToolResult, error) {
	var r CallToolResult
	err := c.Call(ctx, "tools/call", CallToolParams{Name: name, Arguments: args}, &r)
	return r, err
}

func (c *Client) Close() error {
	c.w.Close()
	if c.cmd != nil {
		return c.cmd.Wait()
	}
	return nil
}
