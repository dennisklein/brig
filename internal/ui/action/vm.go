// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

package action

import (
	"fmt"

	"github.com/dennisklein/brig/internal/openshell"
)

func init() {
	register(
		Action{ID: "start", Subject: subjectVM, Build: buildStart},
		Action{ID: "stop", Subject: subjectVM, Build: buildStop},
		Action{ID: "delete", Subject: subjectVM, Build: buildDelete, Typed: subjectVM},
		Action{ID: "upgrade", Subject: subjectVM, Build: buildUpgrade},
	)
}

func buildStart(f Form, c Context) (Command, error) {
	v, err := findVM(f, c)
	if err != nil {
		return Command{}, err
	}
	if v.Running() {
		return Command{}, fmt.Errorf("%s is running already", v.Name)
	}
	cmd := NewCommand("brig", "start", v.Name)
	if len(v.OpenShellConfigs) > 0 {
		cmd = cmd.Note("Applies the OpenShell config directories once the gateway answers. Your keyring may ask to be unlocked.")
	}
	return cmd, nil
}

func buildStop(f Form, c Context) (Command, error) {
	v, err := findVM(f, c)
	if err != nil {
		return Command{}, err
	}
	if v.State == "shut off" {
		return Command{}, fmt.Errorf("%s is shut off already", v.Name)
	}
	cmd := NewCommand("brig", "stop", v.Name)
	if f.Get("force") != "" {
		cmd = NewCommand("brig", "stop", v.Name, "--force").
			Note("--force powers the VM off at once, without asking the guest to shut down. Unsaved work in the VM can be lost.")
	}
	return cmd, nil
}

func buildDelete(f Form, c Context) (Command, error) {
	v, err := findVM(f, c)
	if err != nil {
		return Command{}, err
	}
	cmd := NewCommand("brig", "delete", v.Name)
	if v.State != "shut off" && v.State != "missing" {
		cmd = NewCommand("brig", "delete", v.Name, "--force").
			Note("--force: %s is %s, so brig powers it off first.", v.Name, v.State)
	}
	return cmd, nil
}

func buildUpgrade(f Form, c Context) (Command, error) {
	v, err := findVM(f, c)
	if err != nil {
		return Command{}, err
	}
	newest := c.Snap.NewestImage
	switch {
	case newest == nil:
		return Command{}, fmt.Errorf("there is no base image yet; build one first")
	case v.Image == newest.ID:
		return Command{}, fmt.Errorf("%s runs the newest base image already", v.Name)
	}
	cmd := NewCommand("brig", "upgrade", v.Name)
	if v.OpenShell != "" && !openshell.CompatibleVersions(v.OpenShell, newest.OpenShell) {
		cmd = NewCommand("brig", "upgrade", v.Name, "--allow-minor").
			Note("--allow-minor: OpenShell goes from %s to %s, a minor version change. Sandboxes may need to be recreated; read the release notes first.", v.OpenShell, newest.OpenShell)
	}
	return cmd, nil
}
