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

	"github.com/GSI-HPC/go-clikit/progress"
	"github.com/GSI-HPC/go-clikit/termtext"

	"github.com/dennisklein/brig/internal/bytesize"
	"github.com/dennisklein/brig/internal/config"
	"github.com/dennisklein/brig/internal/guest"
	"github.com/dennisklein/brig/internal/libvirt"
	"github.com/dennisklein/brig/internal/ocsync"
	"github.com/dennisklein/brig/internal/openshell"
	"github.com/dennisklein/brig/internal/ports"
	"github.com/dennisklein/brig/internal/sshx"
	"github.com/dennisklein/brig/internal/vm"
	"github.com/dennisklein/brig/internal/vmnet"
)

const (
	sshReadyTimeout = 5 * time.Minute
	shutdownTimeout = 2 * time.Minute
)

// gatewayReadyTimeout bounds the waits for the gateway; tests shorten it.
var gatewayReadyTimeout = 3 * time.Minute

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
		Hostname:       v.Name,
		AuthorizedKey:  strings.TrimSpace(string(pub)),
		Mounts:         v.Mounts,
		FormatDataDisk: !v.DataDiskReady,
	})
	if err != nil {
		return libvirt.DomainSpec{}, err
	}
	socket, err := netSocket(v)
	if err != nil {
		return libvirt.DomainSpec{}, err
	}
	spec := libvirt.DomainSpec{
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
	}
	if len(v.Mounts) > 0 {
		if spec.MountIDMap, err = libvirt.HostMountIDMap(); err != nil {
			return libvirt.DomainSpec{}, err
		}
	}
	return spec, nil
}

// checkVM checks the settings of a new or changed VM that brig can check
// before it builds an image or changes anything: its CPUs and memory, and
// its mounts and their ID map.
func checkVM(v *vm.VM) error {
	if v.CPUs < config.MinCPUs || v.Memory < config.MinMemory {
		return fmt.Errorf("a VM needs at least %d CPU and %s of memory", config.MinCPUs, config.MinMemory)
	}
	if v.Memory%bytesize.MiB != 0 {
		return errors.New("a VM's memory must be a whole number of MiB")
	}
	if err := checkMounts(v.Mounts); err != nil {
		return err
	}
	if len(v.Mounts) > 0 {
		// Every start needs the mounts' ID map.
		_, err := libvirt.HostMountIDMap()
		return err
	}
	return nil
}

// parseMount parses a --mount argument and resolves symbolic links in its
// source, which checkMounts then expects to find none in.
func parseMount(arg string) (vm.Mount, error) {
	m, err := vm.ParseMount(arg)
	if err != nil {
		return m, err
	}
	// checkMounts reports a source that does not exist.
	if resolved, err := filepath.EvalSymlinks(m.Source); err == nil {
		m.Source = resolved
	}
	return m, nil
}

// checkMounts checks that mounts share host directories, at targets that
// brig allows in the VM. virtiofsd follows symbolic links in a source each
// time the VM starts, so a source must not have become one, or moved under
// one: a VM that can write to a parent directory, through another mount,
// could otherwise point the mount anywhere.
func checkMounts(mounts []vm.Mount) error {
	for _, m := range mounts {
		if fi, err := os.Stat(m.Source); err != nil || !fi.IsDir() {
			return fmt.Errorf("mount source %s is not a directory", m.Source)
		}
		if resolved, err := filepath.EvalSymlinks(m.Source); err != nil || resolved != m.Source {
			return fmt.Errorf("mount source %s now leads to %s through a symbolic link; if that is intended, replace the mount with `brig update --remove-mount %s --add-mount %s:%s`", m.Source, resolved, m.Target, resolved, m.Target)
		}
	}
	if err := guest.CheckMounts(mounts); err != nil {
		return err
	}
	return checkNestedMounts(mounts)
}

// checkNestedMounts checks that the mount point of each mount inside another
// mount's target exists in that mount's source. The VM cannot create it in a
// read-only mount, so the inner mount would fail, which nofail hides, and it
// would create it in the host directory through a read-write one.
func checkNestedMounts(mounts []vm.Mount) error {
	for _, m := range mounts {
		var outer *vm.Mount
		for i, o := range mounts {
			if strings.HasPrefix(m.Target, o.Target+"/") && (outer == nil || len(o.Target) > len(outer.Target)) {
				outer = &mounts[i]
			}
		}
		if outer == nil {
			continue
		}
		dir := filepath.Join(outer.Source, strings.TrimPrefix(m.Target, outer.Target+"/"))
		if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
			return fmt.Errorf("mount %s lies inside mount %s, so its mount point %s must be a directory; create it with mkdir -p %s", m, outer, dir, shellQuote(dir))
		}
	}
	return nil
}

