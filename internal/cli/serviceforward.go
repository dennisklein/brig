// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"context"
	"fmt"
	"io"
	"log"
	"net"
	"strconv"
	"sync"

	"github.com/spf13/cobra"

	"github.com/dennisklein/brig/internal/openshell"
	"github.com/dennisklein/brig/internal/osclient"
	"github.com/dennisklein/brig/internal/svcforward"
	"github.com/dennisklein/brig/internal/vm"
)

func newServiceForwardCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:    "forward NAME",
		Short:  "Serve the host port of a service whose workload runs in a sandbox",
		Hidden: true,
		Long: `Listen on the service's port on the host's loopback interface and pass each
connection on, through the service VM's OpenShell gateway, to the port of
the sandbox where the workload listens. brig runs this in a user unit at
every start of the service VM; it is not meant to be run by hand.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			a, err := newApp()
			if err != nil {
				return err
			}
			svc, err := a.loadService(args[0])
			if err != nil {
				return err
			}
			t, err := serviceType(svc.Type)
			if err != nil {
				return err
			}
			if t.Sandbox == nil {
				return fmt.Errorf("service %s of type %s listens in its VM, not in a sandbox; brig forwards its port without this", svc.Name, t.Name)
			}
			v, err := a.vms.Load(svc.Name)
			if err != nil {
				return err
			}
			l, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(svc.Port)))
			if err != nil {
				return err
			}
			logger := log.New(cmd.ErrOrStderr(), "", 0)
			// The type's manifest is checked: the port is a TCP port.
			d := &sandboxDialer{vm: v, sandbox: t.Sandbox.Name, port: uint32(t.Sandbox.Port)} //nolint:gosec // G115: 1 to 65535
			defer d.close()
			logger.Printf("forwarding 127.0.0.1:%d to port %d of sandbox %s in VM %s", svc.Port, t.Sandbox.Port, t.Sandbox.Name, v.Name)
			return svcforward.Serve(cmd.Context(), l, d.dial, logger.Printf)
		},
	}
	cmd.Annotations = map[string]string{noProgress: "true"}
	return cmd
}

// sandboxDialer opens connections to a port of a sandbox in the default
// workspace of a VM's gateway. It keeps one client of the gateway, which it
// dials again when the client bundle changed, as at each start of the VM,
// or when a connection fails.
type sandboxDialer struct {
	vm      *vm.VM
	sandbox string
	port    uint32

	mu sync.Mutex
	c  *osclient.Client
}

func (d *sandboxDialer) dial(ctx context.Context) (io.ReadWriteCloser, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	var err error
	for range 2 {
		if d.c != nil && d.c.BundleChanged() {
			d.closeLocked()
		}
		if d.c == nil {
			if d.c, err = gatewayClient(d.vm); err != nil {
				d.c = nil
				return nil, err
			}
		}
		var rwc io.ReadWriteCloser
		if rwc, err = d.c.ForwardTCP(ctx, openshell.DefaultWorkspace, d.sandbox, d.port); err == nil {
			return rwc, nil
		}
		// The gateway may have restarted with the VM: dial it again.
		d.closeLocked()
	}
	return nil, err
}

func (d *sandboxDialer) close() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.closeLocked()
}

func (d *sandboxDialer) closeLocked() {
	if d.c != nil {
		_ = d.c.Close()
		d.c = nil
	}
}
