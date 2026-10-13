// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

package audit

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
)

// UnitName is the systemd user unit that runs the VM's collector.
func UnitName(vm string) string { return "brig-audit-" + vm + ".service" }

// netUnit is the unit of the VM's network, which the collector lives as long
// as; see package vmnet.
func netUnit(vm string) string { return "brig-net-" + vm + ".service" }

// unitEnv are the variables that the collector needs from brig's
// environment, which the user manager's may lack.
var unitEnv = []string{"XDG_CONFIG_HOME", "XDG_DATA_HOME", "XDG_STATE_HOME", "XDG_CACHE_HOME"}

// UnitArgs are the arguments of systemd-run that start the VM's collector,
// brig at path brig running brig audit collect.
func UnitArgs(vm, brig string) []string {
	args := []string{
		"--user", "--quiet", "--collect",
		"--unit=" + UnitName(vm),
		"--description=brig audit collector of VM " + vm,
		"--property=Type=exec",
		"--property=BindsTo=" + netUnit(vm),
		"--property=After=" + netUnit(vm),
		"--property=Restart=on-failure",
		"--property=RestartSec=5s",
	}
	for _, name := range unitEnv {
		if v, ok := os.LookupEnv(name); ok {
			args = append(args, "--setenv="+name+"="+v)
		}
	}
	return append(args, "--", brig, "audit", "collect", vm)
}

// StartUnit starts the VM's collector unless it runs.
func StartUnit(ctx context.Context, vm string) error {
	if UnitState(ctx, vm) == UnitRunning {
		return nil
	}
	brig, err := os.Executable()
	if err != nil {
		return err
	}
	resetFailed(ctx, vm)
	if out, err := exec.CommandContext(ctx, "systemd-run", UnitArgs(vm, brig)...).CombinedOutput(); err != nil {
		return fmt.Errorf("starting %s: %w: %s", UnitName(vm), err, strings.TrimSpace(string(out)))
	}
	return nil
}

// StopUnit stops the VM's collector if it runs, which writes what it has
// read before it ends.
func StopUnit(ctx context.Context, vm string) {
	cmd := exec.CommandContext(ctx, "systemctl", "--user", "stop", UnitName(vm))
	// Kept from the terminal's Ctrl-C, like the network unit's stop.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	_ = cmd.Run()
	resetFailed(ctx, vm)
}

func resetFailed(ctx context.Context, vm string) {
	cmd := exec.CommandContext(ctx, "systemctl", "--user", "reset-failed", UnitName(vm))
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	_ = cmd.Run()
}

// The states of a collector's unit.
const (
	UnitRunning = "running"
	UnitFailed  = "failed"
	UnitStopped = "stopped"
	// UnitUnknown: systemd could not be asked.
	UnitUnknown = "unknown"
)

// UnitState says whether the VM's collector runs, failed or is stopped.
func UnitState(ctx context.Context, vm string) string {
	out, err := exec.CommandContext(ctx, "systemctl", "--user", "is-active", UnitName(vm)).Output()
	state := strings.TrimSpace(string(out))
	switch {
	case state == "active" || state == "activating" || state == "reloading":
		return UnitRunning
	case state == "failed":
		return UnitFailed
	case state != "":
		return UnitStopped
	case err != nil:
		return UnitUnknown
	}
	return UnitStopped
}

// UnitError is the last line that the VM's collector logged, for a failed
// one.
func UnitError(ctx context.Context, vm string) string {
	out, err := exec.CommandContext(ctx, "journalctl", "--user", "--unit", UnitName(vm), "--lines", "1", "--no-pager", "--output", "cat").Output()
	if err != nil {
		return "see journalctl --user -u " + UnitName(vm)
	}
	return strings.TrimSpace(string(out))
}
