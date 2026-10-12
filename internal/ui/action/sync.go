// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

package action

import (
	"fmt"
	"slices"
)

func init() {
	register(
		Action{ID: "sync-preview", Subject: subjectVM, Build: buildSyncPreview, ShowOutput: true, Next: "sync"},
		Action{ID: "sync", Subject: subjectVM, Build: buildSync},
	)
}

func buildSyncPreview(f Form, c Context) (Command, error) {
	v, err := findVM(f, c)
	if err != nil {
		return Command{}, err
	}
	if err := needRunning(v, "previewing a sync"); err != nil {
		return Command{}, err
	}
	if len(v.OpenShellConfigs) == 0 {
		return Command{}, fmt.Errorf("%s has no OpenShell config directories to sync", v.Name)
	}
	argv, err := syncArgv(f, c, v.Name)
	if err != nil {
		return Command{}, err
	}
	return NewCommand(append(argv, "--dry-run")...).
		Note("Changes nothing. The report names each profile, provider and policy that a sync would create, update or leave, and where a provider's secret may be sent."), nil
}

func buildSync(f Form, c Context) (Command, error) {
	v, err := findVM(f, c)
	if err != nil {
		return Command{}, err
	}
	if err := needRunning(v, "syncing"); err != nil {
		return Command{}, err
	}
	argv, err := syncArgv(f, c, v.Name)
	if err != nil {
		return Command{}, err
	}
	if f.Get("prune") != "" {
		argv = append(argv, "--prune")
	}
	if f.Get("refresh") != "" {
		argv = append(argv, "--refresh-secrets")
	}
	cmd := NewCommand(argv...).
		Note("Secrets go from your keyring to the gateway, never through this page or onto a disk. Your keyring may ask to be unlocked.")
	if f.Get("prune") != "" {
		cmd = cmd.Note("--prune deletes profiles and providers that brig created but no config directory defines any more.")
	}
	return cmd, nil
}

// syncArgv returns the brig sync command of the VM, limited to the
// workspace that the form's field "workspace" names, if any.
func syncArgv(f Form, c Context, vm string) ([]string, error) {
	argv := []string{"brig", "sync", vm}
	ws := f.Get("workspace")
	if ws == "" {
		return argv, nil
	}
	if g, ok := c.Snap.Gateway(vm); !ok || !slices.Contains(g.Config.WorkspaceNames(), ws) {
		return nil, fmt.Errorf("the config directories of %s define nothing for the workspace %q", vm, ws)
	}
	return append(argv, "--workspace", ws), nil
}