// domains is the part of *libvirt.Conn that starting, stopping and upgrading
// a VM use; tests replace it.
type domains interface {
	State(name string) (libvirt.State, error)
	Define(domainXML string) error
	Start(name string) error
	Shutdown(ctx context.Context, name string, timeout time.Duration) error
	Destroy(name string) error
	Undefine(name string) error
}

// define (re)defines the VM's libvirt domain from its record.
func (a *app) define(conn domains, v *vm.VM) error {
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
func (a *app) start(ctx context.Context, conn domains, v *vm.VM, w io.Writer) error {
	return a.startVM(ctx, conn, v, w, false)
}

// startVM is start; with check, it fails unless the VM's gateway answers,
// as an upgrade's check boot needs.
func (a *app) startVM(ctx context.Context, conn domains, v *vm.VM, w io.Writer, check bool) error {
	state, err := conn.State(v.DomainName())
	if err != nil {
		return err
	}
	if state == libvirt.StatePaused || state == libvirt.StateOther {
		// Saved at logout or paused: shut it down and boot afresh with this
		// boot's settings rather than resume stale memory. A domain in
		// another state, such as one the guest suspended to RAM, is still
		// alive and cannot be created again; stop destroys it.
		if err := a.stop(ctx, conn, v, false); err != nil {
			return err
		}
		state = libvirt.StateShutoff
	}
	if state != libvirt.StateRunning {
		err := inSpan(ctx, progress.KindStep, "start the VM", func(ctx context.Context) error {
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
				// QEMU only says that passt's socket refused it.
				err = errors.Join(err, vmnet.Exited(ctx, v.Name))
				vmnet.Stop(context.WithoutCancel(ctx), v.Name)
				return err
			}
			fmt.Fprintf(w, "Started %s, waiting for it to boot...\n", v.Name)
			return nil
		})
		if err != nil {
			return err
		}
	}
	if err := a.boot(ctx, v); err != nil {
		return err
	}
	if err := a.connectGateway(ctx, v, w, check); err != nil {
		return err
	}
	// Steps that only warn above may have been cut short.
	return ctx.Err()
}

