// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/dennisklein/brig/internal/config"
	"github.com/dennisklein/brig/internal/guest"
	"github.com/dennisklein/brig/internal/libvirt"
	"github.com/dennisklein/brig/internal/openshell"
	"github.com/dennisklein/brig/internal/ports"
	"github.com/dennisklein/brig/internal/sshx"
	"github.com/dennisklein/brig/internal/vm"
	"github.com/dennisklein/brig/internal/vmnet"
)

const (
	sshReadyTimeout     = 5 * time.Minute
	gatewayReadyTimeout = 3 * time.Minute
	shutdownTimeout     = 2 * time.Minute
)

// Files in a VM's directory.
const (
	rootDiskFile   = "root.qcow2"
	dataDiskFile   = "data.qcow2"
	clientKeyFile  = "id_ed25519"
	hostKeyFile    = "ssh_host_ed25519_key"
	knownHostsFile = "known_hosts"
	consoleLogFile = "console.log"
)

func (a *app) vmFile(v *vm.VM, name string) string {
	return filepath.Join(a.vms.Dir(v.Name), name)
}

// netSocket is the vhost-user socket of the VM's network, which lives in
// $XDG_RUNTIME_DIR because passt listens on it only while the VM runs.
func netSocket(v *vm.VM) (string, error) {
	dir := os.Getenv("XDG_RUNTIME_DIR")
	if dir == "" {
		return "", errors.New("XDG_RUNTIME_DIR is not set; run brig from a regular login session")
	}
	return filepath.Join(dir, "brig", v.Name, "net.sock"), nil
}

// netConfig describes the VM's network for package vmnet.
func (a *app) netConfig(v *vm.VM) (vmnet.Config, error) {
	profile, err := a.cfg.Profile(v.NetworkProfile)
	if err != nil {
		return vmnet.Config{}, err
	}
	socket, err := netSocket(v)
	if err != nil {
		return vmnet.Config{}, err
	}
	return vmnet.Config{
		VM:               v.Name,
		Socket:           socket,
		SSHPort:          v.Ports.SSH,
		GatewayPort:      v.Ports.Gateway,
		GuestSSHPort:     guest.SSHPort,
		GuestGatewayPort: guest.GatewayPort,
		Profile:          profile,
	}, nil
}

// sshTarget addresses the VM's sshd through its forwarded port.
func (a *app) sshTarget(v *vm.VM, root bool) sshx.Target {
	user := guest.User
	if root {
		user = "root"
	}
	return sshx.Target{
		Host:           "127.0.0.1",
		Port:           v.Ports.SSH,
		User:           user,
		IdentityFile:   a.vmFile(v, clientKeyFile),
		KnownHostsFile: a.vmFile(v, knownHostsFile),
		HostKeyAlias:   v.DomainName(),
	}
}

// domainSpec derives the libvirt domain from the VM record and its files.
func (a *app) domainSpec(v *vm.VM) (libvirt.DomainSpec, error) {
	pub, err := os.ReadFile(a.vmFile(v, clientKeyFile+".pub"))
	if err != nil {
		return libvirt.DomainSpec{}, err
	}
	if err := checkMounts(v.Mounts); err != nil {
		return libvirt.DomainSpec{}, err
	}
	creds, err := guest.Credentials(guest.BootConfig{
		Hostname:      v.Name,
		AuthorizedKey: strings.TrimSpace(string(pub)),
		Mounts:        v.Mounts,
	})
	if err != nil {
		return libvirt.DomainSpec{}, err
	}
	socket, err := netSocket(v)
	if err != nil {
		return libvirt.DomainSpec{}, err
	}
	return libvirt.DomainSpec{
		Name:        v.DomainName(),
		UUID:        v.UUID,
		CPUs:        v.CPUs,
		Memory:      v.Memory,
		RootDisk:    a.vmFile(v, rootDiskFile),
		BaseImage:   a.images.Path(v.Image),
		DataDisk:    a.vmFile(v, dataDiskFile),
		NetSocket:   socket,
		Mounts:      v.Mounts,
		ConsoleLog:  a.vmFile(v, consoleLogFile),
		Credentials: creds,
		SecretCredentialFiles: map[string]string{
			guest.HostKeyCredential: a.vmFile(v, hostKeyFile),
		},
	}, nil
}

