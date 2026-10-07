// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"errors"
	"fmt"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/dennisklein/brig/internal/doctor"
)

func newDoctorCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "doctor",
		Short: "Check whether this host can build images and run VMs",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			results := doctor.Run(cmd.Context(), doctor.System())
			tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
			for _, r := range results {
				fmt.Fprintf(tw, "%s\t%s\t%s\n", r.Status, r.Name, r.Detail)
			}
			if err := tw.Flush(); err != nil {
				return err
			}
			hints := map[string]bool{}
			for _, r := range results {
				if r.Hint != "" && !hints[r.Hint] {
					hints[r.Hint] = true
					fmt.Fprintf(cmd.OutOrStdout(), "\nTo fix %s: %s\n", r.Name, r.Hint)
				}
			}
			if doctor.Worst(results) == doctor.Fail {
				return errors.New("this host is not ready for brig")
			}
			return nil
		},
	}
}
