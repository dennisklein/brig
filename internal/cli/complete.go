// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"context"
	"io"
	"slices"
	"time"

	"github.com/spf13/cobra"

	"github.com/dennisklein/brig/internal/ocsync"
	"github.com/dennisklein/brig/internal/openshell"
	"github.com/dennisklein/brig/internal/osclient"
	"github.com/dennisklein/brig/internal/paths"
	"github.com/dennisklein/brig/internal/vm"
)

// completionTimeout bounds what a completion asks of a gateway, which must
// not keep the shell waiting.
const completionTimeout = 3 * time.Second

// completionVM returns the VM that a completion is for: the one that args
// start with, if it names a VM, else the one that brig use selected.
func completionVM(args []string) *vm.VM {
	dirs, err := paths.Default()
	if err != nil {
		return nil
	}
	store := vm.NewStore(dirs)
	if len(args) > 0 {
		if v, err := store.Load(args[0]); err == nil {
			return v
		}
	}
	if name, err := selectedVM(); err == nil {
		if v, err := store.Load(name); err == nil {
			return v
		}
	}
	return nil
}

// completionWorkspace returns the workspace that cmd's -w names, else the
// one BRIG_WORKSPACE names, else the default.
func completionWorkspace(cmd *cobra.Command) string {
	given, _ := cmd.Flags().GetString("workspace")
	ws, err := chosenWorkspace(given)
	if err != nil {
		return openshell.DefaultWorkspace
	}
	return ws
}

// gatewayClient dials v's gateway, which the caller must close.
func gatewayClient(v *vm.VM) (*osclient.Client, error) {
	home, err := openshell.ConfigHome()
	if err != nil {
		return nil, err
	}
	return osclient.Dial(v.Ports.Gateway, openshell.MTLSDir(home, v.GatewayName()))
}

// completeWorkspaces completes the workspaces that the VM's OpenShell config
// directories define or its gateway has, the default workspace included.
func completeWorkspaces(_ *cobra.Command, args []string, _ string) ([]string, cobra.ShellCompDirective) {
	names := []string{openshell.DefaultWorkspace}
	if v := completionVM(args); v != nil {
		if set, err := ocsync.Load(v.OpenShellConfigs); err == nil {
			names = append(names, set.WorkspaceNames()...)
		}
		if c, err := gatewayClient(v); err == nil {
			ctx, cancel := context.WithTimeout(context.Background(), completionTimeout)
			if ws, err := c.Workspaces(ctx); err == nil {
				names = append(names, ws...)
			}
			cancel()
			_ = c.Close()
		}
	}
	slices.Sort(names)
	return slices.Compact(names), cobra.ShellCompDirectiveNoFileComp
}

// sandboxNames returns the names of the sandboxes in workspace on v's
// gateway, or nil if it does not answer in time.
func sandboxNames(v *vm.VM, workspace string) []string {
	c, err := gatewayClient(v)
	if err != nil {
		return nil
	}
	defer func() { _ = c.Close() }()
	ctx, cancel := context.WithTimeout(context.Background(), completionTimeout)
	defer cancel()
	sandboxes, err := c.Sandboxes(ctx)
	if err != nil {
		return nil
	}
	var names []string
	for _, s := range sandboxes {
		if s.Workspace == workspace {
			names = append(names, s.Name)
		}
	}
	return names
}

// completeVMAndSandbox completes the arguments [VM] SANDBOX: the VM, which
// may be left out, and the sandbox of the VM in the workspace of -w.
func completeVMAndSandbox(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
	const noFiles = cobra.ShellCompDirectiveNoFileComp
	switch len(args) {
	case 0:
		words, _ := completeVMs(cmd, args, toComplete)
		if v := completionVM(nil); v != nil {
			words = append(words, sandboxNames(v, completionWorkspace(cmd))...)
		}
		return words, noFiles
	case 1:
		if v := completionVM(args); v != nil && v.Name == args[0] {
			return sandboxNames(v, completionWorkspace(cmd)), noFiles
		}
	}
	return nil, noFiles
}

// completePolicyFiles completes the names of the policy files in the
// OpenShell config directories of the VM that args name.
func completePolicyFiles(_ *cobra.Command, args []string, _ string) ([]string, cobra.ShellCompDirective) {
	var names []string
	if v := completionVM(args); v != nil {
		if set, err := ocsync.Load(v.OpenShellConfigs); err == nil {
			for _, p := range set.Policies {
				names = append(names, p.Name)
			}
		}
	}
	slices.Sort(names)
	return slices.Compact(names), cobra.ShellCompDirectiveNoFileComp
}

// completeRules completes the keys of the network rules in which the sandbox
// that args name differs from its policy file.
func completeRules(cmd *cobra.Command, args []string, _ string) ([]string, cobra.ShellCompDirective) {
	const noFiles = cobra.ShellCompDirectiveNoFileComp
	if len(args) == 0 {
		return nil, noFiles
	}
	v := completionVM(args[:len(args)-1])
	if v == nil {
		return nil, noFiles
	}
	a, err := newApp()
	if err != nil {
		return nil, noFiles
	}
	ctx, cancel := context.WithTimeout(context.Background(), completionTimeout)
	defer cancel()
	rep, err := a.policyDiff(ctx, v, completionWorkspace(cmd), args[len(args)-1], "", false, io.Discard)
	if err != nil {
		return nil, noFiles
	}
	var keys []string
	for _, r := range rep.Rules {
		keys = append(keys, r.Key)
	}
	return keys, noFiles
}
