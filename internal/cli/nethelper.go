// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

package cli

import (
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
	return cmd
}
