// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"context"
	"errors"
	"fmt"
	"io"

	clidoctor "github.com/GSI-HPC/go-clikit/doctor"
	"github.com/spf13/cobra"

	"github.com/dennisklein/brig/internal/doctor"
)

// hostPreflight returns the failed checks of brig doctor that take no time;
// tests replace it.
var hostPreflight = func(ctx context.Context) []clidoctor.Check {
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
	if err := clidoctor.Write(w, failed); err != nil {
		return err
	}
	return errors.New("this host is not ready for brig; fix the above, then see brig doctor for the other checks")
}

func newDoctorCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "doctor",
		Short: "Check whether this host can build images and run VMs",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			results := doctor.Run(cmd.Context(), doctor.System())
			if err := clidoctor.Write(cmd.OutOrStdout(), results); err != nil {
				return err
			}
			if clidoctor.Worst(results) == clidoctor.StatusFailed {
				return errors.New("this host is not ready for brig")
			}
			return nil
		},
	}
}