// checkVM checks the settings of a new or changed VM that brig can check
// before it builds an image or changes anything: its CPUs and memory, and
// its mounts.
func checkVM(v *vm.VM) error {
	if v.CPUs < config.MinCPUs || v.Memory < config.MinMemory {
		return fmt.Errorf("a VM needs at least %d CPU and %s of memory", config.MinCPUs, config.MinMemory)
	}
	return checkMounts(v.Mounts)
}

// checkMounts checks that mounts share host directories, at targets that
// brig allows in the VM.
func checkMounts(mounts []vm.Mount) error {
	for _, m := range mounts {
		if fi, err := os.Stat(m.Source); err != nil || !fi.IsDir() {
			return fmt.Errorf("mount source %s is not a directory", m.Source)
		}
	}
	return guest.CheckMounts(mounts)
}

// define (re)defines the VM's libvirt domain from its record.
func (a *app) define(conn *libvirt.Conn, v *vm.VM) error {
	spec, err := a.domainSpec(v)
	if err != nil {
		return err
	}
	xml, err := libvirt.DomainXML(spec)
	if err != nil {
		return err
	}
	return conn.Define(xml)
}

// start boots a stopped VM and connects its gateway to the host's openshell
// CLI. A running VM is only reconnected.
func (a *app) start(ctx context.Context, conn *libvirt.Conn, v *vm.VM, w io.Writer) error {
	state, err := conn.State(v.DomainName())
	if err != nil {
		return err
	}
	if state == libvirt.StatePaused {
		// Saved at logout or paused: shut it down and boot afresh with this
		// boot's settings rather than resume stale memory.
		if err := a.stop(ctx, conn, v, false); err != nil {
			return err
		}
		state = libvirt.StateShutoff
	}
	if state != libvirt.StateRunning {
		if err := a.reservePorts(v); err != nil {
			return err
		}
		if err := a.define(conn, v); err != nil {
			return err
		}
		net, err := a.netConfig(v)
		if err != nil {
			return err
		}
		if err := vmnet.Start(ctx, net); err != nil {
			return fmt.Errorf("starting the network of %s: %w", v.Name, err)
		}
		if err := conn.Start(v.DomainName()); err != nil {
			vmnet.Stop(context.WithoutCancel(ctx), v.Name)
			return err
		}
		fmt.Fprintf(w, "Started %s, waiting for it to boot...\n", v.Name)
	}
	wctx, cancel := context.WithTimeout(ctx, sshReadyTimeout)
	defer cancel()
	if err := a.sshTarget(v, false).WaitReady(wctx); err != nil {
		return fmt.Errorf("%s is not reachable over SSH (boot log: %s): %w", v.Name, a.vmFile(v, consoleLogFile), err)
	}
	return a.connectGateway(ctx, v, w)
}

// reservePorts moves forwarded ports that another program took meanwhile,
// keeping the two ports distinct.
func (a *app) reservePorts(v *vm.VM) error {
	changed := false
	for _, p := range []*int{&v.Ports.SSH, &v.Ports.Gateway} {
		for *p == 0 || !ports.Available(*p) || v.Ports.SSH == v.Ports.Gateway {
			free, err := ports.Free()
			if err != nil {
				return err
			}
			*p, changed = free, true
		}
	}
	if changed {
		return a.vms.Save(v)
	}
	return nil
}

// allocatePorts picks two distinct free ports.
func allocatePorts() (vm.Ports, error) {
	var p vm.Ports
	var err error
	if p.SSH, err = ports.Free(); err != nil {
		return p, err
	}
	for p.Gateway == 0 || p.Gateway == p.SSH {
		if p.Gateway, err = ports.Free(); err != nil {
			return p, err
		}
	}
	return p, nil
}

