// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

// Package cli implements the brig command-line interface.
package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
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
	root := newRootCmd(&p)
	stdout := &countingWriter{w: os.Stdout}
	root.SetOut(stdout)
	cmd, err := root.ExecuteContextC(ctx)
	// An exit status with no message is an outcome, not a failure.
	var exit exitError
	if errors.As(err, &exit) {
		p.end(nil)
		return exit.code
	}
	// End the display before the error line, which it would tear.
	p.end(err)
	return report(cmd, err, stdout.n > 0, os.Stdout, os.Stderr)
}

// report reports err, the error that cmd failed with, if any, and returns
// the exit status. A command run with -o json that printed nothing on
// standard output yet also gets the error there, as JSON.
func report(cmd *cobra.Command, err error, printed bool, stdout, stderr io.Writer) int {
	if err == nil {
		return 0
	}
	code := exitStatus(err)
	if wantsJSON(cmd) && !printed {
		_ = writeJSON(stdout, jsonError{Error: err.Error(), Reason: exitReasons[code], ExitCode: code})
	}
	// An error can carry text from a guest, an image or libvirt.
	fmt.Fprintln(stderr, "Error:", termtext.Escape(err.Error()))
	return code
}

// jsonError is a failure as commands print it with -o json.
type jsonError struct {
	Error    string `json:"error"`
	Reason   string `json:"reason"`
	ExitCode int    `json:"exit_code"`
}

// wantsJSON reports whether cmd was run with -o json.
func wantsJSON(cmd *cobra.Command) bool {
	if cmd == nil {
		return false
	}
	f := cmd.Flags().Lookup("output")
	return f != nil && f.Value.String() == "json"
}

// countingWriter counts what is written through it.
type countingWriter struct {
	w io.Writer
	n int64
}

func (c *countingWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n += int64(n)
	return n, err
}

func newRootCmd(p *progressRun) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "brig",
		Short: "Manage agent-sandbox VMs running NVIDIA OpenShell on Fedora",
		Long: `brig manages the lifecycle of headless Fedora VMs that host an NVIDIA
OpenShell gateway, so autonomous agents run inside a VM boundary in addition
to OpenShell's own sandboxing.

Each VM's gateway is registered with the host's openshell CLI as brig-<name>.
A command that takes a VM NAME works on the VM that brig use selected when you
leave NAME out, and says so on standard error.
Configuration lives in $XDG_CONFIG_HOME/brig/config.yaml. brig help topics
lists the concepts and how-tos that brig help TOPIC explains.

Exit status: 0 success, 3 not found, 4 exists already, 5 not ready (VM,
gateway or sandbox), 6 OpenShell config sync failed, 1 any other failure.`,
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
		newCreateCmd(), newUpCmd(), newListCmd(), newShowCmd(), newStartCmd(), newStopCmd(),
		newUpdateCmd(), newUpgradeCmd(), newSyncCmd(), newDeleteCmd(), newSSHCmd(), newConsoleCmd(), newEnvCmd(), newUseCmd(),
		newSandboxCmd(), newPolicyCmd(), newSecretCmd(), newAgentCmd(), newAuditCmd(),
	} {
		c.GroupID = "vm"
		cmd.AddCommand(c)
	}
	for _, c := range []*cobra.Command{newImageCmd(), newDoctorCmd(), newUICmd(), newPrintFedoraDepsCmd()} {
		c.GroupID = "host"
		cmd.AddCommand(c)
	}
	cmd.AddCommand(newShellInitCmd(), newVersionCmd(), newNetHelperCmd())
	addHelpTopics(cmd)
	// These own the terminal, or must keep standard output exactly as is;
	// ui runs until stopped and is no work to show.
	for _, path := range [][]string{{"ssh"}, {"console"}, {"env"}, {"use"}, {"shell-init"}, {"version"}, {"net-helper"}, {"sandbox", "create"}, {"secret", "set"}, {"agent", "init"}, {"agent", "build"}, {"ui"}} {
		if c, _, err := cmd.Find(path); err == nil && c != cmd {
			if c.Annotations == nil {
				c.Annotations = map[string]string{}
			}
			c.Annotations[noProgress] = "true"
		}
	}
	return cmd
}
