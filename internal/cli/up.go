// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/GSI-HPC/go-clikit/termtext"
	"github.com/spf13/cobra"

	"github.com/dennisklein/brig/internal/openshell"
	"github.com/dennisklein/brig/internal/vm"
)

func newUpCmd() *cobra.Command {
	var (
		create bool
		format outputFormat
	)
	cmd := &cobra.Command{
		Use:   "up [NAME] [--create [-- CREATE-FLAGS...]]",
		Short: "Make a VM ready to work in, and list its sandboxes",
		Long: `Start the VM if it is stopped, or reconnect it if it runs, wait until its
gateway answers and apply its OpenShell config directories, as brig start
--wait does. Then list its sandboxes and the drafted network rules that wait
for your review.

With --create, a VM that does not exist is created first, with the flags of
brig create that follow --; they do not change a VM that exists. brig up is
therefore safe to repeat, which scripts and the start of a day want.

It fails with exit status 3 if the VM does not exist and --create is not
given, 5 if the gateway does not answer and 6 if the sync fails.`,
		Example: `  brig up dev
  brig up dev --create -- --memory 16GiB --mount ~/src:/mnt/src:sandbox
  brig up -o json`,
		Args: func(cmd *cobra.Command, args []string) error {
			if n := len(vmArgs(cmd, args)); n > 1 {
				return fmt.Errorf("accepts at most one VM name before --, received %d", n)
			}
			if len(args) > len(vmArgs(cmd, args)) && !create {
				return errors.New("the flags after -- are for --create")
			}
			return nil
		},
		ValidArgsFunction: completeVMs,
		RunE: func(cmd *cobra.Command, args []string) error {
			named, createArgs := vmArgs(cmd, args), args[len(vmArgs(cmd, args)):]
			ctx, w := cmd.Context(), cmd.ErrOrStderr()
			a, err := newApp()
			if err != nil {
				return err
			}
			v, err := a.resolveVMArg(cmd, named)
			switch {
			case errors.Is(err, vm.ErrNotFound) && create:
				if v, err = a.createForUp(ctx, cmd, named, createArgs); err != nil {
					return err
				}
			case errors.Is(err, vm.ErrNotFound):
				return fmt.Errorf("%w; brig up --create creates it", err)
			case err != nil:
				return err
			default:
				if len(createArgs) > 0 {
					fmt.Fprintf(w, "VM %s exists, so brig up leaves its settings as they are; brig update changes them.\n", v.Name)
				}
				if err := a.lockAndStart(ctx, v, w, readySynced); err != nil {
					return err
				}
			}
			return a.printUp(ctx, cmd, v, format)
		},
	}
	cmd.Flags().BoolVar(&create, "create", false, "create the VM if it does not exist, with the brig create flags after --")
	addOutputFlag(cmd, &format)
	return cmd
}

// createForUp creates the VM that named names, or else the one brig use
// selected, by running brig create NAME --wait with createArgs, and loads
// it.
func (a *app) createForUp(ctx context.Context, cmd *cobra.Command, named, createArgs []string) (*vm.VM, error) {
	name := ""
	if len(named) > 0 {
		name = named[0]
	} else {
		var err error
		if name, err = selectedVM(); err != nil {
			return nil, err
		}
	}
	c := newCreateCmd()
	c.SetArgs(append([]string{name, "--wait"}, createArgs...))
	// brig create's result is a note here: brig up prints the sandboxes.
	c.SetOut(cmd.ErrOrStderr())
	c.SetErr(cmd.ErrOrStderr())
	c.SilenceErrors, c.SilenceUsage = true, true
	if err := c.ExecuteContext(ctx); err != nil {
		return nil, err
	}
	return a.vms.Load(name)
}

// upSandbox is a sandbox as brig up prints it: with the number of network
// rules that OpenShell drafted from its denied requests and that wait for
// review.
type upSandbox struct {
	sandboxRow
	PendingRules int `json:"pending_rules"`
}

// gatewayDrafts counts the drafted rules of each of the sandboxes of v's
// gateway, in order; tests replace it.
var gatewayDrafts = func(ctx context.Context, v *vm.VM, sandboxes []sandboxRow) ([]int, error) {
	c, err := dialGateway(ctx, v)
	if err != nil {
		return nil, err
	}
	defer func() { _ = c.Close() }()
	out := make([]int, len(sandboxes))
	var errs []error
	for i, s := range sandboxes {
		d, err := c.Drafts(ctx, s.Workspace, s.Name)
		if err != nil {
			errs = append(errs, fmt.Errorf("drafted rules of sandbox %s: %w", s.Name, err))
		}
		out[i] = len(d)
	}
	return out, errors.Join(errs...)
}

// printUp prints v's sandboxes and what waits for review.
func (a *app) printUp(ctx context.Context, cmd *cobra.Command, v *vm.VM, format outputFormat) error {
	rows, err := vmSandboxes(ctx, v)
	if err != nil {
		return err
	}
	var pending []int
	if len(rows) > 0 {
		if pending, err = gatewayDrafts(ctx, v, rows); err != nil {
			fmt.Fprintf(cmd.ErrOrStderr(), "Warning: %s\n", termtext.Escape(err.Error()))
		}
	}
	sandboxes := make([]upSandbox, len(rows))
	for i, r := range rows {
		sandboxes[i].sandboxRow = r
		if i < len(pending) {
			sandboxes[i].PendingRules = pending[i]
		}
	}
	out := cmd.OutOrStdout()
	if format == "json" {
		return writeJSON(out, struct {
			VM        vmStatus    `json:"vm"`
			Sandboxes []upSandbox `json:"sandboxes"`
		}{a.vmShow(ctx, v, cmd.ErrOrStderr()), sandboxes})
	}
	if len(rows) == 0 {
		fmt.Fprintf(out, "VM %s is ready and has no sandboxes yet; create one with brig sandbox create %[1]s.\n", v.Name)
		return nil
	}
	if err := printSandboxes(out, rows, format, false); err != nil {
		return err
	}
	return printPending(out, v, sandboxes)
}

// printPending says which sandboxes have drafted rules that wait for review.
func printPending(w io.Writer, v *vm.VM, sandboxes []upSandbox) error {
	var lines []string
	for _, s := range sandboxes {
		if s.PendingRules == 0 {
			continue
		}
		rules := "rules wait"
		if s.PendingRules == 1 {
			rules = "rule waits"
		}
		get := strings.Join(append([]string{"openshell"}, openshell.WorkspaceArgs(v.GatewayName(), s.Workspace)...), " ")
		lines = append(lines, fmt.Sprintf("%d drafted %s for review in sandbox %s: %s rule get %s --status pending, or brig ui.",
			s.PendingRules, rules, termtext.Escape(s.Name), get, termtext.Escape(s.Name)))
	}
	if len(lines) == 0 {
		return nil
	}
	_, err := fmt.Fprintf(w, "\n%s\n", strings.Join(lines, "\n"))
	return err
}
