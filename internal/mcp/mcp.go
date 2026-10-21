// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

// Package mcp serves tools over the Model Context Protocol's streamable
// HTTP transport, in the smallest form the protocol allows: each JSON-RPC
// request is one POST answered with one JSON response. The server keeps no
// sessions, sends no notifications and opens no event streams, so a GET
// gets 405. It implements initialize, ping, tools/list and tools/call.
//
// Agents call these servers, so everything a request carries is untrusted:
// the server bounds the body, checks the bearer token through Auth before it
// reads the body, refuses cross-origin browser requests and answers unknown
// methods with an error.
package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"slices"
	"strings"
)

// Versions are the protocol versions the server speaks, newest first.
var Versions = []string{"2025-11-25", "2025-06-18", "2025-03-26"}

// Tool is a tool the server offers.
type Tool struct {
	Name        string
	Title       string
	Description string
	// InputSchema is the JSON Schema of the tool's arguments.
	InputSchema json.RawMessage
	// ReadOnly and Destructive are hints for clients.
	ReadOnly, Destructive bool
	// Call runs the tool. An error becomes a tool result with isError
	// set, which the model sees, not a protocol error.
	Call func(ctx context.Context, args json.RawMessage) (*Result, error)
}

// Result is a tool's result.
type Result struct {
	Content []Content `json:"content"`
	// Structured is the result as JSON, for clients that read it.
	Structured any  `json:"structuredContent,omitempty"`
	IsError    bool `json:"isError,omitempty"`
}

// Content is one block of a tool result: text, or an image as base64.
type Content struct {
	Type     string `json:"type"`
	Text     string `json:"text,omitempty"`
	Data     string `json:"data,omitempty"`
	MIMEType string `json:"mimeType,omitempty"`
}

// Text returns a result holding s.
func Text(s string) *Result { return &Result{Content: []Content{{Type: "text", Text: s}}} }

// JSON returns a result holding v both as text and as structured content.
func JSON(v any) (*Result, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	return &Result{Content: []Content{{Type: "text", Text: string(b)}}, Structured: v}, nil
}

// Server serves tools to authenticated callers.
type Server struct {
	Name, Version string
	// Instructions tell the model how to use the tools.
	Instructions string
	Tools        []Tool
	// Auth checks a request's credentials and returns the context the
	// tool calls run with, which carries the caller. A request it fails
	// gets 401 and is not read further.
	Auth func(r *http.Request) (context.Context, error)
	// MaxBody bounds a request's body in bytes.
	MaxBody int64
	// Log receives one line per failed request; nil discards them.
	Log *slog.Logger
}

// JSON-RPC error codes.
const (
	codeParse          = -32700
	codeInvalidRequest = -32600
	codeMethodNotFound = -32601
	codeInvalidParams  = -32602
	codeInternal       = -32603
)

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

func (s *Server) logf(format string, args ...any) {
	if s.Log != nil {
		s.Log.Warn(fmt.Sprintf(format, args...))
	}
}

// ServeHTTP handles one HTTP request to the MCP endpoint.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// A browser page must not drive the tools: browsers send Origin on
	// cross-origin POSTs, while MCP clients send none.
	if r.Header.Get("Origin") != "" {
		http.Error(w, "cross-origin requests are not allowed", http.StatusForbidden)
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "this server opens no event streams; POST JSON-RPC requests", http.StatusMethodNotAllowed)
		return
	}
	ctx, err := s.Auth(r)
	if err != nil {
		s.logf("mcp: %s: %v", r.RemoteAddr, err)
		w.Header().Set("WWW-Authenticate", `Bearer realm="brig"`)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if v := r.Header.Get("MCP-Protocol-Version"); v != "" && !slices.Contains(Versions, v) {
		http.Error(w, "unsupported MCP protocol version "+v, http.StatusBadRequest)
		return
	}
	if ct := r.Header.Get("Content-Type"); ct != "" && !strings.HasPrefix(ct, "application/json") {
		http.Error(w, "want Content-Type application/json", http.StatusUnsupportedMediaType)
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, s.MaxBody))
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			http.Error(w, fmt.Sprintf("request larger than %d bytes", s.MaxBody), http.StatusRequestEntityTooLarge)
			return
		}
		http.Error(w, "cannot read request", http.StatusBadRequest)
		return
	}
	var req rpcRequest
	if err := json.Unmarshal(body, &req); err != nil {
		code := codeParse
		if t := bytes.TrimSpace(body); len(t) > 0 && t[0] == '[' {
			code = codeInvalidRequest // batches are gone since 2025-06-18
		}
		s.reply(w, rpcResponse{ID: json.RawMessage("null"), Error: &rpcError{Code: code, Message: "invalid JSON-RPC request"}})
		return
	}
	if req.JSONRPC != "2.0" {
		s.reply(w, rpcResponse{ID: json.RawMessage("null"), Error: &rpcError{Code: codeInvalidRequest, Message: `want "jsonrpc": "2.0"`}})
		return
	}
	if req.Method == "" {
		// A response from the client to a request of ours: we send none.
		w.WriteHeader(http.StatusAccepted)
		return
	}
	if len(req.ID) == 0 || string(req.ID) == "null" {
		// Notifications, such as notifications/initialized, need no answer.
		w.WriteHeader(http.StatusAccepted)
		return
	}
	resp := rpcResponse{ID: req.ID}
	resp.Result, resp.Error = s.handle(ctx, req)
	if resp.Error != nil {
		s.logf("mcp: %s: %s: %s", r.RemoteAddr, req.Method, resp.Error.Message)
	}
	s.reply(w, resp)
}

