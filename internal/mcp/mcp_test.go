// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type callerKey struct{}

func testServer() *Server {
	return &Server{
		Name: "test", Version: "1",
		Instructions: "use the tools",
		MaxBody:      1024,
		Auth: func(r *http.Request) (context.Context, error) {
			if r.Header.Get("Authorization") != "Bearer good" {
				return nil, errors.New("bad token")
			}
			return context.WithValue(r.Context(), callerKey{}, "sandbox-a"), nil
		},
		Tools: []Tool{
			{
				Name: "whoami", Description: "says who calls", ReadOnly: true,
				InputSchema: json.RawMessage(`{"type":"object"}`),
				Call: func(ctx context.Context, _ json.RawMessage) (*Result, error) {
					return Text(ctx.Value(callerKey{}).(string)), nil
				},
			},
			{
				Name: "fail", Description: "fails", Destructive: true,
				InputSchema: json.RawMessage(`{"type":"object"}`),
				Call: func(context.Context, json.RawMessage) (*Result, error) {
					return nil, errors.New("it broke")
				},
			},
		},
	}
}

func post(t *testing.T, s *Server, body string, header map[string]string) (int, string) {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(body))
	r.Header.Set("Authorization", "Bearer good")
	r.Header.Set("Content-Type", "application/json")
	for k, v := range header {
		if v == "" {
			r.Header.Del(k)
		} else {
			r.Header.Set(k, v)
		}
	}
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	b, _ := io.ReadAll(w.Result().Body)
	return w.Code, string(b)
}

func TestInitialize(t *testing.T) {
	s := testServer()
	code, body := post(t, s, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"c","version":"1"}}}`, nil)
	if code != 200 {
		t.Fatalf("code %d: %s", code, body)
	}
	var resp struct {
		ID     int `json:"id"`
		Result struct {
			ProtocolVersion string         `json:"protocolVersion"`
			Capabilities    map[string]any `json:"capabilities"`
			ServerInfo      map[string]any `json:"serverInfo"`
			Instructions    string         `json:"instructions"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.ID != 1 || resp.Result.ProtocolVersion != "2025-06-18" || resp.Result.Capabilities["tools"] == nil ||
		resp.Result.ServerInfo["name"] != "test" || resp.Result.Instructions != "use the tools" {
		t.Errorf("initialize = %s", body)
	}
	// An unknown version gets the newest.
	_, body = post(t, s, `{"jsonrpc":"2.0","id":"x","method":"initialize","params":{"protocolVersion":"1999-01-01"}}`, nil)
	if !strings.Contains(body, `"protocolVersion":"`+Versions[0]+`"`) || !strings.Contains(body, `"id":"x"`) {
		t.Errorf("initialize with unknown version = %s", body)
	}
}

func TestNotificationsAndResponses(t *testing.T) {
	s := testServer()
	for _, body := range []string{
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","id":5,"result":{}}`,
	} {
		if code, out := post(t, s, body, nil); code != http.StatusAccepted || out != "" {
			t.Errorf("%s: code %d, body %q", body, code, out)
		}
	}
}

func TestToolsListAndCall(t *testing.T) {
	s := testServer()
	_, body := post(t, s, `{"jsonrpc":"2.0","id":2,"method":"tools/list"}`, nil)
	for _, want := range []string{`"name":"whoami"`, `"readOnlyHint":true`, `"name":"fail"`, `"destructiveHint":true`, `"inputSchema":{"type":"object"}`} {
		if !strings.Contains(body, want) {
			t.Errorf("tools/list lacks %s: %s", want, body)
		}
	}
	_, body = post(t, s, `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"whoami","arguments":{}}}`, nil)
	if !strings.Contains(body, `"content":[{"type":"text","text":"sandbox-a"}]`) || strings.Contains(body, "isError") {
		t.Errorf("whoami = %s", body)
	}
	_, body = post(t, s, `{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"fail"}}`, nil)
	if !strings.Contains(body, `"text":"it broke"`) || !strings.Contains(body, `"isError":true`) {
		t.Errorf("fail = %s", body)
	}
	_, body = post(t, s, `{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"nope"}}`, nil)
	if !strings.Contains(body, `"code":-32602`) {
		t.Errorf("unknown tool = %s", body)
	}
}

