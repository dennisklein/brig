// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

package guest

import (
	"fmt"
	"path"
	"slices"
	"strings"

	"github.com/dennisklein/brig/internal/vm"
)

// The scripts below change the mounts of a running VM as its boot
// configuration does at the next boot. They run over ssh, root's as root and
// the others as User.

// MountDirsScript returns User's script that creates the target of m, one of
// mounts, if it is below home, with its parents as the boot's tmpfiles lines
// do; else it returns "". It runs before MountScript. As User, a symbolic
// link that User put in the path leads only where User can write anyway.
func MountDirsScript(mounts []vm.Mount, m vm.Mount) (string, error) {
	if err := checkLive(mounts, m); err != nil {
		return "", err
	}
	if !strings.HasPrefix(m.Target, home+"/") {
		return "", nil
	}
	var b strings.Builder
	b.WriteString("set -e\n")
	for _, d := range (BootConfig{Mounts: mounts}).mountParents() {
		if strings.HasPrefix(m.Target, d+"/") {
			fmt.Fprintf(&b, "install -d -m 0700 %s\n", shQuote(d))
		}
	}
	fmt.Fprintf(&b, "mkdir -p %s\n", shQuote(m.Target))
	return b.String(), nil
}

// MountScript returns root's script that mounts m, one of mounts, after the
// host has attached its virtiofs device and MountDirsScript has run. Below
// home, User owns the path and can change it while the script runs, so the
// script creates nothing there, and it mounts on the directory that it
// opened and checked, not on a path that it looks up again. It refuses a
// target that a symbolic link redirects, e.g. to /etc.
func MountScript(mounts []vm.Mount, m vm.Mount) (string, error) {
	if err := checkLive(mounts, m); err != nil {
		return "", err
	}
	var b strings.Builder
	b.WriteString("set -e\n" + refuseLink)
	if !strings.HasPrefix(m.Target, home+"/") {
		var dirs []string
		for d := m.Target; d != "/"; d = path.Dir(d) {
			dirs = append([]string{d}, dirs...)
		}
		for _, d := range dirs {
			q := shQuote(d)
			fmt.Fprintf(&b, "if [ -L %s ]; then refuse_link %[1]s; fi\n[ -d %[1]s ] || mkdir %[1]s\n", q)
		}
	}
	b.WriteString(enterDir(m.Target))
	fmt.Fprintf(&b, "mountpoint -q . || mount -c -t virtiofs -o %s %s .\n", mountOptions(m), m.Tag)
	return b.String(), nil
}

// UnmountScript returns root's script that unmounts m before the host
// detaches its virtiofs device. Like MountScript, it refuses a target that a
// symbolic link redirects. User cannot rename or replace a mount point, so
// the target's name in its checked parent stays the mount.
func UnmountScript(m vm.Mount) (string, error) {
	if err := CheckMounts([]vm.Mount{m}); err != nil {
		return "", err
	}
	dir, base := shQuote(path.Dir(m.Target)), shQuote(path.Base(m.Target))
	return fmt.Sprintf("set -e\n%s[ -d %s ] || exit 0\n%sif [ ! -L %s ] && mountpoint -q %[4]s; then umount -c %[4]s; fi\n",
		refuseLink, dir, enterDir(path.Dir(m.Target)), base), nil
}

// refuseLink defines the shell function that fails a script for a path that
// a symbolic link redirects.
const refuseLink = "refuse_link() { echo \"brig: refusing $1: a symbolic link redirects it\" >&2; exit 1; }\n"

// enterDir returns the lines that change the shell's directory to dir and
// call refuse_link unless the directory reached is dir itself. The working
// directory then stays that directory, even if its path changes later.
func enterDir(dir string) string {
	q := shQuote(dir)
	return fmt.Sprintf("cd -P %s\nif [ \"$(pwd -P)\" != %[1]s ]; then refuse_link %[1]s; fi\n", q)
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
// running containers, such as OpenShell sandboxes, that use the volume of
// sandbox mount m, one per line. Removing m from under them would leave
// them a dead mount.
func VolumeUsersScript(m vm.Mount) (string, error) {
	if err := CheckMounts([]vm.Mount{m}); err != nil {
		return "", err
	}
	return fmt.Sprintf("podman ps --filter volume=%s --format '{{.Names}}'\n", m.Tag), nil
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
