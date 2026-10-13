// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"

	"github.com/GSI-HPC/go-clikit/termtext"
	"github.com/spf13/cobra"

	"github.com/dennisklein/brig/internal/doctor"
)

// hostPreflight returns the failed checks of brig doctor that take no time;
// tests replace it.
var hostPreflight = func(ctx context.Context) []doctor.Result {
	return doctor.Preflight(ctx, doctor.System())
}

// checkHost fails, before a command starts long work, if the host fails a
// check of brig doctor that the work needs. It tells on w which checks and
// how to fix them.
func checkHost(ctx context.Context, w io.Writer) error {
	failed := hostPreflight(ctx)
	if len(failed) == 0 {
		return nil
	}
	fmt.Fprintln(w, "This host fails checks that brig needs:")
	for _, r := range failed {
		fmt.Fprintf(w, "  %s: %s\n", termtext.Escape(r.Name), termtext.Escape(r.Detail))
	}
	printHints(w, failed)
	return errors.New("this host is not ready for brig; fix the above, then see brig doctor for the other checks")
}

// printHints writes the hints of results to w, each once.
func printHints(w io.Writer, results []doctor.Result) {
	hints := map[string]bool{}
	for _, r := range results {
		if r.Hint != "" && !hints[r.Hint] {
			hints[r.Hint] = true
			// Indent the continuation lines of a multi-line hint.
			fmt.Fprintf(w, "\nTo fix %s: %s\n", termtext.Escape(r.Name), strings.ReplaceAll(termtext.EscapeLines(r.Hint), "\n", "\n    "))
		}
	}
}

func newDoctorCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "doctor",
		Short: "Check whether this host can build images and run VMs",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			results := doctor.Run(cmd.Context(), doctor.System())
			tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
			for _, r := range results {
				fmt.Fprintf(tw, "%s\t%s\t%s\n", r.Status, termtext.Escape(r.Name), termtext.Escape(r.Detail))
			}
			if err := tw.Flush(); err != nil {
				return err
			}
			printHints(cmd.OutOrStdout(), results)
			if doctor.Worst(results) == doctor.Fail {
				return errors.New("this host is not ready for brig")
			}
			return nil
		},
	}
}
