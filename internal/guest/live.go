// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

package guest

import (
	"fmt"
	"slices"
	"strings"

	"github.com/dennisklein/brig/internal/vm"
)

// The scripts below change the mounts of a running VM as its boot
// configuration does at the next boot. They run over ssh, root's as root and
// the others as User.

// MountScript returns root's script that mounts m, one of mounts, after the
// host has attached its virtiofs device. Like the boot's tmpfiles lines, it
// creates the parents of a target below home for User.
func MountScript(mounts []vm.Mount, m vm.Mount) (string, error) {
	if err := checkLive(mounts, m); err != nil {
		return "", err
	}
	var b strings.Builder
	b.WriteString("set -e\n")
	for _, d := range (BootConfig{Mounts: mounts}).mountParents() {
		if strings.HasPrefix(m.Target, d+"/") {
			fmt.Fprintf(&b, "install -d -m 0700 -o %s -g %[1]s %s\n", User, shQuote(d))
		}
	}
	t := shQuote(m.Target)
	fmt.Fprintf(&b, "mkdir -p %s\n", t)
	fmt.Fprintf(&b, "mountpoint -q %s || mount -t virtiofs -o %s %s %[1]s\n", t, mountOptions(m), m.Tag)
	return b.String(), nil
}

// UnmountScript returns root's script that unmounts m before the host
// detaches its virtiofs device.
func UnmountScript(m vm.Mount) (string, error) {
	if err := CheckMounts([]vm.Mount{m}); err != nil {
		return "", err
	}
	t := shQuote(m.Target)
	return fmt.Sprintf("if mountpoint -q %s; then umount %[1]s; fi\n", t), nil
}

// SandboxVolumesScript returns User's script that offers the sandbox mounts
// among mounts to OpenShell sandboxes, and no others: it rewrites the list
// that brig-sandbox-volumes.service reads and restarts the service. It
// prints GatewayRestartNeeded if the gateway was started without the
// configuration that accepts the volumes, as by brig before it set that
// configuration on every VM.
func SandboxVolumesScript(mounts []vm.Mount) (string, error) {
	if err := CheckMounts(mounts); err != nil {
		return "", err
	}
	return fmt.Sprintf("set -e\numask 077\nprintf %%s %s >%s\nsystemctl --user restart brig-sandbox-volumes.service\n"+
		"grep -qsF %s %s || echo %s\n",
		shQuote(sandboxVolumeList(mounts)), sandboxVolumes,
		gatewayMountsConfig, gatewayEnv, GatewayRestartNeeded), nil
}

// GatewayRestartNeeded is what SandboxVolumesScript prints when the gateway
// does not accept sandbox volumes until the VM restarts.
const GatewayRestartNeeded = "brig: gateway restart needed"

// VolumeUsersScript returns User's script that prints the names of the
// containers, such as OpenShell sandboxes, that use the volume of sandbox
// mount m, one per line, running or not. Removing m from under them would
// leave them a dead mount.
func VolumeUsersScript(m vm.Mount) (string, error) {
	if err := CheckMounts([]vm.Mount{m}); err != nil {
		return "", err
	}
	return fmt.Sprintf("podman ps -a --filter volume=%s --format '{{.Names}}'\n", m.Tag), nil
}

func checkLive(mounts []vm.Mount, m vm.Mount) error {
	if err := CheckMounts(mounts); err != nil {
		return err
	}
	if !slices.Contains(mounts, m) {
		return fmt.Errorf("mount %s is not one of the VM's mounts", m)
	}
	return nil
}

// shQuote quotes s for a POSIX shell.
func shQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
