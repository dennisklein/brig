// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

package litellm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
)

const master = "sk-master-0123456789"

// fake is a LiteLLM proxy with only the key endpoints, which answers as
// version 1.106.2 does.
type fake struct {
	mu   sync.Mutex
	keys map[string]string // alias to key
	next int
	reqs []string
}

func newFake(t *testing.T) (*fake, *Client) {
	f := &fake{keys: map[string]string{}}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	return f, &Client{BaseURL: srv.URL, MasterKey: master}
}

func (f *fake) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reqs = append(f.reqs, r.Method+" "+r.URL.Path)
	if r.Header.Get("Authorization") != "Bearer "+master {
		w.WriteHeader(http.StatusUnauthorized)
		fmt.Fprintf(w, `{"error":{"message":"Authentication Error, Invalid proxy server token passed. Received API Key = %s","type":"auth_error"}}`, r.Header.Get("Authorization")) //nolint:gosec // G705: echoes the key, as LiteLLM does
		return
	}
	switch r.Method + " " + r.URL.Path {
	case "POST /key/generate":
		var req KeyRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if _, ok := f.keys[req.Alias]; ok {
			w.WriteHeader(http.StatusBadRequest)
			fmt.Fprintf(w, `{"error":{"message":"Key with alias '%s' already exists. Unique key aliases across all keys are required."}}`, req.Alias)
			return
		}
		f.next++
		key := "sk-virtual-" + strconv.Itoa(f.next) + "abcdefgh"
		f.keys[req.Alias] = key
		_ = json.NewEncoder(w).Encode(map[string]any{"key": key, "key_alias": req.Alias, "models": req.Models})
	case "POST /key/delete":
		var req struct {
			KeyAliases []string `json:"key_aliases"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		n := 0
		for _, a := range req.KeyAliases {
			if _, ok := f.keys[a]; ok {
				delete(f.keys, a)
				n++
			}
		}
		if n == 0 {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"detail":{"error":"No keys found"}}`))
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"deleted_keys": req.KeyAliases})
	case "GET /key/list":
		q := r.URL.Query()
		sub := strings.ToLower(q.Get("key_alias"))
		page, _ := strconv.Atoi(q.Get("page"))
		size, _ := strconv.Atoi(q.Get("size"))
		var all []string
		for a := range f.keys {
			if strings.Contains(strings.ToLower(a), sub) {
				all = append(all, a)
			}
		}
		slices.Sort(all)
		pages := (len(all) + size - 1) / size
		var keys []map[string]string
		for i := (page - 1) * size; i < len(all) && i < page*size; i++ {
			keys = append(keys, map[string]string{"key_alias": all[i], "token": "hashed"})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"keys": keys, "total_count": len(all), "current_page": page, "total_pages": pages})
	default:
		http.NotFound(w, r)
	}
}

func TestOwnerAlias(t *testing.T) {
	o := Owner{Gateway: "dev", Workspace: "default", Sandbox: "webapp"}
	a, err := o.Alias()
	if err != nil || a != "brig/dev/default/webapp" {
		t.Fatalf("Alias() = %q, %v", a, err)
	}
	if got, ok := ParseAlias(a); !ok || got != o {
		t.Fatalf("ParseAlias(%q) = %+v, %v", a, got, ok)
	}
	for _, bad := range []Owner{
		{Gateway: "dev", Workspace: "a/b", Sandbox: "x"},
		{Gateway: "", Workspace: "default", Sandbox: "x"},
		{Gateway: "dev", Workspace: "default", Sandbox: "x y"},
		{Gateway: "dev", Workspace: "default", Sandbox: "x-"},
	} {
		if a, err := bad.Alias(); err == nil {
			t.Errorf("Alias() of %+v = %q, want an error", bad, a)
		}
	}
	for _, a := range []string{"other/dev/default/x", "brig/dev/x", "brig/dev/default/x/y", "brig//default/x"} {
		if _, ok := ParseAlias(a); ok {
			t.Errorf("ParseAlias(%q) took it as brig's", a)
		}
	}
}

