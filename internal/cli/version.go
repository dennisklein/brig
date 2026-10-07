// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"fmt"
	"runtime"

	"github.com/spf13/cobra"

	"github.com/dennisklein/brig/internal/version"
)

func newVersionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print the brig version",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			_, err := fmt.Fprintf(cmd.OutOrStdout(), "brig %s (%s, %s/%s)\n",
				version.Version(), runtime.Version(), runtime.GOOS, runtime.GOARCH)
			return err
		},
	}
}