func (s *Server) reply(w http.ResponseWriter, resp rpcResponse) {
	resp.JSONRPC = "2.0"
	b, err := json.Marshal(resp)
	if err != nil {
		b, _ = json.Marshal(rpcResponse{JSONRPC: "2.0", ID: resp.ID, Error: &rpcError{Code: codeInternal, Message: err.Error()}})
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(b)
}

func (s *Server) handle(ctx context.Context, req rpcRequest) (any, *rpcError) {
	switch req.Method {
	case "initialize":
		var p struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		if err := json.Unmarshal(orEmpty(req.Params), &p); err != nil {
			return nil, &rpcError{Code: codeInvalidParams, Message: err.Error()}
		}
		version := Versions[0]
		if slices.Contains(Versions, p.ProtocolVersion) {
			version = p.ProtocolVersion
		}
		return map[string]any{
			"protocolVersion": version,
			"capabilities":    map[string]any{"tools": map[string]bool{"listChanged": false}},
			"serverInfo":      map[string]string{"name": s.Name, "version": s.Version},
			"instructions":    s.Instructions,
		}, nil
	case "ping":
		return struct{}{}, nil
	case "tools/list":
		return map[string]any{"tools": s.list()}, nil
	case "tools/call":
		var p struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		}
		if err := json.Unmarshal(orEmpty(req.Params), &p); err != nil {
			return nil, &rpcError{Code: codeInvalidParams, Message: err.Error()}
		}
		i := slices.IndexFunc(s.Tools, func(t Tool) bool { return t.Name == p.Name })
		if i < 0 {
			return nil, &rpcError{Code: codeInvalidParams, Message: fmt.Sprintf("unknown tool %q", p.Name)}
		}
		res, err := s.Tools[i].Call(ctx, orEmpty(p.Arguments))
		if err != nil {
			return &Result{Content: []Content{{Type: "text", Text: err.Error()}}, IsError: true}, nil
		}
		return res, nil
	}
	return nil, &rpcError{Code: codeMethodNotFound, Message: fmt.Sprintf("method %q not found", req.Method)}
}

func orEmpty(m json.RawMessage) json.RawMessage {
	if len(m) == 0 || string(m) == "null" {
		return json.RawMessage("{}")
	}
	return m
}

type toolInfo struct {
	Name        string          `json:"name"`
	Title       string          `json:"title,omitempty"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"inputSchema"`
	Annotations map[string]any  `json:"annotations,omitempty"`
}

func (s *Server) list() []toolInfo {
	tools := make([]toolInfo, len(s.Tools))
	for i, t := range s.Tools {
		tools[i] = toolInfo{
			Name: t.Name, Title: t.Title, Description: t.Description, InputSchema: t.InputSchema,
			Annotations: map[string]any{"readOnlyHint": t.ReadOnly, "destructiveHint": t.Destructive, "openWorldHint": false},
		}
	}
	return tools
}

// Decode decodes a tool's arguments strictly: unknown fields and trailing
// data are errors, so a misspelt argument is not silently ignored.
func Decode(args json.RawMessage, v any) error {
	dec := json.NewDecoder(bytes.NewReader(args))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return fmt.Errorf("invalid arguments: %w", err)
	}
	if dec.More() {
		return errors.New("invalid arguments: trailing data")
	}
	return nil
}
