// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"errors"
	"os"
	"os/exec"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/dennisklein/brig/internal/vmnet"
)

// newNetHelperCmd is the process pasta starts in a VM's network namespace;
// see package vmnet. It is not meant to be run by hand.
func newNetHelperCmd() *cobra.Command {
	var o vmnet.HelperOptions
	cmd := &cobra.Command{
		Use:    "net-helper",
		Short:  "Load a VM's firewall and run passt (internal)",
		Hidden: true,
		Args:   cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error {
			return vmnet.RunHelper(o)
		},
	}
	f := cmd.Flags()
	f.StringVar(&o.Socket, "socket", "", "vhost-user socket path")
	f.IntVar(&o.GuestSSHPort, "guest-ssh-port", 0, "guest SSH port")
	f.IntVar(&o.GuestGatewayPort, "guest-gateway-port", 0, "guest OpenShell gateway port")
	f.StringVar(&o.Profile, "profile", "", "network profile as JSON")
	f.StringArrayVar(&o.HostAddrs, "host-addr", nil, "host address with prefix length (repeatable)")
	f.StringArrayVar(&o.Routes, "route", nil, "destination of a host route (repeatable)")
	f.StringVar(&o.HostNet, "host-net", "", "file the watcher writes the host's network to")
	f.StringVar(&o.Spool, "spool", "", "spool of the fence 2 listener")
	cmd.AddCommand(newNetWatchCmd(), newNetReloadCmd(), newFence2LogCmd())
	return cmd
}

// newNetWatchCmd is the network unit's main process, on the host: it runs
// pasta and writes the host's network to a file when it changes.
func newNetWatchCmd() *cobra.Command {
	var path string
	cmd := &cobra.Command{
		Use:    "watch --host-net FILE -- PASTA [ARG...]",
		Short:  "Run pasta and follow the host's network (internal)",
		Hidden: true,
		Args:   cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			err := vmnet.RunWatcher(cmd.Context(), path, args)
			// pasta has said why.
			if exit, ok := errors.AsType[*exec.ExitError](err); ok {
				return exitError{code: max(exit.ExitCode(), 1)}
			}
			return err
		},
	}
	cmd.Flags().StringVar(&path, "host-net", "", "file to write the host's network to")
	return cmd
}

// newNetReloadCmd runs in the namespace beside passt, and loads the
// firewall again when the host's network changes.
func newNetReloadCmd() *cobra.Command {
	var o vmnet.HelperOptions
	cmd := &cobra.Command{
		Use:    "reload",
		Short:  "Load a VM's firewall again when the host's network changes (internal)",
		Hidden: true,
		Args:   cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return vmnet.RunReloader(cmd.Context(), o)
		},
	}
	f := cmd.Flags()
	f.StringVar(&o.Profile, "profile", "", "network profile as JSON")
	f.StringArrayVar(&o.HostAddrs, "host-addr", nil, "host address with prefix length the firewall was loaded with (repeatable)")
	f.StringArrayVar(&o.Routes, "route", nil, "destination of a host route the firewall was loaded with (repeatable)")
	f.StringVar(&o.HostNet, "host-net", "", "file the watcher writes the host's network to")
	f.StringVar(&o.Fence2, "fence2", "", "how the firewall records drops: itemised, counts only or off")
	return cmd
}

// newFence2LogCmd is the listener that the net-helper starts beside passt,
// to record what the firewall drops; see package fence2.
func newFence2LogCmd() *cobra.Command {
	var o vmnet.ListenerOptions
	cmd := &cobra.Command{
		Use:    "fence2-log",
		Short:  "Record what a VM's firewall drops (internal)",
		Hidden: true,
		Args:   cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			return vmnet.RunListener(ctx, o)
		},
	}
	f := cmd.Flags()
	f.StringVar(&o.Spool, "spool", "", "spool to write the drops to")
	f.StringVar(&o.Mode, "mode", "", "what the firewall records")
	f.StringVar(&o.Why, "why", "", "why it does not itemise drops")
	return cmd
}