// openshellConfigHome is where the openshell CLI keeps its configuration.
func openshellConfigHome() (string, error) {
	if dir := os.Getenv("XDG_CONFIG_HOME"); dir != "" {
		return filepath.Join(dir, "openshell"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".config", "openshell"), nil
}

// connectGateway copies the gateway's client certificate bundle to the host
// and registers the gateway with the host's openshell CLI.
func (a *app) connectGateway(ctx context.Context, v *vm.VM, w io.Writer) error {
	t := a.sshTarget(v, false)
	cfgHome, err := openshellConfigHome()
	if err != nil {
		return err
	}
	sync := func(ctx context.Context) error {
		_, err := openshell.SyncBundle(ctx, t, cfgHome, v.GatewayName())
		return err
	}
	// The gateway creates its PKI when its user service first starts.
	if err := waitFor(ctx, gatewayReadyTimeout, sync); err != nil {
		if errors.Is(err, openshell.ErrNoBundle) {
			return fmt.Errorf("the OpenShell gateway in %s did not create its certificates; check `brig ssh %s -- journalctl --user -u openshell-gateway`: %w", v.Name, v.Name, err)
		}
		return fmt.Errorf("copying the gateway's client certificates: %w", err)
	}
	cli, err := openshell.Find()
	if errors.Is(err, openshell.ErrNotInstalled) {
		fmt.Fprintf(w, "The openshell CLI is not installed, so the gateway is not registered. %v\n", err)
		return nil
	}
	if err != nil {
		return err
	}
	if err := cli.Register(ctx, v.GatewayName(), v.Ports.Gateway); err != nil {
		return fmt.Errorf("registering the gateway: %w", err)
	}
	a.checkVersions(ctx, cli, v, sync, w)
	fmt.Fprintf(w, "OpenShell gateway registered as %[1]s. Use it with `openshell -g %[1]s ...` or `eval $(brig env %[2]s)`.\n", v.GatewayName(), v.Name)
	return nil
}

// checkVersions waits until the gateway answers and warns when the host CLI
// and the gateway differ in their minor version, which OpenShell does not
// support. While the gateway does not answer, it copies the client
// certificates again with sync: a gateway regenerates its whole PKI on start
// when its certificates lack a name its version requires, e.g. after an
// upgrade, and an earlier copy may then be stale.
func (a *app) checkVersions(ctx context.Context, cli *openshell.CLI, v *vm.VM, sync func(context.Context) error, w io.Writer) {
	host, err := cli.Version(ctx)
	if err != nil {
		return
	}
	var gw string
	_ = waitFor(ctx, gatewayReadyTimeout, func(ctx context.Context) error {
		if gw, err = cli.GatewayVersion(ctx, v.GatewayName()); err != nil {
			_ = sync(ctx)
		}
		return err
	})
	if gw == "" {
		fmt.Fprintf(w, "Warning: the gateway %s does not answer yet; check `openshell -g %s status` later.\n", v.GatewayName(), v.GatewayName())
		return
	}
	if !openshell.CompatibleVersions(host, gw) {
		fmt.Fprintf(w, "Warning: the openshell CLI is version %s but the gateway in %s runs %s; install the matching CLI version.\n", host, v.Name, gw)
	}
}

// waitFor retries fn with a growing pause until it succeeds or timeout.
func waitFor(ctx context.Context, timeout time.Duration, fn func(context.Context) error) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	pause := time.Second
	for {
		err := fn(ctx)
		if err == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("%w (last error: %w)", ctx.Err(), err)
		case <-time.After(pause):
		}
		pause = min(2*pause, 5*time.Second)
	}
}

// stop shuts the VM down, gracefully unless force is set, and stops its
// network.
func (a *app) stop(ctx context.Context, conn *libvirt.Conn, v *vm.VM, force bool) error {
	state, err := conn.State(v.DomainName())
	if err != nil {
		return err
	}
	switch {
	case state == libvirt.StateMissing || state == libvirt.StateShutoff:
		vmnet.Stop(ctx, v.Name)
		return nil
	case force:
		err = conn.Destroy(v.DomainName())
	default:
		if state == libvirt.StatePaused {
			// A domain saved at logout restores, and then shuts down cleanly,
			// only while its network listens; otherwise Shutdown powers it off.
			if net, nerr := a.netConfig(v); nerr == nil {
				_ = vmnet.Start(ctx, net)
			}
		}
		err = conn.Shutdown(ctx, v.DomainName(), shutdownTimeout)
	}
	if err == nil {
		vmnet.Stop(ctx, v.Name)
	}
	return err
}
