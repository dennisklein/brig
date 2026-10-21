// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

package vmrunner

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/dennisklein/brig/internal/service"
)

// Unit runs the controller of a vm runner service as a transient systemd
// user unit, `brig vm-runner serve NAME`. It is the service type's
// service.HostRunner.
type Unit struct {
	// RuntimeDir is $XDG_RUNTIME_DIR, where the admin socket goes.
	RuntimeDir string
	// run runs systemd-run and systemctl; tests replace it.
	run func(ctx context.Context, name string, args ...string) ([]byte, error)
}

var _ service.HostRunner = (*Unit)(nil)

// UnitName is the systemd user unit of the vm runner service called name.
func UnitName(name string) string { return "brig-vm-runner-" + name + ".service" }

func (u *Unit) runCmd(ctx context.Context, name string, args ...string) ([]byte, error) {
	if u.run != nil {
		return u.run(ctx, name, args...)
	}
	return exec.CommandContext(ctx, name, args...).CombinedOutput()
}

// Start starts the service's controller unless it runs, and waits until
// its admin API answers. brig calls it at every start of a gateway that
// uses the service, so a running controller and its calls in progress stay
// as they are.
func (u *Unit) Start(ctx context.Context, svc service.Service) error {
	if u.answers(ctx, svc.Name) {
		return nil
	}
	brig, err := os.Executable()
	if err != nil {
		return err
	}
	// A failed unit keeps its name until it is stopped.
	_ = u.Stop(ctx, svc)
	unit := UnitName(svc.Name)
	args := []string{
		"--user", "--quiet", "--collect",
		"--unit=" + unit,
		"--description=brig vm runner " + svc.Name,
		"--property=Type=exec",
		// A crash must not leave sandboxes without their VMs' controller.
		"--property=Restart=on-failure",
		"--property=RestartSec=2",
		"--", brig, "vm-runner", "serve", svc.Name,
	}
	if out, err := u.runCmd(ctx, "systemd-run", args...); err != nil {
		return fmt.Errorf("start %s: %w: %s", unit, err, strings.TrimSpace(string(out)))
	}
	return u.await(ctx, svc.Name, unit)
}

func (u *Unit) answers(ctx context.Context, name string) bool {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	_, err := NewAdminClient(AdminSocket(u.RuntimeDir, name)).List(ctx)
	return err == nil
}

// await waits until the admin API of the controller answers.
func (u *Unit) await(ctx context.Context, name, unit string) error {
	client := NewAdminClient(AdminSocket(u.RuntimeDir, name))
	deadline := time.Now().Add(30 * time.Second)
	for {
		cctx, cancel := context.WithTimeout(ctx, 2*time.Second)
		_, err := client.List(cctx)
		cancel()
		if err == nil {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("%s did not start (journalctl --user -u %s tells why): %w", unit, unit, err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// Stop stops the service's controller. The VMs keep running; a new
// controller takes them over.
func (u *Unit) Stop(ctx context.Context, svc service.Service) error {
	unit := UnitName(svc.Name)
	out, err := u.runCmd(ctx, "systemctl", "--user", "stop", unit)
	if err != nil && !strings.Contains(string(out), "not loaded") {
		return fmt.Errorf("stop %s: %w: %s", unit, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// Delete destroys all VMs of the service through its controller, starting
// the controller if it does not run, and stops the controller.
func (u *Unit) Delete(ctx context.Context, svc service.Service) error {
	if err := u.Start(ctx, svc); err != nil {
		return err
	}
	client := NewAdminClient(AdminSocket(u.RuntimeDir, svc.Name))
	vms, err := client.List(ctx)
	if err != nil {
		return err
	}
	for _, v := range vms {
		if err := client.Destroy(ctx, v.ID); err != nil {
			return fmt.Errorf("destroy VM %s: %w", v.ID, err)
		}
	}
	return u.Stop(ctx, svc)
}
