// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

package ocsync

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"time"

	"github.com/dennisklein/brig/internal/paths"
)

// StateFile is the name of the file in a VM's directory that holds its
// State.
const StateFile = "openshell-sync.json"

// State is what brig remembers about a VM's gateway between syncs: which
// profiles and providers it manages, and fingerprints of the credentials it
// set, so that it updates a provider only when a secret changed. It lives in
// the VM's private directory.
type State struct {
	// Key keys the credential fingerprints. It is stored beside them, so
	// that they protect secrets only as well as this file is protected:
	// someone who reads it can test guesses of a low-entropy secret.
	Key []byte `json:"key"`
	// Version is StateVersion; a file with another version is read for its
	// Key and LastSync only.
	Version int `json:"version"`
	// Workspaces maps workspace names to what brig manages in them.
	Workspaces map[string]*WorkspaceState `json:"workspaces,omitempty"`
	LastSync   *Record                    `json:"last_sync,omitempty"`
}

// StateVersion is the layout of the state file: 2 keeps profiles and
// providers per workspace. The flat layout before it is not read, so
// objects that are on the gateway already count as not created by brig.
const StateVersion = 2

// WorkspaceState is what brig manages in one workspace.
type WorkspaceState struct {
	// Created is set when brig created the workspace; brig never deletes
	// a workspace.
	Created   bool                     `json:"created,omitempty"`
	Profiles  map[string]ProfileState  `json:"profiles,omitempty"`
	Providers map[string]ProviderState `json:"providers,omitempty"`
}

// Workspace returns the state of workspace name, creating it if needed.
func (st *State) Workspace(name string) *WorkspaceState {
	if st.Workspaces == nil {
		st.Workspaces = map[string]*WorkspaceState{}
	}
	ws := st.Workspaces[name]
	if ws == nil {
		ws = &WorkspaceState{}
		st.Workspaces[name] = ws
	}
	if ws.Profiles == nil {
		ws.Profiles = map[string]ProfileState{}
	}
	if ws.Providers == nil {
		ws.Providers = map[string]ProviderState{}
	}
	return ws
}

// ProfileState records the profile file content brig last applied.
type ProfileState struct {
	// SHA256 is empty after an import that failed: the gateway may still
	// have committed it, as after a timeout, so the profile counts as
	// created by brig if it exists at the next sync.
	SHA256 string `json:"sha256"`
	// Created is set when brig imported the profile, rather than taking
	// over one that existed; only those does brig ever delete.
	Created bool `json:"created,omitempty"`
	// Routing holds the digests of the profile's keys that decide where and
	// how credentials are sent, as brig applied them (see routing).
	Routing map[string]string `json:"routing,omitempty"`
}

// ProviderState records a provider brig created or updated.
type ProviderState struct {
	Type string `json:"type"`
	// Credentials maps each credential brig set to its fingerprint. It is
	// empty after a create that failed but which the gateway may still have
	// committed, like a profile's SHA256.
	Credentials map[string]string `json:"credentials"`
	// Created is set when brig created the provider, rather than taking over
	// one that existed; only those does brig ever delete.
	Created bool `json:"created,omitempty"`
	// Sources maps each credential brig set to where it was looked up, as
	// Provider.sources gives it.
	Sources map[string]string `json:"sources,omitempty"`
}

// holdsCredentials reports whether the gateway holds credentials that brig
// set, in any workspace.
func (st *State) holdsCredentials() bool {
	for _, ws := range st.Workspaces {
		for _, p := range ws.Providers {
			if len(p.Credentials) > 0 {
				return true
			}
		}
	}
	return false
}

// Record is the outcome of a sync.
type Record struct {
	Time  time.Time `json:"time"`
	Error string    `json:"error,omitempty"`
}

// LoadState reads the state file at path; a missing file yields an empty
// state.
func LoadState(path string) (*State, error) {
	st := &State{}
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return st, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(data, st); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if st.Version != StateVersion {
		st.Workspaces = nil
	}
	return st, nil
}

// Save writes the state to path, readable only by the user.
func (st *State) Save(path string) error {
	st.Version = StateVersion
	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	return paths.WriteFile(path, append(data, '\n'), 0o600)
}

// fingerprint returns a keyed hash of a provider credential's value,
// creating the key on first use.
func (st *State) fingerprint(provider, key, secret string) (string, error) {
	if len(st.Key) == 0 {
		st.Key = make([]byte, 32)
		if _, err := rand.Read(st.Key); err != nil {
			return "", err
		}
	}
	mac := hmac.New(sha256.New, st.Key)
	for _, s := range []string{provider, key, secret} {
		mac.Write([]byte(s))
		mac.Write([]byte{0})
	}
	return hex.EncodeToString(mac.Sum(nil)), nil
}