func TestProtocolErrors(t *testing.T) {
	s := testServer()
	for body, want := range map[string]string{
		`{"jsonrpc":"2.0","id":1,"method":"resources/list"}`: `"code":-32601`,
		`not json`: `"code":-32700`,
		`[{"jsonrpc":"2.0","id":1,"method":"ping"}]`:                `"code":-32600`,
		`{"jsonrpc":"1.0","id":1,"method":"ping"}`:                  `"code":-32600`,
		`{"jsonrpc":"2.0","id":1,"method":"ping"}`:                  `"result":{}`,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":7}`: `"code":-32602`,
	} {
		if _, got := post(t, s, body, nil); !strings.Contains(got, want) {
			t.Errorf("%s: got %s, want %s", body, got, want)
		}
	}
}

func TestHTTPChecks(t *testing.T) {
	s := testServer()
	ping := `{"jsonrpc":"2.0","id":1,"method":"ping"}`
	for name, tc := range map[string]struct {
		body   string
		header map[string]string
		code   int
	}{
		"no token":     {ping, map[string]string{"Authorization": ""}, http.StatusUnauthorized},
		"bad token":    {ping, map[string]string{"Authorization": "Bearer bad"}, http.StatusUnauthorized},
		"origin":       {ping, map[string]string{"Origin": "http://evil.example"}, http.StatusForbidden},
		"version":      {ping, map[string]string{"MCP-Protocol-Version": "1999-01-01"}, http.StatusBadRequest},
		"content type": {ping, map[string]string{"Content-Type": "text/plain"}, http.StatusUnsupportedMediaType},
		"too large":    {`{"jsonrpc":"2.0","id":1,"method":"ping","x":"` + strings.Repeat("a", 2000) + `"}`, nil, http.StatusRequestEntityTooLarge},
		"good version": {ping, map[string]string{"MCP-Protocol-Version": "2025-06-18"}, http.StatusOK},
	} {
		if code, body := post(t, s, tc.body, tc.header); code != tc.code {
			t.Errorf("%s: code %d, want %d: %s", name, code, tc.code, body)
		}
	}
	w := httptest.NewRecorder()
	s.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/mcp", nil))
	if w.Code != http.StatusMethodNotAllowed || w.Header().Get("Allow") != "POST" {
		t.Errorf("GET: code %d, Allow %q", w.Code, w.Header().Get("Allow"))
	}
}

func TestDecode(t *testing.T) {
	var v struct {
		VM string `json:"vm"`
	}
	if err := Decode(json.RawMessage(`{"vm":"a"}`), &v); err != nil || v.VM != "a" {
		t.Errorf("Decode = %v, %+v", err, v)
	}
	for _, bad := range []string{`{"vm":"a","x":1}`, `{"vm":1}`, `{"vm":"a"} {}`, `[]`} {
		if err := Decode(json.RawMessage(bad), &v); err == nil {
			t.Errorf("Decode(%s) succeeded", bad)
		}
	}
}

func FuzzServeHTTP(f *testing.F) {
	f.Add(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"whoami","arguments":{}}}`)
	f.Add(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18"}}`)
	f.Add(`[1,2]`)
	s := testServer()
	f.Fuzz(func(t *testing.T, body string) {
		r := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(body))
		r.Header.Set("Authorization", "Bearer good")
		w := httptest.NewRecorder()
		s.ServeHTTP(w, r)
		if w.Code == http.StatusOK && !json.Valid(w.Body.Bytes()) {
			t.Errorf("invalid JSON response %q", w.Body.String())
		}
	})
}
