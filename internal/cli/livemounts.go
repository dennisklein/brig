// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"context"
	"fmt"
	"strings"

	"github.com/GSI-HPC/go-clikit/progress"
	"github.com/GSI-HPC/go-clikit/termtext"

	"github.com/dennisklein/brig/internal/guest"
	"github.com/dennisklein/brig/internal/libvirt"
	"github.com/dennisklein/brig/internal/vm"
)

// liveDevices is the part of *libvirt.Conn that changes the devices of a
// running domain; tests replace it.
type liveDevices interface {
	AttachDevice(name, deviceXML string) error
	DetachDevice(name, deviceXML string) error
}

// hostMountIDMap returns the ID map of the VMs' mounts; tests replace it.
var hostMountIDMap = libvirt.HostMountIDMap

// guestRun runs script in v, as root or as guest.User, and returns its
// standard output.
func (a *app) guestRun(ctx context.Context, v *vm.VM, root bool, script string) (string, error) {
	out, err := a.sshTarget(v, root).Output(ctx, script)
	return string(out), err
}

// mountChanges returns the mounts of old that mounts lacks and the mounts of
// mounts that old lacks. Tags are never reused, so they identify mounts.
func mountChanges(old, mounts []vm.Mount) (removed, added []vm.Mount) {
	has := func(ms []vm.Mount, tag string) bool {
		for _, m := range ms {
			if m.Tag == tag {
				return true
			}
		}
		return false
	}
	for _, m := range old {
		if !has(mounts, m.Tag) {
			removed = append(removed, m)
		}
	}
	for _, m := range mounts {
		if !has(old, m.Tag) {
			added = append(added, m)
		}
	}
	return removed, added
}

// checkVolumesUnused fails if a container in v, such as an OpenShell
// sandbox, running or not, uses the volume of a sandbox mount among removed.
// Unmounting it would leave the container a dead mount.
func (a *app) checkVolumesUnused(ctx context.Context, v *vm.VM, removed []vm.Mount) error {
	for _, m := range removed {
		if !m.Sandbox {
			continue
		}
		script, err := guest.VolumeUsersScript(m)
		if err != nil {
			return err
		}
		out, err := a.guestRun(ctx, v, false, script)
		if err != nil {
			return fmt.Errorf("check which sandboxes use volume %s: %w", m.Tag, err)
		}
		if users := strings.Fields(out); len(users) > 0 {
			return fmt.Errorf("mount %s is in use as volume %s by %s; delete those sandboxes first, or stop VM %s",
				m.Target, m.Tag, termtext.Escape(strings.Join(users, ", ")), v.Name)
		}
	}
	return nil
}

// applyMounts changes the mounts of the running VM v from old to v.Mounts,
// as its next boot would: it unmounts and detaches removed mounts, attaches
// and mounts added ones, and offers the sandbox mounts among them to
// OpenShell sandboxes. It reports whether the gateway accepts sandbox volumes
// only after the VM restarts.
func (a *app) applyMounts(ctx context.Context, conn liveDevices, v *vm.VM, old []vm.Mount) (gatewayRestart bool, err error) {
	removed, added := mountChanges(old, v.Mounts)
	if len(removed)+len(added) == 0 {
		return false, nil
	}
	idmap, err := hostMountIDMap()
	if err != nil {
		return false, err
	}
	sandbox := false
	for _, m := range removed {
		sandbox = sandbox || m.Sandbox
		err := inSpan(ctx, progress.KindStep, "unmount "+m.Target, func(ctx context.Context) error {
			// A sandbox may have attached the volume since the caller checked.
			if err := a.checkVolumesUnused(ctx, v, []vm.Mount{m}); err != nil {
				return err
			}
			script, err := guest.UnmountScript(m)
			if err != nil {
				return err
			}
			if _, err := a.guestRun(ctx, v, true, script); err != nil {
				return err
			}
			x, err := libvirt.FilesystemXML(m, idmap)
			if err != nil {
				return err
			}
			return conn.DetachDevice(v.DomainName(), x)
		})
		if err != nil {
			return false, err
		}
	}
	for _, m := range added {
		sandbox = sandbox || m.Sandbox
		err := inSpan(ctx, progress.KindStep, "mount "+m.Target, func(ctx context.Context) error {
			x, err := libvirt.FilesystemXML(m, idmap)
			if err != nil {
				return err
			}
			if err := conn.AttachDevice(v.DomainName(), x); err != nil {
				return err
			}
			script, err := guest.MountScript(v.Mounts, m)
			if err != nil {
				return err
			}
			_, err = a.guestRun(ctx, v, true, script)
			return err
		})
		if err != nil {
			return false, err
		}
	}
	if !sandbox {
		return false, nil
	}
	err = inSpan(ctx, progress.KindStep, "update the sandbox volumes", func(ctx context.Context) error {
		script, err := guest.SandboxVolumesScript(v.Mounts)
		if err != nil {
			return err
		}
		out, err := a.guestRun(ctx, v, false, script)
		gatewayRestart = strings.Contains(out, guest.GatewayRestartNeeded)
		return err
	})
	return gatewayRestart, err
}
