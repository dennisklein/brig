// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

// Package litellm manages the virtual keys of a LiteLLM proxy through its
// admin API, with the proxy's master key. The llm-proxy service gives each
// sandbox it is attached to one such key, named by an alias that says which
// gateway, workspace and sandbox holds it.
//
// The requests and responses follow LiteLLM's key management endpoints as of
// version 1.106.2: POST /key/generate, POST /key/delete and GET /key/list.
// Neither the master key nor a virtual key ever appears in an error.
package litellm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

// AliasPrefix starts the alias of every key that brig creates, so that brig
// can tell its own keys from those made in LiteLLM's UI.
const AliasPrefix = "brig/"

// Client talks to one LiteLLM proxy.
type Client struct {
	// BaseURL is the proxy's URL without a path, such as
	// http://127.0.0.1:4000.
	BaseURL string
	// MasterKey authorises the admin calls.
	MasterKey string
	// HTTP is the client to use; nil means one with a 30 second timeout.
	HTTP *http.Client
}

// KeyRequest describes a virtual key to create.
type KeyRequest struct {
	// Alias names the key; see Alias.
	Alias string `json:"key_alias"`
	// Models limits the key to these model names of the proxy's model
	// list; empty allows all of them.
	Models []string `json:"models,omitempty"`
	// MaxBudget is the most the key may spend, in US dollars; nil means no
	// limit.
	MaxBudget *float64 `json:"max_budget,omitempty"`
	// BudgetDuration resets the spend after this period, such as "30d".
	BudgetDuration string `json:"budget_duration,omitempty"`
	// Metadata is stored with the key and shown in LiteLLM's UI.
	Metadata map[string]string `json:"metadata,omitempty"`
}

// Owner is the sandbox that holds a key.
type Owner struct {
	Gateway, Workspace, Sandbox string
}

// aliasRE is what LiteLLM accepts as a key alias.
var aliasRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_/.@-]{0,253}[A-Za-z0-9]$`)

// partRE is what a gateway, workspace or sandbox name may look like in an
// alias: no slash, so that the alias splits back into its parts.
var partRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.@-]*$`)

// Alias returns the alias of o's key: brig/GATEWAY/WORKSPACE/SANDBOX.
func (o Owner) Alias() (string, error) {
	for _, p := range []string{o.Gateway, o.Workspace, o.Sandbox} {
		if !partRE.MatchString(p) {
			return "", fmt.Errorf("cannot name a LiteLLM key after %q", p)
		}
	}
	a := AliasPrefix + o.Gateway + "/" + o.Workspace + "/" + o.Sandbox
	if !aliasRE.MatchString(a) {
		return "", fmt.Errorf("LiteLLM key alias %q is not valid", a)
	}
	return a, nil
}

// Metadata returns the metadata that a key of o carries.
func (o Owner) Metadata() map[string]string {
	return map[string]string{"brig_gateway": o.Gateway, "brig_workspace": o.Workspace, "brig_sandbox": o.Sandbox}
}

// ParseAlias returns the owner that alias names, and whether it is an alias
// that brig made.
func ParseAlias(alias string) (Owner, bool) {
	rest, ok := strings.CutPrefix(alias, AliasPrefix)
	if !ok {
		return Owner{}, false
	}
	parts := strings.Split(rest, "/")
	if len(parts) != 3 || slices.ContainsFunc(parts, func(p string) bool { return !partRE.MatchString(p) }) {
		return Owner{}, false
	}
	return Owner{Gateway: parts[0], Workspace: parts[1], Sandbox: parts[2]}, true
}

// ErrNotFound means that no key matched.
var ErrNotFound = errors.New("no such LiteLLM key")

// GenerateKey creates a key and returns it.
func (c *Client) GenerateKey(ctx context.Context, req KeyRequest) (string, error) {
	if !aliasRE.MatchString(req.Alias) {
		return "", fmt.Errorf("LiteLLM key alias %q is not valid", req.Alias)
	}
	var resp struct {
		Key string `json:"key"`
	}
	if err := c.do(ctx, http.MethodPost, "/key/generate", nil, req, &resp); err != nil {
		return "", fmt.Errorf("create LiteLLM key %s: %w", req.Alias, err)
	}
	if resp.Key == "" {
		return "", fmt.Errorf("create LiteLLM key %s: the response holds no key", req.Alias)
	}
	return resp.Key, nil
}

// ReplaceKey deletes the key of req's alias, if there is one, and creates it
// anew. brig keeps no copy of a key, so this is how an attach that runs again
// gets a key to store.
func (c *Client) ReplaceKey(ctx context.Context, req KeyRequest) (string, error) {
	if err := c.DeleteKeys(ctx, req.Alias); err != nil && !errors.Is(err, ErrNotFound) {
		return "", err
	}
	return c.GenerateKey(ctx, req)
}

