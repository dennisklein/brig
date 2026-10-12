// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"os"

	"github.com/spf13/cobra"

	"github.com/dennisklein/brig/internal/paths"
	"github.com/dennisklein/brig/internal/ui"
)

func newUICmd() *cobra.Command {
	var o ui.Options
	cmd := &cobra.Command{
		Use:   "ui",
		Short: "Open a local web UI for brig's VMs",
		Long: `ui serves a web UI on 127.0.0.1 and opens it in your browser. It shows
your VMs, their OpenShell sandboxes and traffic, the host checks and what
brig did, and updates live. It also changes things: each dialog shows the
brig or openshell command it runs, and runs exactly that.

Only you can open it: it prints a URL with a token that is new each run,
and it refuses other users' connections. A second brig ui opens the
running one instead.

--demo serves made-up VMs and touches nothing on this host.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			dirs, err := paths.Default()
			if err != nil {
				return err
			}
			o.Dirs = dirs
			o.ActivityLog = dirs.ActivityLog()
			o.RuntimeDir = os.Getenv("XDG_RUNTIME_DIR")
			o.Stdout, o.Stderr = cmd.OutOrStdout(), cmd.ErrOrStderr()
			return ui.Run(cmd.Context(), o)
		},
	}
	f := cmd.Flags()
	f.IntVar(&o.Port, "port", 0, "port on 127.0.0.1 to listen on (default: a free one)")
	f.BoolVar(&o.NoOpen, "no-open", false, "print the URL without opening a browser")
	f.BoolVar(&o.Demo, "demo", false, "serve made-up VMs, for a look around or screenshots")
	f.StringVar(&o.DevAssets, "dev-assets", "", "serve templates and static files from this directory (internal/ui/web), for working on them")
	_ = f.MarkHidden("dev-assets")
	hideDefaults(cmd, "port")
	return cmd
}
