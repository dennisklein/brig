// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"fmt"
	"strings"

	"github.com/GSI-HPC/go-clikit/cliout"
	"github.com/spf13/cobra"

	"github.com/dennisklein/brig/internal/deps"
)

func newPrintFedoraDepsCmd() *cobra.Command {
	var with []string
	var explain bool
	cmd := &cobra.Command{
		Use:   "print-fedora-deps",
		Short: "Print the Fedora packages brig needs on this host",
		Long: `Print the Fedora packages brig needs on this host, as one line suitable for
dnf. The required packages are always included; --with adds optional groups.`,
		Example: `  sudo dnf install $(brig print-fedora-deps)
  sudo dnf install $(brig print-fedora-deps --with mounts)
  brig print-fedora-deps --with all --explain`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			names, err := deps.Packages(with...)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if !explain {
				_, err := fmt.Fprintln(out, strings.Join(names, " "))
				return err
			}
			want := map[string]bool{}
			for _, n := range names {
				want[n] = true
			}
			t := cliout.Table{Columns: columns("GROUP", "PACKAGE", "REASON")}
			for _, g := range deps.Groups() {
				for _, p := range g.Packages {
					if want[p.Name] {
						t.Add(g.Name, p.Name, p.Reason)
					}
				}
			}
			return t.Write(out, false)
		},
	}
	cmd.Flags().StringSliceVar(&with, "with", nil,
		"optional package groups to include: "+strings.Join(deps.OptionalGroupNames(), ", ")+" or all")
	cmd.Flags().BoolVar(&explain, "explain", false, "print a table with the reason for each package")
	_ = cmd.RegisterFlagCompletionFunc("with", func(*cobra.Command, []string, string) ([]string, cobra.ShellCompDirective) {
		return append(deps.OptionalGroupNames(), "all"), cobra.ShellCompDirectiveNoFileComp
	})
	return cmd
}
