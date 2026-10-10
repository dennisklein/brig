// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/GSI-HPC/go-clikit/progress/progresstest"

	"github.com/dennisklein/brig/internal/libvirt"
)

// fakeDomains is a libvirt that knows one domain, in one state.
type fakeDomains struct {
	state libvirt.State
	calls []string
	// shutdownErr is what Shutdown returns.
	shutdownErr error
}

func (f *fakeDomains) State(string) (libvirt.State, error) { return f.state, nil }

func (f *fakeDomains) Define(string) error { f.calls = append(f.calls, "define"); return nil }

func (f *fakeDomains) Start(string) error { f.calls = append(f.calls, "start"); return nil }

func (f *fakeDomains) Shutdown(context.Context, string, time.Duration) error {
	f.calls = append(f.calls, "shutdown")
	if f.shutdownErr == nil {
		f.state = libvirt.StateShutoff
	}
	return f.shutdownErr
}

func (f *fakeDomains) Destroy(string) error {
	f.calls = append(f.calls, "destroy")
	f.state = libvirt.StateShutoff
	return nil
}

func (f *fakeDomains) Undefine(string) error {
	f.calls = append(f.calls, "undefine")
	f.state = libvirt.StateMissing
	return nil
}

// fakeGuest puts a fake ssh on PATH, and nothing else, that answers like a
// VM whose gateway has created its certificates. A command in the guest
// that matches the shell case pattern in cases gets the answer of its arm
// instead.
func fakeGuest(t *testing.T, cases string) {
	t.Helper()
	pem := func(typ string) string {
		return base64.StdEncoding.EncodeToString([]byte("-----BEGIN " + typ + "-----\nAAAA\n-----END " + typ + "-----\n"))
	}
	dir := t.TempDir()
	body := fmt.Sprintf("#!/bin/sh\ncase \"$*\" in\n%s\n*base64*) printf '%%s\\n' %s %s %s ;;\nesac\n",
		cases, pem("CERTIFICATE"), pem("CERTIFICATE"), pem("PRIVATE KEY"))
	if err := os.WriteFile(filepath.Join(dir, "ssh"), []byte(body), 0o700); err != nil { //nolint:gosec // G306: scripts must be executable
		t.Fatal(err)
	}
	// Without openshell on PATH, brig leaves the gateway unregistered.
	t.Setenv("PATH", dir)
}

func TestStartVMProgress(t *testing.T) {
	a := testApp(t)
	v := testVM(t, a, "dev")
	fakeGuest(t, "")
	ctx, w := progresstest.Watch(t.Context(), t)
	var out bytes.Buffer
	if err := a.startVM(ctx, &fakeDomains{state: libvirt.StateRunning}, v, &out, true); err != nil {
		t.Fatalf("startVM: %v\n%s", err, &out)
	}
	t.Log(w.Tree())
	want := `step boot the VM: ok
  wait wait for SSH: ok
step connect the gateway: ok
  wait wait for the gateway to run: ok
  wait wait for the gateway's certificates: ok
`
	if got := w.Finish(); got != want {
		t.Errorf("progress\n%s\nwant\n%s", got, want)
	}
	if !strings.HasPrefix(out.String(), "The openshell CLI is not installed") {
		t.Errorf("output = %q", &out)
	}
}

func TestStartVMBootProgress(t *testing.T) {
	a := testApp(t)
	v := testVM(t, a, "dev")
	fakeGuest(t, "*true*) echo 'Connection refused' >&2; exit 255 ;;")
	ctx, w := progresstest.Watch(t.Context(), t)
	ctx, cancel := context.WithTimeout(ctx, 300*time.Millisecond)
	defer cancel()
	if err := a.startVM(ctx, &fakeDomains{state: libvirt.StateRunning}, v, new(bytes.Buffer), false); err == nil {
		t.Fatal("startVM succeeded")
	}
	t.Log(w.Tree())
	got := w.Finish()
	for _, want := range []string{"step boot the VM: failed (timeout)", "wait wait for SSH: failed (timeout)"} {
		if !strings.Contains(got, want) {
			t.Errorf("progress lacks %q:\n%s", want, got)
		}
	}
	// The gateway is not tried while the VM does not boot.
	if strings.Contains(got, "gateway") {
		t.Errorf("progress reports the gateway:\n%s", got)
	}
}

