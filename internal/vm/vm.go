// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

// Package vm defines brig's record of a VM and stores it on disk.
//
// Each VM lives in its own directory below paths.Dirs.VMsDir, which holds
// vm.json, the VM's disks, keys and logs. vm.json is brig's source of truth;
// the libvirt domain is derived from it.
package vm

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"time"

	"github.com/dennisklein/brig/internal/bytesize"
	"github.com/dennisklein/brig/internal/paths"
)

// VM is brig's record of a VM.
type VM struct {
	Name           string        `json:"name"`
	UUID           string        `json:"uuid"`
	CreatedAt      time.Time     `json:"created_at"`
	Image          string        `json:"image"`
	CPUs           int           `json:"cpus"`
	Memory         bytesize.Size `json:"memory"`
	RootDisk       bytesize.Size `json:"root_disk"`
	DataDisk       bytesize.Size `json:"data_disk"`
	NetworkProfile string        `json:"network_profile"`
	Mounts         []Mount       `json:"mounts,omitempty"`
	// NextMountTag numbers the next mount's virtiofs tag; see
	// AssignMountTags.
	NextMountTag int   `json:"next_mount_tag,omitempty"`
	Ports        Ports `json:"ports"`
}

// Ports are the host loopback ports forwarded into the VM.
type Ports struct {
	SSH     int `json:"ssh"`
	Gateway int `json:"gateway"`
}

// Mount shares a host directory with the VM via virtiofs.
type Mount struct {
	// Source is an absolute host directory.
	Source string `json:"source"`
	// Target is an absolute directory in the VM.
	Target   string `json:"target"`
	ReadOnly bool   `json:"read_only"`
	// Sandbox also offers the directory to OpenShell sandboxes as a Podman
	// volume named Tag; such mounts are always read-only.
	Sandbox bool `json:"sandbox"`
	// Tag identifies the virtiofs device in the VM.
	Tag string `json:"tag"`
}

// DomainName is the libvirt domain name of the VM.
func (v *VM) DomainName() string { return "brig-" + v.Name }

// GatewayName is the name under which the VM's OpenShell gateway is
// registered with the host's openshell CLI.
func (v *VM) GatewayName() string { return "brig-" + v.Name }

var nameRE = regexp.MustCompile(`^[a-z][a-z0-9-]{0,29}[a-z0-9]$|^[a-z]$`)

// ValidateName checks that name can serve as VM, hostname, libvirt domain and
// OpenShell gateway name.
func ValidateName(name string) error {
	if !nameRE.MatchString(name) {
		return fmt.Errorf("invalid VM name %q: use 1-31 lowercase letters, digits and dashes, starting with a letter and not ending with a dash", name)
	}
	return nil
}

// ErrNotFound is returned for VMs that do not exist.
var ErrNotFound = errors.New("no such VM")

// Store keeps VM records on disk.
type Store struct {
	root string
}

// NewStore returns a store rooted at dirs.VMsDir().
func NewStore(dirs paths.Dirs) Store { return Store{root: dirs.VMsDir()} }

// Dir is the directory holding the named VM's files.
func (s Store) Dir(name string) string { return filepath.Join(s.root, name) }

func (s Store) recordPath(name string) string { return filepath.Join(s.Dir(name), "vm.json") }

// Exists reports whether a VM record exists.
func (s Store) Exists(name string) bool {
	_, err := os.Stat(s.recordPath(name))
	return err == nil
}

// Load reads the named VM's record.
func (s Store) Load(name string) (*VM, error) {
	if err := ValidateName(name); err != nil {
		return nil, err
	}
	data, err := os.ReadFile(s.recordPath(name))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("%w: %s", ErrNotFound, name)
	}
	if err != nil {
		return nil, err
	}
	var v VM
	if err := json.Unmarshal(data, &v); err != nil {
		return nil, fmt.Errorf("%s: %w", s.recordPath(name), err)
	}
	if v.Name != name {
		return nil, fmt.Errorf("%s: record is for VM %q", s.recordPath(name), v.Name)
	}
	for _, m := range v.Mounts {
		if m.Sandbox && !m.ReadOnly {
			return nil, fmt.Errorf("%s: sandbox mount %s must be read-only", s.recordPath(name), m.Target)
		}
	}
	return &v, nil
}

// Save writes the VM's record atomically, creating its directory with mode
// 0700.
func (s Store) Save(v *VM) error {
	if err := ValidateName(v.Name); err != nil {
		return err
	}
	if err := paths.EnsurePrivate(s.Dir(v.Name)); err != nil {
		return err
	}
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return WriteFileAtomic(s.recordPath(v.Name), append(data, '\n'), 0o600)
}

// List returns all VM records sorted by name. Directories without a record
// are skipped. Records that cannot be loaded are left out, and the error
// then names them, so that one broken record does not hide the other VMs.
func (s Store) List() ([]*VM, error) {
	entries, err := os.ReadDir(s.root)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var vms []*VM
	var errs []error
	for _, e := range entries {
		if !e.IsDir() || ValidateName(e.Name()) != nil {
			continue
		}
		v, err := s.Load(e.Name())
		if errors.Is(err, ErrNotFound) {
			continue
		}
		if err != nil {
			errs = append(errs, err)
			continue
		}
		vms = append(vms, v)
	}
	sort.Slice(vms, func(i, j int) bool { return vms[i].Name < vms[j].Name })
	return vms, errors.Join(errs...)
}

// Names returns the names of all VMs, for shell completion.
func (s Store) Names() []string {
	vms, _ := s.List()
	names := make([]string, len(vms))
	for i, v := range vms {
		names[i] = v.Name
	}
	return names
}

// Remove deletes the VM's directory and everything in it.
func (s Store) Remove(name string) error {
	if err := ValidateName(name); err != nil {
		return err
	}
	return os.RemoveAll(s.Dir(name))
}

// WriteFileAtomic writes data to a temporary file next to path and renames it
// over path, so readers never see a partial file.
func WriteFileAtomic(path string, data []byte, perm fs.FileMode) error {
	f, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer func() { _ = os.Remove(tmp) }() // no-op after a successful rename
	_, err = f.Write(data)
	if err == nil {
		err = f.Chmod(perm)
	}
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
