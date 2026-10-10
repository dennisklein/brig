// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/GSI-HPC/go-clikit/progress/progresstest"

	"github.com/dennisklein/brig/internal/libvirt"
)

// TestUpgradeRollbackProgress upgrades a running VM whose network cannot
// start, and checks the steps of the upgrade, of the rollback, and of the
// start of the VM on its old image, which fails as well.
func TestUpgradeRollbackProgress(t *testing.T) {
	a := testApp(t)
	v := testVM(t, a, "dev")
	for _, f := range []string{rootDiskFile, dataDiskFile, clientKeyFile + ".pub"} {
		if err := os.WriteFile(a.vmFile(v, f), []byte("x\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// Without pasta on PATH, the network of the check boot does not start.
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "qemu-img"), []byte("#!/bin/sh\n"+qemuImgInfo+"\n"), 0o700); err != nil { //nolint:gosec // G306: scripts must be executable
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)

	ctx, w := progresstest.Watch(t.Context(), t)
	conn := &fakeDomains{state: libvirt.StateShutoff}
	var out bytes.Buffer
	err := a.upgrade(ctx, conn, v, "f44-openshell0.1.3-20261008T120000Z", true, &out)
	if err == nil || !strings.Contains(err.Error(), "upgrade failed and was rolled back") {
		t.Fatalf("upgrade: %v\n%s", err, &out)
	}
	t.Log(w.Tree())
	want := `step back up the data disk: ok
  call qemu-img info: ok
  call qemu-img snapshot: ok
step roll back the upgrade: ok
  call qemu-img snapshot: ok
  call qemu-img snapshot: ok
step start the VM: failed (target): starting the network of dev: pasta is not installed (package passt): exec: "pasta": executable file not found in $PATH
step start the VM: failed (target): starting the network of dev: pasta is not installed (package passt): exec: "pasta": executable file not found in $PATH
step switch to the new image: ok
  call qemu-img create: ok
  call qemu-img info: ok
`
	if got := w.Finish(); got != want {
		t.Errorf("progress\n%s\nwant\n%s", got, want)
	}
	if v.Image != "f44-openshell0.1.2-20261007T120000Z" {
		t.Errorf("image = %s after the rollback", v.Image)
	}
}
