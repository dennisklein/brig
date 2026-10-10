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

	"github.com/GSI-HPC/go-clikit/termtext"
	"github.com/spf13/cobra"

	"github.com/dennisklein/brig/internal/version"
)

// Execute runs the brig command line and returns the process exit code.
func Execute() int {
	// A closed terminal sends SIGHUP; like Ctrl-C, it cancels the command so
	// that it cleans up or rolls back.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	defer stop()

	var p progressRun
	err := newRootCmd(&p).ExecuteContext(ctx)
	// End the display before the error line, which it would tear.
	p.end(err)
	if err != nil {
		// An error can carry text from a guest, an image or libvirt.
		fmt.Fprintln(os.Stderr, "Error:", termtext.Escape(err.Error()))
		return 1
	}
	return 0
}

func newRootCmd(p *progressRun) *cobra.Command {
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
	var f progressFlags
	f.register(cmd)
	cmd.PersistentPreRunE = func(c *cobra.Command, args []string) error { return p.start(c, args, &f) }
	cmd.AddGroup(
		&cobra.Group{ID: "vm", Title: "VM commands:"},
		&cobra.Group{ID: "host", Title: "Host commands:"},
	)
	for _, c := range []*cobra.Command{
		newCreateCmd(), newListCmd(), newShowCmd(), newStartCmd(), newStopCmd(),
		newUpdateCmd(), newUpgradeCmd(), newSyncCmd(), newDeleteCmd(), newSSHCmd(), newConsoleCmd(), newEnvCmd(), newUseCmd(),
	} {
		c.GroupID = "vm"
		cmd.AddCommand(c)
	}
	for _, c := range []*cobra.Command{newImageCmd(), newDoctorCmd(), newPrintFedoraDepsCmd()} {
		c.GroupID = "host"
		cmd.AddCommand(c)
	}
	cmd.AddCommand(newShellInitCmd(), newVersionCmd(), newNetHelperCmd())
	// These own the terminal, or must keep standard output exactly as is.
	for _, name := range []string{"ssh", "console", "env", "use", "shell-init", "version", "net-helper"} {
		if c, _, err := cmd.Find([]string{name}); err == nil && c != cmd {
			if c.Annotations == nil {
				c.Annotations = map[string]string{}
			}
			c.Annotations[noProgress] = "true"
		}
	}
	return cmd
}
