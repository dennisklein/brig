// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

package vmrunner

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"strings"
	"sync"

	"github.com/dennisklein/brig/internal/service"
)

// Owner is the sandbox that a token belongs to, and so the owner of the VMs
// created with it.
type Owner struct {
	Gateway   string `json:"gateway"`
	Workspace string `json:"workspace"`
	Sandbox   string `json:"sandbox"`
}

func (o Owner) String() string { return o.Gateway + "/" + o.Workspace + "/" + o.Sandbox }

// Tokens reads the token file of the service and maps tokens to owners. It
// reads the file again whenever it changes, so attaching and detaching
// sandboxes need no restart.
type Tokens struct {
	path string

	mu sync.Mutex
	// fi is the file as last loaded. brig replaces the file by renaming a
	// new one over it, which a new inode tells as surely as a new
	// modification time.
	fi     os.FileInfo
	owners map[string]Owner // by hex SHA-256
}

// NewTokens returns the tokens of the file at path, which need not exist.
func NewTokens(path string) *Tokens { return &Tokens{path: path} }

// load reads the file if it changed since the last load. A missing file
// has no tokens. A file that cannot be read keeps the old tokens out: no
// caller is accepted until it is fixed.
func (t *Tokens) load() (map[string]Owner, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	fi, err := os.Stat(t.path)
	if errors.Is(err, fs.ErrNotExist) {
		t.owners, t.fi = map[string]Owner{}, nil
		return t.owners, nil
	}
	if err != nil {
		t.owners = nil
		return nil, err
	}
	if t.owners != nil && t.fi != nil && os.SameFile(fi, t.fi) && fi.ModTime().Equal(t.fi.ModTime()) && fi.Size() == t.fi.Size() {
		return t.owners, nil
	}
	owners, err := readTokens(t.path)
	if err != nil {
		t.owners = nil
		return nil, err
	}
	t.owners, t.fi = owners, fi
	return owners, nil
}

func readTokens(path string) (map[string]Owner, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var f service.Tokens
	if err := json.Unmarshal(data, &f); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if f.Version != service.TokensVersion {
		return nil, fmt.Errorf("%s: version %d, want %d", path, f.Version, service.TokensVersion)
	}
	owners := make(map[string]Owner, len(f.Tokens))
	for _, a := range f.Tokens {
		if b, err := hex.DecodeString(a.SHA256); err != nil || len(b) != sha256.Size {
			return nil, fmt.Errorf("%s: invalid sha256 %q", path, a.SHA256)
		}
		if a.Gateway == "" || a.Workspace == "" || a.Sandbox == "" {
			return nil, fmt.Errorf("%s: token %s… has no complete owner", path, a.SHA256[:8])
		}
		owners[strings.ToLower(a.SHA256)] = Owner{Gateway: a.Gateway, Workspace: a.Workspace, Sandbox: a.Sandbox}
	}
	return owners, nil
}

// Owners returns the owners that have a token.
func (t *Tokens) Owners() (map[Owner]bool, error) {
	owners, err := t.load()
	if err != nil {
		return nil, err
	}
	out := make(map[Owner]bool, len(owners))
	for _, o := range owners {
		out[o] = true
	}
	return out, nil
}

// Lookup returns the owner of a bearer token.
func (t *Tokens) Lookup(token string) (Owner, bool, error) {
	owners, err := t.load()
	if err != nil {
		return Owner{}, false, err
	}
	o, ok := owners[service.TokenHash(token)]
	return o, ok, nil
}

type ownerKey struct{}

// ownerOf returns the owner that Auth put into ctx.
func ownerOf(ctx context.Context) (Owner, bool) {
	o, ok := ctx.Value(ownerKey{}).(Owner)
	return o, ok
}

// Auth checks the request's bearer token and returns a context that
// carries its owner.
func (t *Tokens) Auth(r *http.Request) (context.Context, error) {
	token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok || token == "" {
		return nil, errors.New("no bearer token")
	}
	o, ok, err := t.Lookup(token)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, errors.New("unknown token")
	}
	return context.WithValue(r.Context(), ownerKey{}, o), nil
}
