// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

package service

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/dennisklein/brig/internal/openshell"
	"github.com/dennisklein/brig/internal/paths"
	"github.com/dennisklein/brig/internal/vm"
)

// Service is brig's record of a service. A service of placement vm also has
// a VM of the same name, whose record has the role vm.RoleService.
type Service struct {
	Name      string    `json:"name"`
	Type      string    `json:"type"`
	Placement Placement `json:"placement"`
	// Port is the port on the host's loopback interface at which gateways
	// reach the service; it stays the same for the service's life, since
	// the gateways' firewalls and the URLs in sandboxes name it.
	Port int `json:"port"`
	// Binaries, if set, replace the type's programs that may call the
	// service from a sandbox.
	Binaries  []string  `json:"binaries,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

// CallerBinaries are the programs that may call the service from a
// sandbox: the service's own, else its type's.
func (s *Service) CallerBinaries(t Type) []string {
	if len(s.Binaries) > 0 {
		return s.Binaries
	}
	return t.Binaries
}

// Attachment is a sandbox's access to a service: its token, which brig
// stores as an OpenShell provider on the sandbox's gateway.
type Attachment struct {
	// SHA256 is the hex-encoded SHA-256 of the token, which the default
	// driver gives the service to check tokens against; a driver whose
	// service mints its own tokens leaves it empty.
	SHA256 string `json:"sha256,omitempty"`
	// Gateway names the gateway VM, not its OpenShell gateway.
	Gateway   string `json:"gateway"`
	Workspace string `json:"workspace"`
	Sandbox   string `json:"sandbox"`
	// Provider is the OpenShell provider that holds the token.
	Provider string    `json:"provider"`
	Created  time.Time `json:"created"`
}

// Owner names the attachment's sandbox as GATEWAY/SANDBOX, with the
// workspace unless it is the default one.
func (a Attachment) Owner() string {
	if a.Workspace == openshell.DefaultWorkspace {
		return a.Gateway + "/" + a.Sandbox
	}
	return a.Gateway + "/" + a.Sandbox + " (workspace " + a.Workspace + ")"
}

// Is reports whether the attachment belongs to the sandbox in workspace ws
// of gateway gw.
func (a Attachment) Is(gw, ws, sandbox string) bool {
	return a.Gateway == gw && a.Workspace == ws && a.Sandbox == sandbox
}

// TokensVersion is the layout of tokens.json.
const TokensVersion = 1

// Tokens is the content of a service's tokens.json, which a service, or the
// controller of one on the host, may read: the attachments of the service,
// each with its token's hash.
type Tokens struct {
	Version int          `json:"version"`
	Tokens  []Attachment `json:"tokens"`
}

// ErrNotFound is returned for services that do not exist.
var ErrNotFound = errors.New("no such service")

// Store keeps service records on disk: a directory per service below
// $XDG_DATA_HOME/brig/services holding service.json and tokens.json.
type Store struct {
	root string
}

// NewStore returns a store rooted at the services directory of dirs.
func NewStore(dirs paths.Dirs) Store { return Store{root: filepath.Join(dirs.Data, "services")} }

// Dir is the directory holding the named service's files.
func (s Store) Dir(name string) string { return filepath.Join(s.root, name) }

// TokensPath is the named service's tokens.json.
func (s Store) TokensPath(name string) string { return filepath.Join(s.Dir(name), "tokens.json") }

func (s Store) recordPath(name string) string { return filepath.Join(s.Dir(name), "service.json") }

// Exists reports whether a service record exists.
func (s Store) Exists(name string) bool {
	_, err := os.Stat(s.recordPath(name))
	return err == nil
}

// Load reads the named service's record.
func (s Store) Load(name string) (*Service, error) {
	if err := vm.ValidateName(name); err != nil {
		return nil, err
	}
	data, err := os.ReadFile(s.recordPath(name))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("%w: %s", ErrNotFound, name)
	}
	if err != nil {
		return nil, err
	}
	var svc Service
	if err := json.Unmarshal(data, &svc); err != nil {
		return nil, fmt.Errorf("%s: %w", s.recordPath(name), err)
	}
	if svc.Name != name {
		return nil, fmt.Errorf("%s: record is for service %q", s.recordPath(name), svc.Name)
	}
	if err := svc.check(); err != nil {
		return nil, fmt.Errorf("%s: %w", s.recordPath(name), err)
	}
	return &svc, nil
}

func (s *Service) check() error {
	switch {
	case vm.ValidateName(s.Type) != nil:
		return fmt.Errorf("invalid type %q", s.Type)
	case s.Placement != PlacementVM && s.Placement != PlacementHost:
		return fmt.Errorf("invalid placement %q", s.Placement)
	case s.Port < 1 || s.Port > 65535:
		return fmt.Errorf("invalid port %d", s.Port)
	}
	return checkBinaries(s.Binaries)
}

// Save writes the service's record atomically, creating its directory with
// mode 0700.
func (s Store) Save(svc *Service) error {
	if err := vm.ValidateName(svc.Name); err != nil {
		return err
	}
	if err := svc.check(); err != nil {
		return err
	}
	if err := paths.EnsurePrivate(s.Dir(svc.Name)); err != nil {
		return err
	}
	data, err := json.MarshalIndent(svc, "", "  ")
	if err != nil {
		return err
	}
	return vm.WriteFileAtomic(s.recordPath(svc.Name), append(data, '\n'), 0o600)
}

// List returns all service records sorted by name. Like vm.Store.List, it
// leaves out records that cannot be loaded and names them in its error.
func (s Store) List() ([]*Service, error) {
	entries, err := os.ReadDir(s.root)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var svcs []*Service
	var errs []error
	for _, e := range entries {
		if !e.IsDir() || vm.ValidateName(e.Name()) != nil {
			continue
		}
		svc, err := s.Load(e.Name())
		if errors.Is(err, ErrNotFound) {
			continue
		}
		if err != nil {
			errs = append(errs, err)
			continue
		}
		svcs = append(svcs, svc)
	}
	sort.Slice(svcs, func(i, j int) bool { return svcs[i].Name < svcs[j].Name })
	return svcs, errors.Join(errs...)
}

// Remove deletes the service's directory, its tokens included.
func (s Store) Remove(name string) error {
	if err := vm.ValidateName(name); err != nil {
		return err
	}
	return os.RemoveAll(s.Dir(name))
}

// Lock serialises brig commands that change the named service's record or
// its attachments; it fails at once if another command holds the lock. The
// lock file sits next to the service's directory, as a VM's does.
func (s Store) Lock(name string) (unlock func(), err error) {
	if err := vm.ValidateName(name); err != nil {
		return nil, err
	}
	if err := paths.EnsurePrivate(s.root); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(s.root, "."+name+".lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, fmt.Errorf("another brig command is working on service %s", name)
		}
		return nil, err
	}
	return func() { _ = f.Close() }, nil
}

// Attachments returns the named service's attachments, sorted by gateway,
// workspace and sandbox; a service without tokens.json has none.
func (s Store) Attachments(name string) ([]Attachment, error) {
	if err := vm.ValidateName(name); err != nil {
		return nil, err
	}
	data, err := os.ReadFile(s.TokensPath(name))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var t Tokens
	if err := json.Unmarshal(data, &t); err != nil {
		return nil, fmt.Errorf("%s: %w", s.TokensPath(name), err)
	}
	if t.Version != TokensVersion {
		return nil, fmt.Errorf("%s: version %d, want %d", s.TokensPath(name), t.Version, TokensVersion)
	}
	return t.Tokens, nil
}

// SaveAttachments replaces the named service's tokens.json with atts,
// atomically, readable by the user only.
func (s Store) SaveAttachments(name string, atts []Attachment) error {
	data, err := TokensJSON(atts)
	if err != nil {
		return err
	}
	if err := paths.EnsurePrivate(s.Dir(name)); err != nil {
		return err
	}
	return vm.WriteFileAtomic(s.TokensPath(name), data, 0o600)
}

// TokensJSON returns the content of a tokens.json that lists atts, sorted.
func TokensJSON(atts []Attachment) ([]byte, error) {
	atts = slices.Clone(atts)
	slices.SortFunc(atts, func(a, b Attachment) int {
		return strings.Compare(a.Gateway+"\x00"+a.Workspace+"\x00"+a.Sandbox, b.Gateway+"\x00"+b.Workspace+"\x00"+b.Sandbox)
	})
	if atts == nil {
		atts = []Attachment{}
	}
	data, err := json.MarshalIndent(Tokens{Version: TokensVersion, Tokens: atts}, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(data, '\n'), nil
}
