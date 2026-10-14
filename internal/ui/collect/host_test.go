// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

package collect

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/dennisklein/brig/internal/ui/store"
)

func fakeOpenShell(t *testing.T, dir, version string) {
	t.Helper()
	script := "#!/bin/sh\necho 'openshell " + version + "'\n"
	if err := os.WriteFile(filepath.Join(dir, "openshell"), []byte(script), 0o700); err != nil { //nolint:gosec // G306: the fake must be executable
		t.Fatal(err)
	}
}

func TestHostRunPicksUpUpgradedCLI(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PATH", dir)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	fakeOpenShell(t, dir, "0.1.3")

	st := store.New()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); hostRun(ctx, st, "addr", 10*time.Millisecond) }()
	defer func() { cancel(); <-done }()

	waitCLI := func(want string) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for st.Snapshot().Host.OpenShellCLI != want {
			if time.Now().After(deadline) {
				t.Fatalf("OpenShellCLI = %q, want %q", st.Snapshot().Host.OpenShellCLI, want)
			}
			time.Sleep(5 * time.Millisecond)
		}
	}
	waitCLI("0.1.3")
	fakeOpenShell(t, dir, "0.1.4")
	waitCLI("0.1.4")
}
