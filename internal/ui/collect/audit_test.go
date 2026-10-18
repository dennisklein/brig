// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

package collect

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/dennisklein/brig/internal/audit"
	"github.com/dennisklein/brig/internal/ui/store"
	"github.com/dennisklein/brig/internal/vm"
)

// fakeSystemctl puts a systemctl on the PATH that counts its calls, and
// returns the count.
func fakeSystemctl(t *testing.T) func() int {
	t.Helper()
	dir := t.TempDir()
	count := filepath.Join(dir, "count")
	script := "#!/bin/sh\necho x >> " + count + "\necho inactive\nexit 3\n"
	if err := os.WriteFile(filepath.Join(dir, "systemctl"), []byte(script), 0o755); err != nil { //nolint:gosec // a test script
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return func() int {
		data, _ := os.ReadFile(count)
		return strings.Count(string(data), "\n")
	}
}

// Only a change of the VMs, or the timer, makes Audit read the trail again:
// the notices of other topics, such as its own result, do not.
func TestAuditReadsOnVMChanges(t *testing.T) {
	calls := fakeSystemctl(t)
	root := filepath.Join(t.TempDir(), "audit")
	if err := os.MkdirAll(filepath.Join(root, "dev"), 0o700); err != nil {
		t.Fatal(err)
	}
	st := store.New()
	st.SetVMs(store.VMState{VMs: []store.VM{{VM: vm.VM{Name: "dev"}, State: "running"}}})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); Audit(ctx, st, audit.Store{Root: root}) }()
	defer func() { cancel(); <-done }()

	waitFor := func(n int) {
		t.Helper()
		for deadline := time.Now().Add(5 * time.Second); calls() < n; {
			if time.Now().After(deadline) {
				t.Fatalf("systemctl ran %d times, want %d", calls(), n)
			}
			time.Sleep(5 * time.Millisecond)
		}
	}
	waitFor(1)
	base := calls()
	for i := range 20 {
		st.SetActivity(make([]store.Run, i+1))
	}
	// A change of the VMs is read once; the notices before it add nothing.
	st.SetVMs(store.VMState{VMs: []store.VM{{VM: vm.VM{Name: "dev"}, State: "shut off"}}})
	waitFor(base + 1)
	time.Sleep(100 * time.Millisecond)
	if got := calls(); got != base+1 {
		t.Errorf("systemctl ran %d times, want %s", got, strconv.Itoa(base+1))
	}
}
