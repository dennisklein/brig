// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

// Package cli implements the brig command-line interface.
package cli

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/dennisklein/brig/internal/version"
)

// Execute runs the brig command line and returns the process exit code.
func Execute() int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := newRootCmd().ExecuteContext(ctx); err != nil {
		fmt.Fprintln(os.Stderr, "Error:", err)
		return 1
	}
	return 0
}

func newRootCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "brig",
		Short: "Manage agent-sandbox VMs running NVIDIA OpenShell on Fedora",
		Long: `brig manages the lifecycle of headless Fedora VMs that host an NVIDIA
OpenShell gateway, so autonomous agents run inside a VM boundary in addition
to OpenShell's own sandboxing.

Each VM's gateway is registered with the host's openshell CLI as brig-<name>.
Configuration lives in $XDG_CONFIG_HOME/brig/config.yaml.`,
		Version:       version.Version(),
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	cmd.AddGroup(
		&cobra.Group{ID: "vm", Title: "VM commands:"},
		&cobra.Group{ID: "host", Title: "Host commands:"},
	)
	for _, c := range []*cobra.Command{
		newCreateCmd(), newListCmd(), newShowCmd(), newStartCmd(), newStopCmd(),
		newUpdateCmd(), newUpgradeCmd(), newDeleteCmd(), newSSHCmd(), newConsoleCmd(), newEnvCmd(),
	} {
		c.GroupID = "vm"
		cmd.AddCommand(c)
	}
	for _, c := range []*cobra.Command{newImageCmd(), newDoctorCmd(), newPrintFedoraDepsCmd()} {
		c.GroupID = "host"
		cmd.AddCommand(c)
	}
	cmd.AddCommand(newVersionCmd(), newNetHelperCmd())
	return cmd
}
