// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/GSI-HPC/go-clikit/progress/progresstest"

	"github.com/dennisklein/brig/internal/libvirt"
	"github.com/dennisklein/brig/internal/vmnet"
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

// TestUpgradeProgress upgrades a running VM successfully: it boots on the
// new image, is stopped to delete the data disk snapshot and then starts
// again.
func TestUpgradeProgress(t *testing.T) {
	a := testApp(t)
	v := testVM(t, a, "dev")
	for _, f := range []string{rootDiskFile, dataDiskFile, clientKeyFile + ".pub"} {
		if err := os.WriteFile(a.vmFile(v, f), []byte("x\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	started := 0
	orig := startNetwork
	startNetwork = func(context.Context, vmnet.Config) error { started++; return nil }
	t.Cleanup(func() { startNetwork = orig })

	dir := fakeGuest(t, "")
	if err := os.WriteFile(filepath.Join(dir, "qemu-img"), []byte("#!/bin/sh\n"+qemuImgInfo+"\n"), 0o700); err != nil { //nolint:gosec // G306: scripts must be executable
		t.Fatal(err)
	}

	ctx, w := progresstest.Watch(t.Context(), t)
	conn := &fakeDomains{state: libvirt.StateShutoff}
	var out bytes.Buffer
	if err := a.upgrade(ctx, conn, v, "f44-openshell0.1.3-20261008T120000Z", true, &out); err != nil {
		t.Fatalf("upgrade: %v\n%s", err, &out)
	}
	t.Log(w.Tree())
	got := w.Finish()
	for _, want := range []string{
		"step back up the data disk: ok",
		"step switch to the new image: ok",
		"step boot the VM: ok",
		"step shut down the VM: ok",
		"step delete the data disk snapshot: ok",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("progress lacks %q:\n%s", want, got)
		}
	}
	// The check boot and the start afterwards each start the VM.
	if got := strings.Count(got, "step start the VM: ok"); got != 2 {
		t.Errorf("progress has %d starts of the VM, want 2", got)
	}
	if started != 2 {
		t.Errorf("network started %d times, want 2", started)
	}
	if v.Image != "f44-openshell0.1.3-20261008T120000Z" || conn.state != libvirt.StateRunning {
		t.Errorf("image = %s, state = %s after the upgrade", v.Image, conn.state)
	}
	if !strings.HasSuffix(out.String(), "Upgraded dev to image f44-openshell0.1.3-20261008T120000Z.\n") {
		t.Errorf("output = %q", &out)
	}
}
