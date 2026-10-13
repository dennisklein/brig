// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

package collect

import (
	"context"
	"fmt"

	"github.com/dennisklein/brig/internal/openshell"
	"github.com/dennisklein/brig/internal/osclient"
	"github.com/dennisklein/brig/internal/ui/store"
)

// DirClient lists a directory in a sandbox; osclient.Client does.
type DirClient interface {
	ListDir(ctx context.Context, workspace, sandbox, dir string) (store.Dir, error)
	Close() error
}

// Files lists directories in the sandboxes of the running VMs. It dials the
// VM's gateway for each listing, since a listing is asked for now and then,
// and the gateway is on this host.
type Files struct {
	Store *store.Store
	// Dial returns a client of a VM's gateway.
	Dial func(v store.VM) (DirClient, error)
}

// NewFiles returns a lister that reaches each gateway with the mTLS bundle
// that brig keeps in the openshell CLI's config directory.
func NewFiles(st *store.Store, configHome string) *Files {
	return &Files{Store: st, Dial: func(v store.VM) (DirClient, error) {
		return osclient.Dial(v.Ports.Gateway, openshell.MTLSDir(configHome, v.GatewayName()))
	}}
}

// ListFiles lists the directory dir in the sandbox of the VM named vm.
func (f *Files) ListFiles(ctx context.Context, vm, workspace, sandbox, dir string) (store.Dir, error) {
	v, ok := f.Store.Snapshot().VM(vm)
	if !ok || !v.Running() {
		return store.Dir{}, fmt.Errorf("%s does not run", vm)
	}
	c, err := f.Dial(v)
	if err != nil {
		return store.Dir{}, err
	}
	defer func() { _ = c.Close() }()
	return c.ListDir(ctx, workspace, sandbox, dir)
}
