// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"context"
	"slices"
	"time"

	"github.com/spf13/cobra"

	"github.com/dennisklein/brig/internal/ocsync"
	"github.com/dennisklein/brig/internal/openshell"
	"github.com/dennisklein/brig/internal/paths"
	"github.com/dennisklein/brig/internal/ui/osclient"
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