func TestGenerateReplaceDelete(t *testing.T) {
	f, c := newFake(t)
	ctx := context.Background()
	budget := 5.0
	req := KeyRequest{
		Alias: "brig/dev/default/webapp", Models: []string{"claude"}, MaxBudget: &budget, BudgetDuration: "30d",
		Metadata: Owner{"dev", "default", "webapp"}.Metadata(),
	}
	k1, err := c.GenerateKey(ctx, req)
	if err != nil || !strings.HasPrefix(k1, "sk-virtual-") {
		t.Fatalf("GenerateKey = %q, %v", k1, err)
	}
	// A second create of the same alias fails as LiteLLM's does; Replace
	// deletes it first.
	_, err = c.GenerateKey(ctx, req)
	if err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("GenerateKey again: %v", err)
	}
	k2, err := c.ReplaceKey(ctx, req)
	if err != nil || k2 == k1 {
		t.Fatalf("ReplaceKey = %q, %v", k2, err)
	}
	// Replace of a missing key just creates it.
	other := req
	other.Alias = "brig/dev/default/other"
	if _, err := c.ReplaceKey(ctx, other); err != nil {
		t.Fatal(err)
	}
	if err := c.DeleteKeys(ctx, req.Alias, other.Alias); err != nil {
		t.Fatal(err)
	}
	if len(f.keys) != 0 {
		t.Fatalf("keys left: %v", f.keys)
	}
	if err := c.DeleteKeys(ctx, req.Alias); !errors.Is(err, ErrNotFound) {
		t.Fatalf("DeleteKeys of a gone key: %v, want ErrNotFound", err)
	}
	if err := c.DeleteKeys(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestAliasesPagesAndFilters(t *testing.T) {
	f, c := newFake(t)
	var want []string
	for i := range 250 {
		a := fmt.Sprintf("brig/dev/default/s%03d", i)
		f.keys[a] = "k"
		want = append(want, a)
	}
	// Made in LiteLLM's UI, or matching only as a substring.
	f.keys["alice"] = "k"
	f.keys["x-brig/dev/default/y"] = "k"
	f.keys["BRIG/dev/default/y"] = "k"
	got, err := c.Aliases(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(got, want) {
		t.Fatalf("Aliases() = %d aliases, want %d", len(got), len(want))
	}
	if n := slices.Index(f.reqs, "GET /key/list"); n < 0 || len(f.reqs) != 3 {
		t.Fatalf("requests = %v, want three pages", f.reqs)
	}
}

func TestErrorsHideKeys(t *testing.T) {
	_, c := newFake(t)
	c.MasterKey = "sk-wrong-master-key-123"
	_, err := c.GenerateKey(context.Background(), KeyRequest{Alias: "brig/dev/default/x"})
	var se *StatusError
	if !errors.As(err, &se) || se.Code != http.StatusUnauthorized {
		t.Fatalf("err = %v, want a 401 StatusError", err)
	}
	if strings.Contains(err.Error(), c.MasterKey) || strings.Contains(err.Error(), "sk-wrong") {
		t.Fatalf("error repeats the master key: %v", err)
	}
	if !strings.Contains(err.Error(), "refused the master key") {
		t.Fatalf("err = %v", err)
	}
}

func TestErrorMessage(t *testing.T) {
	for _, tc := range []struct{ body, want string }{
		{`{"error":{"message":"bad alias"}}`, "bad alias"},
		{`{"detail":{"error":"No keys found"}}`, "No keys found"},
		{`{"detail":"Not Found"}`, "Not Found"},
		{`Internal Server Error`, "Internal Server Error"},
		{``, "no message"},
		{`{"error":{"message":"key sk-abcdefghijkl is blocked"}}`, "key [key] is blocked"},
	} {
		if got := errorMessage([]byte(tc.body)); got != tc.want {
			t.Errorf("errorMessage(%s) = %q, want %q", tc.body, got, tc.want)
		}
	}
}

func TestInvalidInput(t *testing.T) {
	c := &Client{BaseURL: "file:///x", MasterKey: master}
	if _, err := c.GenerateKey(context.Background(), KeyRequest{Alias: "brig/dev/default/x"}); err == nil || !strings.Contains(err.Error(), "invalid LiteLLM URL") {
		t.Fatalf("err = %v", err)
	}
	c.BaseURL = "http://127.0.0.1:1"
	if _, err := c.GenerateKey(context.Background(), KeyRequest{Alias: "bad alias"}); err == nil || !strings.Contains(err.Error(), "not valid") {
		t.Fatalf("err = %v", err)
	}
}
