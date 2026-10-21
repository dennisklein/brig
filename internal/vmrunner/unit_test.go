// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

package vmrunner

import (
	"context"
	"errors"
	"net"
	"net/http"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/dennisklein/brig/internal/service"
)

// fakeSystemd stands in for systemd-run and systemctl: systemd-run serves
// the admin API of env's controller, systemctl stop ends it.
type fakeSystemd struct {
	t       *testing.T
	env     *testEnv
	runtime string
	calls   []string
	ln      net.Listener
}

func (f *fakeSystemd) run(_ context.Context, name string, args ...string) ([]byte, error) {
	f.calls = append(f.calls, name+" "+strings.Join(args, " "))
	switch name {
	case "systemd-run":
		ln, err := listenUnix(AdminSocket(f.runtime, args[len(args)-1]))
		if err != nil {
			f.t.Fatal(err)
		}
		f.ln = ln
		go func() { _ = http.Serve(ln, f.env.c.Admin()) }() //nolint:gosec // G114: a test server
		return nil, nil
	case "systemctl":
		if f.ln == nil {
			return []byte("Failed to stop x.service: Unit x.service not loaded."), errors.New("exit status 5")
		}
		_ = f.ln.Close()
		f.ln = nil
		return nil, nil
	}
	return nil, errors.New("unknown command")
}

func TestUnit(t *testing.T) {
	env := newEnv(t)
	// A UNIX socket's path is short; t.TempDir's may be too long.
	runtime, err := os.MkdirTemp("", "brig")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(runtime) })
	sd := &fakeSystemd{t: t, env: env, runtime: runtime}
	u := &Unit{RuntimeDir: runtime, run: sd.run}
	svc := service.Service{Name: "vms", Type: ServiceType}
	ctx := context.Background()

	if err := u.Start(ctx, svc); err != nil {
		t.Fatal(err)
	}
	if len(sd.calls) != 2 || !strings.HasPrefix(sd.calls[0], "systemctl --user stop brig-vm-runner-vms.service") ||
		!strings.Contains(sd.calls[1], "--unit=brig-vm-runner-vms.service") || !strings.HasSuffix(sd.calls[1], " vm-runner serve vms") {
		t.Errorf("calls = %q", sd.calls)
	}
	// A running controller keeps running, with its calls in progress.
	sd.calls = nil
	if err := u.Start(ctx, svc); err != nil || len(sd.calls) != 0 {
		t.Errorf("second Start = %v with calls %q", err, sd.calls)
	}

	id := env.create("tok-alice", nil)
	other := env.create("tok-bob", nil)
	if err := u.Delete(ctx, svc); err != nil {
		t.Fatal(err)
	}
	if len(env.c.List()) != 0 || env.hv.domains[DomainName(id)] != nil || env.hv.domains[DomainName(other)] != nil {
		t.Errorf("Delete left VMs %v", env.c.List())
	}
	if sd.ln != nil || !slices.ContainsFunc(sd.calls, func(c string) bool { return strings.HasPrefix(c, "systemctl --user stop") }) {
		t.Errorf("Delete did not stop the controller: %q", sd.calls)
	}
	if err := u.Stop(ctx, svc); err != nil {
		t.Errorf("Stop of a unit that is not loaded: %v", err)
	}
}
