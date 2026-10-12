// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

package collect

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/dennisklein/brig/internal/ui/store"
	"github.com/dennisklein/brig/internal/vm"
)

type fakeDir struct {
	asked  []string
	closed bool
	err    error
}

func (f *fakeDir) ListDir(_ context.Context, _, sandbox, dir string) (store.Dir, error) {
	f.asked = append(f.asked, sandbox+":"+dir)
	return store.Dir{Files: []store.File{{Name: "a", Kind: "file"}}}, f.err
}

func (f *fakeDir) Close() error { f.closed = true; return nil }

func TestListFiles(t *testing.T) {
	st := store.New()
	st.SetVMs(store.VMState{VMs: []store.VM{
		{VM: vm.VM{Name: "dev", Ports: vm.Ports{Gateway: 4242}}, State: "running"},
		{VM: vm.VM{Name: "off"}, State: "shut off"},
	}})
	c := &fakeDir{}
	var dialed []int
	f := &Files{Store: st, Dial: func(v store.VM) (DirClient, error) { dialed = append(dialed, v.Ports.Gateway); return c, nil }}
	d, err := f.ListFiles(context.Background(), "dev", "default", "webapp", "/sandbox")
	if err != nil || len(d.Files) != 1 || len(c.asked) != 1 || c.asked[0] != "webapp:/sandbox" || !c.closed || len(dialed) != 1 || dialed[0] != 4242 {
		t.Errorf("ListFiles = %+v, %v; asked %v, closed %v, dialed %v", d, err, c.asked, c.closed, dialed)
	}
	for _, name := range []string{"off", "nope"} {
		if _, err := f.ListFiles(context.Background(), name, "default", "x", "/sandbox"); err == nil || !strings.Contains(err.Error(), "does not run") {
			t.Errorf("%s: %v", name, err)
		}
	}
	if len(dialed) != 1 {
		t.Errorf("dialed %v: a VM that does not run was dialed", dialed)
	}
	f.Dial = func(store.VM) (DirClient, error) { return nil, errors.New("no bundle") }
	if _, err := f.ListFiles(context.Background(), "dev", "default", "x", "/sandbox"); err == nil || err.Error() != "no bundle" {
		t.Errorf("dial error: %v", err)
	}
}
