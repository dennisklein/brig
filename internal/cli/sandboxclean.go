// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/GSI-HPC/go-clikit/termtext"
	"github.com/spf13/cobra"

	"github.com/dennisklein/brig/internal/openshell"
	"github.com/dennisklein/brig/internal/ui/store"
)

func newSandboxCleanCmd() *cobra.Command {
	var (
		runs      bool
		workspace string
		format    outputFormat
	)
	cmd := &cobra.Command{
		Use:   "clean [VM] --runs",
		Short: "Delete the expired sandboxes that brig sandbox run left",
		Long: `With --runs, delete the sandboxes of a VM that brig sandbox run kept, or left
behind when it was killed, once their brig.dev/expires label has passed. A
run's sandbox expires a day after brig created it, or with --timeout, an
hour plus the timeout after that. -w deletes only those of that workspace;
BRIG_WORKSPACE does not narrow it.`,
		Example: `  brig sandbox clean dev --runs
  brig sandbox clean --runs -o json`,
		Args:              cobra.MaximumNArgs(1),
		ValidArgsFunction: completeVMs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if !runs {
				return errors.New("say what to clean: --runs")
			}
			if workspace != "" {
				if err := openshell.CheckWorkspace(workspace); err != nil {
					return err
				}
			}
			a, err := newApp()
			if err != nil {
				return err
			}
			v, err := a.resolveVMArg(cmd, args)
			if err != nil {
				return err
			}
			all, err := gatewaySandboxes(cmd.Context(), v)
			if err != nil {
				return err
			}
			now := time.Now()
			var expired []store.Sandbox
			for _, s := range all {
				exp, perr := strconv.ParseInt(s.Labels[expiresLabel], 10, 64)
				if s.Labels[runLabel] != "" && perr == nil && !time.Unix(exp, 0).After(now) && (workspace == "" || s.Workspace == workspace) {
					expired = append(expired, s)
				}
			}
			deleted := []sandboxRow{}
			var errs []error
			if len(expired) > 0 {
				gw, err := dialRunGateway(cmd.Context(), v)
				if err != nil {
					return err
				}
				defer func() { _ = gw.Close() }()
				w := resultWriter(cmd, format)
				for _, s := range expired {
					if err := gw.DeleteSandbox(cmd.Context(), s.Workspace, s.Name); err != nil {
						errs = append(errs, fmt.Errorf("deleting sandbox %s (workspace %s): %w", s.Name, s.Workspace, err))
						continue
					}
					deleted = append(deleted, newSandboxRow(v.Name, s))
					fmt.Fprintf(w, "Deleted sandbox %s of VM %s (workspace %s).\n", termtext.Escape(s.Name), v.Name, termtext.Escape(s.Workspace))
				}
			}
			if format == "json" {
				if err := writeJSON(cmd.OutOrStdout(), deleted); err != nil {
					errs = append(errs, err)
				}
			}
			return errors.Join(errs...)
		},
	}
	addOutputFlag(cmd, &format)
	cmd.Flags().BoolVar(&runs, "runs", false, "delete the expired sandboxes of brig sandbox run")
	cmd.Flags().StringVarP(&workspace, "workspace", "w", "", "delete only the sandboxes of this workspace")
	_ = cmd.RegisterFlagCompletionFunc("workspace", completeWorkspaces)
	return cmd
}