// boot waits until the started VM answers over SSH, which it does once it has
// booted and mounted its data disk.
func (a *app) boot(ctx context.Context, v *vm.VM) error {
	return inSpan(ctx, progress.KindStep, "boot the VM", func(ctx context.Context) error {
		err := inSpan(ctx, progress.KindWait, "wait for SSH", func(ctx context.Context) error {
			ctx, cancel := context.WithTimeout(ctx, sshReadyTimeout)
			defer cancel()
			return a.sshTarget(v, false).WaitReady(ctx)
		})
		if err != nil {
			return fmt.Errorf("%s is not reachable over SSH (boot log: %s): %w", v.Name, a.vmFile(v, consoleLogFile), err)
		}
		// SSH logs in as the data disk's owner, so the disk is mounted.
		if !v.DataDiskReady {
			v.DataDiskReady = true
			return a.vms.Save(v)
		}
		return nil
	})
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
// and registers the gateway with the host's openshell CLI. With check, it
// fails unless the gateway answers.
func (a *app) connectGateway(ctx context.Context, v *vm.VM, w io.Writer, check bool) (err error) {
	ctx, step := progress.Start(ctx, progress.KindStep, "connect the gateway")
	defer func() { step.End(err) }()
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
	if err := inSpan(ctx, progress.KindWait, "wait for the gateway's certificates", func(ctx context.Context) error {
		return waitFor(ctx, gatewayReadyTimeout, sync)
	}); err != nil {
		if errors.Is(err, openshell.ErrNoBundle) {
			return fmt.Errorf("the OpenShell gateway in %s did not create its certificates; check `brig ssh %s -- journalctl --user -u openshell-gateway`: %w", v.Name, v.Name, err)
		}
		return fmt.Errorf("copying the gateway's client certificates: %w", err)
	}
	cli, err := openshell.Find()
	if errors.Is(err, openshell.ErrNotInstalled) {
		fmt.Fprintf(w, "The openshell CLI is not installed, so the gateway is not registered. %s\n", termtext.Escape(err.Error()))
		if check {
			// Without the CLI, the gateway's service must at least run.
			return inSpan(ctx, progress.KindWait, "wait for the gateway to run", func(ctx context.Context) error {
				return waitFor(ctx, gatewayReadyTimeout, func(ctx context.Context) error {
					_, err := t.Output(ctx, "systemctl --user is-active --quiet openshell-gateway.service")
					if err != nil {
						return fmt.Errorf("the OpenShell gateway in %s is not running: %w", v.Name, err)
					}
					return nil
				})
			})
		}
		return nil
	}
	if err != nil {
		return err
	}
	if err := cli.Register(ctx, v.GatewayName(), v.Ports.Gateway); err != nil {
		return fmt.Errorf("registering the gateway: %w", err)
	}
	answers := a.checkVersions(ctx, cli, v, sync, w, check)
	if check && !answers {
		return fmt.Errorf("the OpenShell gateway in %s does not answer; check `brig ssh %s -- journalctl --user -u openshell-gateway`", v.Name, v.Name)
	}
	fmt.Fprintf(w, "OpenShell gateway registered as %[1]s. Use it with `brig use %[2]s` (see brig shell-init --help) or `eval \"$(brig env %[2]s)\"`, which also set the default sandbox policy, or with `openshell -g %[1]s ...`.\n", v.GatewayName(), v.Name)
	if len(v.OpenShellConfigs) > 0 {
		if !answers {
			fmt.Fprintf(w, "Run `brig sync %s` to apply its OpenShell config directories once the gateway answers.\n", v.Name)
			return nil
		}
		err := inOptionalSpan(ctx, progress.KindStep, "apply the OpenShell config directories", func(ctx context.Context) error {
			// A keyring that waits for an unlock must not hold up the start
			// for long.
			ctx, cancel := context.WithTimeout(ctx, startSyncTimeout)
			defer cancel()
			return a.syncOpenShell(ctx, cli, v, ocsync.Options{Out: w, HoldNewEndpoints: true}, "")
		})
		if err != nil {
			fmt.Fprintf(w, "Warning: applying the OpenShell config directories failed; fix it and run `brig sync %s`: %s\n", v.Name, termtext.Escape(err.Error()))
		}
	}
	return nil
}

// startSyncTimeout bounds the sync of a VM's OpenShell config directories
// when it starts.
const startSyncTimeout = 3 * time.Minute

// checkVersions waits until the gateway answers and warns when the host CLI
// and the gateway differ in their minor version, which OpenShell does not
// support. While the gateway does not answer, it copies the client
// certificates again with sync: a gateway regenerates its whole PKI on start
// when its certificates lack a name its version requires, e.g. after an
// upgrade, and an earlier copy may then be stale. It reports whether the
// gateway answers.
func (a *app) checkVersions(ctx context.Context, cli *openshell.CLI, v *vm.VM, sync func(context.Context) error, w io.Writer, check bool) bool {
	host, err := cli.Version(ctx)
	if err != nil {
		return false
	}
	var gw string
	// A gateway that does not answer fails the start only with check.
	span := inSpan
	if !check {
		span = inOptionalSpan
	}
	_ = span(ctx, progress.KindWait, "wait for the gateway to answer", func(ctx context.Context) error {
		return waitFor(ctx, gatewayReadyTimeout, func(ctx context.Context) error {
			if gw, err = cli.GatewayVersion(ctx, v.GatewayName()); err != nil {
				_ = sync(ctx)
			}
			return err
		})
	})
	if gw == "" {
		fmt.Fprintf(w, "Warning: the gateway %s does not answer yet; check `openshell -g %s status` later.\n", v.GatewayName(), v.GatewayName())
		return false
	}
	if !openshell.CompatibleVersions(host, gw) {
		fmt.Fprintf(w, "Warning: the openshell CLI is version %s but the gateway in %s runs %s; install the matching CLI version.\n", termtext.Escape(host), v.Name, termtext.Escape(gw))
	}
	return true
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
func (a *app) stop(ctx context.Context, conn domains, v *vm.VM, force bool) error {
	state, err := conn.State(v.DomainName())
	if err != nil {
		return err
	}
	switch {
	case state == libvirt.StateMissing || state == libvirt.StateShutoff:
		vmnet.Stop(ctx, v.Name)
		return nil
	case force:
		err = inSpan(ctx, progress.KindStep, "power off the VM", func(context.Context) error {
			return conn.Destroy(v.DomainName())
		})
	default:
		err = inSpan(ctx, progress.KindStep, "shut down the VM", func(ctx context.Context) error {
			if state == libvirt.StatePaused {
				// A domain saved at logout restores, and then shuts down cleanly,
				// only while its network listens; otherwise Shutdown powers it off.
				if net, nerr := a.netConfig(v); nerr == nil {
					_ = vmnet.Start(ctx, net)
				}
			}
			return conn.Shutdown(ctx, v.DomainName(), shutdownTimeout)
		})
	}
	if err == nil {
		vmnet.Stop(ctx, v.Name)
	}
	return err
}