func TestStartVMGatewayProgress(t *testing.T) {
	a := testApp(t)
	v := testVM(t, a, "dev")
	// The gateway has not created its certificates.
	fakeGuest(t, "*base64*) exit 3 ;;")
	ctx, w := progresstest.Watch(t.Context(), t)
	ctx, cancel := context.WithTimeout(ctx, 300*time.Millisecond)
	defer cancel()
	if err := a.startVM(ctx, &fakeDomains{state: libvirt.StateRunning}, v, new(bytes.Buffer), false); err == nil {
		t.Fatal("startVM succeeded")
	}
	t.Log(w.Tree())
	got := w.Finish()
	for _, want := range []string{
		"step boot the VM: ok\n  wait wait for SSH: ok\n",
		"step connect the gateway: failed (timeout)",
		"wait wait for the gateway's certificates: failed (timeout)",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("progress lacks %q:\n%s", want, got)
		}
	}
}

func TestStopProgress(t *testing.T) {
	for _, tc := range []struct {
		name  string
		state libvirt.State
		force bool
		err   error
		want  string
	}{
		{"graceful", libvirt.StateRunning, false, nil, "step shut down the VM: ok\n"},
		{"forced", libvirt.StateRunning, true, nil, "step power off the VM: ok\n"},
		{"failed", libvirt.StateRunning, false, errors.New("guest is stuck"), "step shut down the VM: failed (target): guest is stuck\n"},
		// Nothing to wait for.
		{"shut off", libvirt.StateShutoff, false, nil, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := testApp(t)
			v := testVM(t, a, "dev")
			ctx, w := progresstest.Watch(t.Context(), t)
			conn := &fakeDomains{state: tc.state, shutdownErr: tc.err}
			if err := a.stop(ctx, conn, v, tc.force); !errors.Is(err, tc.err) {
				t.Fatalf("stop: %v, want %v", err, tc.err)
			}
			if got := w.Finish(); got != tc.want {
				t.Errorf("progress\n%s\nwant\n%s", got, tc.want)
			}
		})
	}
}

func TestRemoveProgress(t *testing.T) {
	a := testApp(t)
	v := testVM(t, a, "dev")
	t.Setenv("PATH", t.TempDir()) // no openshell to unregister the gateway from
	ctx, w := progresstest.Watch(t.Context(), t)
	var stdout, stderr bytes.Buffer
	conn := &fakeDomains{state: libvirt.StateRunning}
	if err := a.remove(ctx, conn, v, true, &stdout, &stderr); err != nil {
		t.Fatalf("remove: %v\n%s", err, &stderr)
	}
	want := "step power off the VM: ok\nstep remove the VM: ok\n"
	if got := w.Finish(); got != want {
		t.Errorf("progress\n%s\nwant\n%s", got, want)
	}
	if stdout.String() != "Deleted VM dev.\n" || a.vms.Exists("dev") {
		t.Errorf("stdout = %q, VM exists: %v", &stdout, a.vms.Exists("dev"))
	}
}

// qemuImgInfo makes a fake qemu-img answer `info` for a base image.
const qemuImgInfo = `if [ "$1" = info ]; then echo '{"virtual-size": 4294967296, "format": "qcow2"}'; exit 0; fi`

func TestProvisionProgress(t *testing.T) {
	a := testApp(t)
	v := testVM(t, a, "dev")
	fakeCommands(t, map[string]string{"qemu-img": qemuImgInfo})
	ctx, w := progresstest.Watch(t.Context(), t)
	if err := a.provision(ctx, &fakeDomains{state: libvirt.StateMissing}, v); err != nil {
		t.Fatal(err)
	}
	want := "step create the VM: ok\n  call qemu-img create: ok\n  call qemu-img create: ok\n  call qemu-img info: ok\n"
	if got := w.Finish(); got != want {
		t.Errorf("progress\n%s\nwant\n%s", got, want)
	}

	// A failure ends the step with its error, and removes the VM again.
	other := testVM(t, a, "other")
	fakeCommands(t, map[string]string{"qemu-img": qemuImgInfo + "\necho 'qemu-img: no space left' >&2; exit 1"})
	ctx, w = progresstest.Watch(t.Context(), t)
	if err := a.provision(ctx, &fakeDomains{state: libvirt.StateMissing}, other); err == nil {
		t.Fatal("provision succeeded")
	}
	if got := w.Finish(); !strings.HasPrefix(got, "step create the VM: failed (target): qemu-img create: no space left") || a.vms.Exists("other") {
		t.Errorf("progress\n%s\nVM exists: %v", got, a.vms.Exists("other"))
	}
}
