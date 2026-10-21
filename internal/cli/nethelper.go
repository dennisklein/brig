// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"errors"
	"fmt"
	"os/exec"
	"strconv"

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
	f.IntVar(&o.GuestServicePort, "guest-service-port", 0, "guest port of a service VM's workload")
	f.StringVar(&o.ServicePorts, "service-ports", "", "file that lists the host ports of attached services")
	cmd.AddCommand(newNetWatchCmd(), newNetReloadCmd())
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
	f.StringVar(&o.ServicePorts, "service-ports", "", "file that lists the host ports of attached services")
	f.Var(portList{&o.LoadedServicePorts}, "loaded-service-port", "host port of an attached service that the firewall was loaded with (repeatable)")
	return cmd
}

// portList is a repeatable flag of TCP ports.
type portList struct{ ports *[]uint16 }

func (l portList) String() string {
	if l.ports == nil {
		return ""
	}
	return fmt.Sprint(*l.ports)
}

func (l portList) Set(v string) error {
	n, err := strconv.ParseUint(v, 10, 16)
	if err != nil || n == 0 {
		return fmt.Errorf("invalid port %q", v)
	}
	*l.ports = append(*l.ports, uint16(n))
	return nil
}

func (portList) Type() string { return "port" }
