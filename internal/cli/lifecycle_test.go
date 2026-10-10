// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dennisklein/brig/internal/vm"
)

func TestWaitForWrapsLastError(t *testing.T) {
	sentinel := errors.New("not yet")
	err := waitFor(context.Background(), 10*time.Millisecond, func(context.Context) error { return sentinel })
	if !errors.Is(err, sentinel) || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("waitFor() = %v, want it to wrap both the last error and the deadline", err)
	}
}

func TestMountSourcesMustNotTurnIntoSymlinks(t *testing.T) {
	home := t.TempDir()
	src, secret := filepath.Join(home, "src"), filepath.Join(home, ".ssh")
	for _, d := range []string{filepath.Join(src, "kit"), secret} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	// A source given through a symbolic link is resolved once.
	link := filepath.Join(home, "link")
	if err := os.Symlink(src, link); err != nil {
		t.Fatal(err)
	}
	m, err := parseMount(link + ":/work:rw")
	if err != nil || m.Source != src {
		t.Fatalf("parseMount = %+v, %v; want source %s", m, err, src)
	}
	kit, err := parseMount(filepath.Join(src, "kit") + ":/kit:ro,sandbox")
	if err != nil {
		t.Fatal(err)
	}
	m.Tag, kit.Tag = "brig0", "brig1"
	if err := checkMounts([]vm.Mount{m, kit}); err != nil {
		t.Fatal(err)
	}
	// The VM swaps the nested source for a link through its rw mount.
	if err := os.Remove(kit.Source); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../.ssh", kit.Source); err != nil {
		t.Fatal(err)
	}
	if err := checkMounts([]vm.Mount{m, kit}); err == nil || !strings.Contains(err.Error(), "symbolic link") {
		t.Fatalf("checkMounts after the swap = %v, want a refusal", err)
	}
}
