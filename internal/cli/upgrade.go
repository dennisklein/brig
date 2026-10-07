// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/spf13/cobra"

	"github.com/dennisklein/brig/internal/libvirt"
	"github.com/dennisklein/brig/internal/openshell"
	"github.com/dennisklein/brig/internal/qemuimg"
	"github.com/dennisklein/brig/internal/vm"
	"github.com/dennisklein/brig/internal/vmnet"
)

func newUpgradeCmd() *cobra.Command {
	var (
		imageID string
		force   bool
	)
	cmd := vmCommand("upgrade NAME", "Move a VM onto a newer base image, keeping its data", func(ctx context.Context, cmd *cobra.Command, a *app, v *vm.VM) error {
		unlock, err := a.lockVM(v.Name)
		if err != nil {
			return err
		}
		defer unlock()

		target, err := a.images.Newest()
		if imageID != "" {
			target, err = a.images.Get(imageID)
		}
		if err != nil {
			return err
		}
		w := cmd.OutOrStdout()
		if target.ID == v.Image {
			fmt.Fprintf(w, "VM %s already uses image %s.\n", v.Name, v.Image)
			return nil
		}
		if current, err := a.images.Get(v.Image); err == nil && !force &&
			!openshell.CompatibleVersions(current.OpenShellVersion, target.OpenShellVersion) {
			return fmt.Errorf("image %s moves OpenShell from %s to %s; minor version changes may need sandboxes to be recreated, so pass --force to upgrade anyway",
				target.ID, current.OpenShellVersion, target.OpenShellVersion)
		}

		conn, err := a.connect(ctx)
		if err != nil {
			return err
		}
		defer func() { _ = conn.Close() }()
		state, err := conn.State(v.DomainName())
		if err != nil {
			return err
		}
		wasRunning := state != libvirt.StateShutoff && state != libvirt.StateMissing
		if wasRunning {
			fmt.Fprintf(w, "Stopping %s...\n", v.Name)
			if err := a.stop(ctx, conn, v, false); err != nil {
				return err
			}
		}
		return a.upgrade(ctx, conn, v, target.ID, wasRunning, w)
	})
	cmd.Flags().StringVar(&imageID, "image", "", "base image ID (default: newest)")
	cmd.Flags().BoolVar(&force, "force", false, "allow an OpenShell minor version change")
	_ = cmd.RegisterFlagCompletionFunc("image", completeImages)
	return cmd
}

// upgrade swaps the stopped VM's root disk for a fresh overlay on image id,
// boots it to check that it works and rolls everything back otherwise. The
// data disk is snapshotted first, because the new OpenShell gateway may
// migrate its database. After the check boot, the VM is stopped to delete
// the snapshot and then, with keepRunning, started again.
func (a *app) upgrade(ctx context.Context, conn *libvirt.Conn, v *vm.VM, id string, keepRunning bool, w io.Writer) (err error) {
	root, prev, data := a.vmFile(v, rootDiskFile), a.vmFile(v, rootDiskFile+".prev"), a.vmFile(v, dataDiskFile)
	snapshot := "brig-upgrade-" + time.Now().UTC().Format("20060102T150405Z")
	oldImage := v.Image

	if err := qemuimg.Snapshot(ctx, data, snapshot); err != nil {
		return err
	}
	if err := os.Rename(root, prev); err != nil {
		return err
	}
	v.Image = id
	err = qemuimg.CreateOverlay(ctx, root, a.images.Path(id), v.RootDisk)
	if err == nil {
		err = a.vms.Save(v)
	}
	if err == nil {
		// Start over with a fresh UEFI variable store, whose boot entries
		// may point into the old image; start defines the domain again.
		err = conn.Undefine(v.DomainName())
	}
	if err == nil {
		fmt.Fprintf(w, "Booting %s on image %s...\n", v.Name, id)
		err = a.start(ctx, conn, v, w)
	}
	if err != nil {
		// Roll back even when ctx was cancelled, e.g. by Ctrl-C during the
		// boot: a rollback cut short leaves the VM's record and disks at odds.
		if rerr := a.rollback(context.WithoutCancel(ctx), conn, v, oldImage, snapshot); rerr != nil {
			return fmt.Errorf("upgrade failed: %w; rolling back failed too: %w", err, rerr)
		}
		if keepRunning && ctx.Err() == nil {
			fmt.Fprintf(w, "Upgrade failed; starting %s again on image %s...\n", v.Name, oldImage)
			if serr := a.start(ctx, conn, v, w); serr != nil {
				return fmt.Errorf("upgrade failed and was rolled back: %w; starting %s again failed: %w", err, v.Name, serr)
			}
		}
		return fmt.Errorf("upgrade failed and was rolled back: %w", err)
	}

	if err := os.Remove(prev); err != nil {
		fmt.Fprintf(w, "Warning: %v\n", err)
	}
	// qemu-img cannot change the data disk while the VM has it open.
	if err := a.stop(ctx, conn, v, false); err != nil {
		return fmt.Errorf("upgraded %s to image %s, but stopping it to delete the data disk snapshot %s failed: %w", v.Name, id, snapshot, err)
	}
	if err := qemuimg.DeleteSnapshot(ctx, data, snapshot); err != nil {
		fmt.Fprintf(w, "Warning: could not delete data disk snapshot %s: %v\n", snapshot, err)
	}
	if keepRunning {
		fmt.Fprintf(w, "Starting %s again...\n", v.Name)
		if err := a.start(ctx, conn, v, w); err != nil {
			return fmt.Errorf("upgraded %s to image %s, but starting it again failed: %w", v.Name, id, err)
		}
	}
	fmt.Fprintf(w, "Upgraded %s to image %s.\n", v.Name, id)
	return nil
}

// rollback puts the VM back onto oldImage, with its old root disk and its
// data disk as of snapshot. Its errors say what is left to restore by hand.
func (a *app) rollback(ctx context.Context, conn *libvirt.Conn, v *vm.VM, oldImage, snapshot string) error {
	root, prev, data := a.vmFile(v, rootDiskFile), a.vmFile(v, rootDiskFile+".prev"), a.vmFile(v, dataDiskFile)
	kept := fmt.Sprintf("the old root disk is %s and the data disk keeps the snapshot %s", prev, snapshot)
	if err := conn.Undefine(v.DomainName()); err != nil {
		return fmt.Errorf("%w (%s)", err, kept)
	}
	vmnet.Stop(ctx, v.Name)
	if err := os.Remove(root); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("%w (%s)", err, kept)
	}
	if err := os.Rename(prev, root); err != nil {
		return fmt.Errorf("%w (%s)", err, kept)
	}
	// The record must name the image that the root disk is an overlay of,
	// which the domain gives the disk as its backing store.
	v.Image = oldImage
	if err := a.vms.Save(v); err != nil {
		return fmt.Errorf("%w (the VM's record must name image %s again, and the data disk keeps the snapshot %s)", err, oldImage, snapshot)
	}
	if err := qemuimg.ApplySnapshot(ctx, data, snapshot); err != nil {
		return fmt.Errorf("%w (the data disk keeps the snapshot %s of its state before the upgrade)", err, snapshot)
	}
	if err := qemuimg.DeleteSnapshot(ctx, data, snapshot); err != nil {
		return fmt.Errorf("%w (the data disk was restored but keeps the snapshot %s)", err, snapshot)
	}
	return a.define(conn, v)
}
