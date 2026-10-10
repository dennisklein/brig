// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"bytes"
	"context"
	"encoding/base64"
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
