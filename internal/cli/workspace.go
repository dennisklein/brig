// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/dennisklein/brig/internal/openshell"
)

// workspaceEnv names the variable that holds the workspace commands work in
// when they are not given one with -w. brig use VM -w W sets it.
const workspaceEnv = "BRIG_WORKSPACE"

// openshellWorkspaceEnv names the variable through which the openshell CLI
// picks its workspace; brig use -w sets it too, so that plain openshell
// commands work in the workspace that brig commands do.
const openshellWorkspaceEnv = "OPENSHELL_WORKSPACE"

// workspaceUsage is the help of -w, which says where its default comes from.
const workspaceUsage = "the workspace of the gateway to work in (default: $" + workspaceEnv + ", else " + openshell.DefaultWorkspace + ")"

// addWorkspaceFlag adds -w/--workspace, for commands that work in one
// workspace; chosenWorkspace turns its value into the workspace to use.
func addWorkspaceFlag(cmd *cobra.Command, workspace *string) {
	cmd.Flags().StringVarP(workspace, "workspace", "w", "", workspaceUsage)
	_ = cmd.RegisterFlagCompletionFunc("workspace", completeWorkspaces)
}

// envWorkspace returns the workspace that BRIG_WORKSPACE names, or "" if it
// is unset or empty.
func envWorkspace() (string, error) {
	ws := os.Getenv(workspaceEnv)
	if ws == "" {
		return "", nil
	}
	if err := openshell.CheckWorkspace(ws); err != nil {
		return "", fmt.Errorf("%s: %w", workspaceEnv, err)
	}
	return ws, nil
}

// chosenWorkspace returns the workspace to work in: given, which is the value
// of -w, else the one that BRIG_WORKSPACE names, else the default workspace.
func chosenWorkspace(given string) (string, error) {
	if given != "" {
		return given, openshell.CheckWorkspace(given)
	}
	ws, err := envWorkspace()
	if ws == "" {
		ws = openshell.DefaultWorkspace
	}
	return ws, err
}
