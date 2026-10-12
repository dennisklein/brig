// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"bytes"
	"context"
	"fmt"
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
// new image and keeps running, with the data disk snapshot kept until the VM
// stops. A VM that was stopped before is stopped again and loses the
// snapshot.
func TestUpgradeProgress(t *testing.T) {
	for _, keepRunning := range []bool{true, false} {
		t.Run(fmt.Sprint("keepRunning=", keepRunning), func(t *testing.T) { testUpgradeProgress(t, keepRunning) })
	}
}

func testUpgradeProgress(t *testing.T, keepRunning bool) {
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
	// Snapshots are listed once one has been taken.
	snapshots := filepath.Join(t.TempDir(), "snapshots")
	qemuImg := `if [ "$1" = snapshot ]; then if [ "$4" = -c ]; then echo "$5"; fi > ` + snapshots + `; exit 0; fi
if [ "$1" = info ] && [ -s ` + snapshots + ` ]; then
  read -r name < ` + snapshots + `
  echo '{"virtual-size": 4294967296, "format": "qcow2", "snapshots": [{"name": "'$name'"}]}'; exit 0
fi
` + qemuImgInfo
	if err := os.WriteFile(filepath.Join(dir, "qemu-img"), []byte("#!/bin/sh\n"+qemuImg+"\n"), 0o700); err != nil { //nolint:gosec // G306: scripts must be executable
		t.Fatal(err)
	}

	ctx, w := progresstest.Watch(t.Context(), t)
	conn := &fakeDomains{state: libvirt.StateShutoff}
	var out bytes.Buffer
	if err := a.upgrade(ctx, conn, v, "f44-openshell0.1.3-20261008T120000Z", keepRunning, &out); err != nil {
		t.Fatalf("upgrade: %v\n%s", err, &out)
	}
	t.Log(w.Tree())
	got := w.Finish()
	for _, want := range []string{
		"step back up the data disk: ok",
		"step switch to the new image: ok",
		"step boot the VM: ok",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("progress lacks %q:\n%s", want, got)
		}
	}
	// Only the check boot starts the VM.
	if got := strings.Count(got, "step start the VM: ok"); got != 1 || started != 1 {
		t.Errorf("progress has %d starts of the VM and %d network starts, want 1", got, started)
	}
	stopped := strings.Contains(got, "step shut down the VM: ok")
	deleted := strings.Contains(got, "step delete the data disk snapshot: ok")
	if stopped == keepRunning || deleted == keepRunning {
		t.Errorf("VM stopped = %v, snapshot deleted = %v with keepRunning = %v:\n%s", stopped, deleted, keepRunning, got)
	}
	wantState := libvirt.StateShutoff
	if keepRunning {
		wantState = libvirt.StateRunning
	}
	if v.Image != "f44-openshell0.1.3-20261008T120000Z" || conn.state != wantState {
		t.Errorf("image = %s, state = %s after the upgrade", v.Image, conn.state)
	}
	if !strings.HasSuffix(out.String(), "Upgraded dev to image f44-openshell0.1.3-20261008T120000Z.\n") {
		t.Errorf("output = %q", &out)
	}
}