// DeleteKeys deletes the keys of these aliases. It returns ErrNotFound when
// none of them exists, which callers that revoke usually ignore.
func (c *Client) DeleteKeys(ctx context.Context, aliases ...string) error {
	if len(aliases) == 0 {
		return nil
	}
	body := struct {
		KeyAliases []string `json:"key_aliases"`
	}{aliases}
	err := c.do(ctx, http.MethodPost, "/key/delete", nil, body, nil)
	if se := (*StatusError)(nil); errors.As(err, &se) && se.Code == http.StatusNotFound {
		err = fmt.Errorf("%w: %s", ErrNotFound, se.Message)
	}
	if err != nil {
		return fmt.Errorf("delete LiteLLM keys %s: %w", strings.Join(aliases, ", "), err)
	}
	return nil
}

// pageSize is the most keys that /key/list returns at once.
const pageSize = 100

// maxPages bounds a listing, in case the proxy keeps reporting more pages.
const maxPages = 1000

// Aliases returns the sorted aliases of the keys that brig made.
func (c *Client) Aliases(ctx context.Context) ([]string, error) {
	var out []string
	for page := 1; page <= maxPages; page++ {
		q := url.Values{
			"key_alias":          {AliasPrefix},
			"substring_matching": {"true"},
			"return_full_object": {"true"},
			"page":               {strconv.Itoa(page)},
			"size":               {strconv.Itoa(pageSize)},
		}
		var resp struct {
			Keys []struct {
				Alias string `json:"key_alias"`
			} `json:"keys"`
			TotalPages int `json:"total_pages"`
		}
		if err := c.do(ctx, http.MethodGet, "/key/list", q, nil, &resp); err != nil {
			return nil, fmt.Errorf("list LiteLLM keys: %w", err)
		}
		for _, k := range resp.Keys {
			// substring_matching matches anywhere and ignores case.
			if _, ok := ParseAlias(k.Alias); ok {
				out = append(out, k.Alias)
			}
		}
		if page >= resp.TotalPages || len(resp.Keys) == 0 {
			slices.Sort(out)
			return slices.Compact(out), nil
		}
	}
	return nil, fmt.Errorf("list LiteLLM keys: more than %d pages", maxPages)
}

// StatusError is an answer of LiteLLM's other than 2xx.
type StatusError struct {
	Code int
	// Message is LiteLLM's message, without keys.
	Message string
}

func (e *StatusError) Error() string {
	if e.Code == http.StatusUnauthorized || e.Code == http.StatusForbidden {
		return fmt.Sprintf("LiteLLM refused the master key (%d %s): %s", e.Code, http.StatusText(e.Code), e.Message)
	}
	return fmt.Sprintf("LiteLLM answered %d %s: %s", e.Code, http.StatusText(e.Code), e.Message)
}

// maxResponse bounds what brig reads of a response.
const maxResponse = 4 << 20

func (c *Client) do(ctx context.Context, method, path string, query url.Values, in, out any) error {
	u, err := url.Parse(c.BaseURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return fmt.Errorf("invalid LiteLLM URL %q", c.BaseURL)
	}
	u = u.JoinPath(path)
	u.RawQuery = query.Encode()
	var body io.Reader
	if in != nil {
		data, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, u.String(), body)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.MasterKey)
	req.Header.Set("Accept", "application/json")
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	hc := c.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: 30 * time.Second}
	}
	resp, err := hc.Do(req)
	if err != nil {
		// The URL holds no secret; the error may quote it.
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxResponse))
	if err != nil {
		return err
	}
	if resp.StatusCode/100 != 2 {
		return &StatusError{Code: resp.StatusCode, Message: c.redact(errorMessage(data))}
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("read LiteLLM's answer: %w", err)
	}
	return nil
}

// redact removes the master key from msg, in case LiteLLM quotes it.
func (c *Client) redact(msg string) string {
	if c.MasterKey == "" {
		return msg
	}
	return strings.ReplaceAll(msg, c.MasterKey, "[master key]")
}

// virtualKeyRE matches LiteLLM's virtual keys, so that an error never
// repeats one.
var virtualKeyRE = regexp.MustCompile(`sk-[A-Za-z0-9_-]{8,}`)

// errorMessage takes the message out of a LiteLLM error body. LiteLLM
// answers {"error": {"message": ...}} for its own exceptions and
// {"detail": ...} for FastAPI's.
func errorMessage(data []byte) string {
	var body struct {
		Error  json.RawMessage `json:"error"`
		Detail json.RawMessage `json:"detail"`
	}
	msg := ""
	if json.Unmarshal(data, &body) == nil {
		for _, raw := range []json.RawMessage{body.Error, body.Detail} {
			if msg = textOf(raw); msg != "" {
				break
			}
		}
	}
	if msg == "" {
		msg = strings.TrimSpace(string(data))
	}
	msg = virtualKeyRE.ReplaceAllString(msg, "[key]")
	if len(msg) > 300 {
		msg = msg[:300] + "..."
	}
	if msg == "" {
		msg = "no message"
	}
	return msg
}

// textOf returns raw if it is a string, or else its "message" or "error"
// member.
func textOf(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var m struct {
		Message string          `json:"message"`
		Error   json.RawMessage `json:"error"`
	}
	if json.Unmarshal(raw, &m) == nil {
		if m.Message != "" {
			return m.Message
		}
		return textOf(m.Error)
	}
	return ""
}
