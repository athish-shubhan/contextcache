// Package mcp implements the parts of the Model Context Protocol a tool
// gateway needs: JSON-RPC 2.0 messages, the initialize handshake, tools/list
// (with pagination), tools/call, ping, and notifications/tools/list_changed,
// over stdio (newline-delimited JSON) and over Streamable HTTP in its JSON
// response mode. Written from the specification, no SDK.
package mcp

import (
	"encoding/json"
)

// Versions this implementation speaks, newest first. The server answers
// with the client's version when it is one of these.
var Versions = []string{"2025-11-25", "2025-06-18", "2025-03-26", "2024-11-05"}

type Message struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *Error          `json:"error,omitempty"`
}

func (m *Message) IsRequest() bool      { return m.Method != "" && len(m.ID) > 0 }
func (m *Message) IsNotification() bool { return m.Method != "" && len(m.ID) == 0 }

type Error struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e *Error) Error() string { return e.Message }

const (
	ParseError     = -32700
	InvalidRequest = -32600
	MethodNotFound = -32601
	InvalidParams  = -32602
	InternalError  = -32603
)

type Implementation struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

type InitializeParams struct {
	ProtocolVersion string          `json:"protocolVersion"`
	Capabilities    json.RawMessage `json:"capabilities"`
	ClientInfo      Implementation  `json:"clientInfo"`
}

type InitializeResult struct {
	ProtocolVersion string         `json:"protocolVersion"`
	Capabilities    map[string]any `json:"capabilities"`
	ServerInfo      Implementation `json:"serverInfo"`
	Instructions    string         `json:"instructions,omitempty"`
}

type Tool struct {
	Name        string          `json:"name"`
	Title       string          `json:"title,omitempty"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"inputSchema"`
}

type ListToolsParams struct {
	Cursor string `json:"cursor,omitempty"`
}

type ListToolsResult struct {
	Tools      []Tool `json:"tools"`
	NextCursor string `json:"nextCursor,omitempty"`
}

type CallToolParams struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments,omitempty"`
}

type Content struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`
}

type CallToolResult struct {
	Content           []Content       `json:"content"`
	StructuredContent json.RawMessage `json:"structuredContent,omitempty"`
	IsError           bool            `json:"isError,omitempty"`
}

func TextResult(s string, isErr bool) CallToolResult {
	return CallToolResult{Content: []Content{{Type: "text", Text: s}}, IsError: isErr}
}

func Negotiate(requested string) string {
	for _, v := range Versions {
		if v == requested {
			return v
		}
	}
	return Versions[0]
}
