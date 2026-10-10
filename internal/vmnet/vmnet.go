// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

// Package vmnet runs a VM's user-mode network outside the VM, where nothing
// in the VM can change it:
//
//	VM ──vhost-user── passt ── private netns + nftables ── pasta ── host
//
// pasta connects a private network namespace to the host's network and
// forwards the VM's SSH and gateway ports from the host's loopback interface
// into it. Inside the namespace, `brig net-helper` loads nftables rules that
// enforce the VM's network profile and then becomes passt, which serves the
// VM over a vhost-user socket. Everything runs unprivileged, in a transient
// systemd user unit per VM.
package vmnet

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/dennisklein/brig/internal/config"
)

// DNSAddr is the resolver address the VM is given. pasta forwards DNS
// queries to it to the host's first nameserver, which also works when that
// is a loopback stub such as systemd-resolved's 127.0.0.53.
const DNSAddr = "169.254.1.1"

// nsSSHPort is the namespace port that pasta forwards the host's SSH port
// to and passt forwards on to the guest's sshd.
const nsSSHPort = 2222

// Config describes one VM's network.
type Config struct {
	// VM is the VM's name.
	VM string
	// Socket is the vhost-user socket passt listens on and QEMU connects to.
	Socket string
	// SSHPort and GatewayPort are the host loopback ports forwarded to
	// GuestSSHPort and GuestGatewayPort in the VM.
	SSHPort, GatewayPort           int
	GuestSSHPort, GuestGatewayPort int
	// Profile restricts what the VM may reach.
	Profile config.NetworkProfile
	// HostAddrs are the host's interface addresses with their prefix
	// lengths, which Ruleset keeps the VM from reaching as the host and as
	// the LAN. Start reads them from the host, so they are those of the
	// networks the host is in when the VM starts.
	HostAddrs []netip.Prefix
	// Routes are the destinations of the host's routes, which Ruleset also
	// counts as the LAN. Start reads them when the VM starts.
	Routes []netip.Prefix
}

// UnitName is the systemd user unit that runs the VM's network.
func UnitName(vm string) string { return "brig-net-" + vm + ".service" }

func (c Config) validate() error {
	if c.VM == "" || !filepath.IsAbs(c.Socket) {
		return errors.New("vmnet: VM name and an absolute socket path are required")
	}
	for _, p := range []int{c.SSHPort, c.GatewayPort, c.GuestSSHPort, c.GuestGatewayPort} {
		if p < 1 || p > 65535 {
			return fmt.Errorf("vmnet: invalid port %d", p)
		}
	}
	if c.GuestGatewayPort == nsSSHPort {
		return fmt.Errorf("vmnet: guest gateway port %d collides with the namespace SSH port", nsSSHPort)
	}
	return nil
}

// pastaArgs returns pasta's arguments, up to and including "--".
func (c Config) pastaArgs() []string {
	args := []string{
		"--config-net",
		"--quiet",
		"--dns-forward", DNSAddr,
		// Bind the forwards to the host's loopback only. pasta's defaults
		// would forward every port the namespace binds, in both directions.
		"-t", fmt.Sprintf("127.0.0.1/%d:%d", c.SSHPort, nsSSHPort),
		"-t", fmt.Sprintf("127.0.0.1/%d:%d", c.GatewayPort, c.GuestGatewayPort),
		"-u", "none",
		"-T", "none",
		"-U", "none",
	}
	if !c.Profile.IPv6 {
		args = append(args, "-4")
	}
	return append(args, "--")
}

// passtArgs returns passt's argv; passt runs inside the namespace.
func (c Config) passtArgs(passt string) []string {
	args := []string{
		passt,
		"--foreground",
		"--vhost-user",
		"--socket", c.Socket,
		// Exit when QEMU disconnects, i.e. when the VM shuts down; a reboot
		// inside the VM keeps the connection.
		"--one-off",
		// The VM's traffic to its gateway must leave the namespace like any
		// other traffic, so the firewall sees it and pasta maps it to the
		// host's loopback.
		"--map-host-loopback", "none",
		"--dns", DNSAddr,
		"-t", fmt.Sprintf("%d:%d", nsSSHPort, c.GuestSSHPort),
		"-t", fmt.Sprintf("%d:%d", c.GuestGatewayPort, c.GuestGatewayPort),
		"-u", "none",
	}
	if !c.Profile.IPv6 {
		args = append(args, "-4")
	}
	return args
}

// helperArgs returns the arguments of `brig net-helper` for this network.
func (c Config) helperArgs() ([]string, error) {
	profile, err := encodeProfile(c.Profile)
	if err != nil {
		return nil, err
	}
	args := []string{
		"net-helper",
		"--socket", c.Socket,
		"--guest-ssh-port", strconv.Itoa(c.GuestSSHPort),
		"--guest-gateway-port", strconv.Itoa(c.GuestGatewayPort),
		"--profile", profile,
	}
	for _, p := range c.HostAddrs {
		args = append(args, "--host-addr", p.String())
	}
	for _, p := range c.Routes {
		args = append(args, "--route", p.String())
	}
	return args, nil
}

// Command returns the full command line of the VM's network unit: pasta,
// which runs brig (at path brig) as net-helper in its namespace.
func (c Config) Command(pasta, brig string) ([]string, error) {
	if err := c.validate(); err != nil {
		return nil, err
	}
	helper, err := c.helperArgs()
	if err != nil {
		return nil, err
	}
	argv := append([]string{pasta}, c.pastaArgs()...)
	// pasta's SELinux policy runs shells unconfined but would keep a brig
	// binary in the home directory (where mise installs it) confined to
	// pasta's own domain, which may not manage nftables. Exec through the
	// shell to get the shell's transition.
	argv = append(argv, "/bin/sh", "-c", `exec "$0" "$@"`, brig)
	return append(argv, helper...), nil
}

// Start (re)starts the VM's network unit and waits until passt listens on
// its socket.
func Start(ctx context.Context, c Config) error {
	pasta, err := exec.LookPath("pasta")
	if err != nil {
		return fmt.Errorf("pasta is not installed (package passt): %w", err)
	}
	brig, err := os.Executable()
	if err != nil {
		return err
	}
	if c.HostAddrs, err = hostAddrs(); err != nil {
		return err
	}
	if c.Routes, err = ReadRoutes(); err != nil {
		return err
	}
	if err := checkIPv4Gateway(c.Routes); err != nil {
		return err
	}
	argv, err := c.Command(pasta, brig)
	if err != nil {
		return err
	}
	Stop(ctx, c.VM)
	if err := os.MkdirAll(filepath.Dir(c.Socket), 0o700); err != nil {
		return err
	}
	if err := os.Remove(c.Socket); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	unit := UnitName(c.VM)
	run := append([]string{
		"--user", "--quiet", "--collect",
		"--unit=" + unit,
		"--description=brig network of VM " + c.VM,
		"--property=Type=exec",
		"--",
	}, argv...)
	if out, err := exec.CommandContext(ctx, "systemd-run", run...).CombinedOutput(); err != nil {
		return fmt.Errorf("starting %s: %w: %s", unit, err, strings.TrimSpace(string(out)))
	}
	return waitForSocket(ctx, c.Socket, unit)
}

// checkIPv4Gateway fails when the host has IPv4 routes but no IPv4 default
// gateway, e.g. offline with a container bridge up or on a PPP link. pasta
// then copies the routes into the namespace without a gateway, and passt
// hands the VM no address, so it would never become reachable. Without any
// IPv4 route, pasta uses a link-local network of its own instead.
func checkIPv4Gateway(routes []netip.Prefix) error {
	if !slices.ContainsFunc(routes, func(p netip.Prefix) bool { return p.Addr().Is4() }) {
		return nil
	}
	gws, err := ReadGateways()
	if err != nil {
		return err
	}
	if len(gws.IPv4) == 0 {
		return errors.New("the host has no IPv4 default gateway, so the VM's network cannot give it an address; connect the host to a network with one, or take down interfaces such as container bridges while offline")
	}
	return nil
}

// hostAddrs returns the addresses of the host's network interfaces, with
// their prefix lengths, other than loopback addresses. An address added to
// the loopback interface is the host's like any other.
func hostAddrs() ([]netip.Prefix, error) {
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil, fmt.Errorf("listing the host's network interfaces: %w", err)
	}
	var prefixes []netip.Prefix
	for _, iface := range ifaces {
		addrs, err := iface.Addrs()
		if err != nil {
			return nil, fmt.Errorf("listing the addresses of %s: %w", iface.Name, err)
		}
		for _, a := range addrs {
			if p, ok := toPrefix(a); ok && !p.Addr().IsLoopback() {
				prefixes = append(prefixes, p)
			}
		}
	}
	return prefixes, nil
}

// toPrefix converts an interface address to a prefix.
func toPrefix(a net.Addr) (netip.Prefix, bool) {
	ipnet, ok := a.(*net.IPNet)
	if !ok {
		return netip.Prefix{}, false
	}
	addr, ok := netip.AddrFromSlice(ipnet.IP)
	if !ok {
		return netip.Prefix{}, false
	}
	ones, bits := ipnet.Mask.Size()
	if addr.Is4In6() && bits == 128 {
		ones -= 96
	}
	p := netip.PrefixFrom(addr.Unmap(), ones)
	return p, p.IsValid()
}

func waitForSocket(ctx context.Context, socket, unit string) error {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	for {
		if fi, err := os.Stat(socket); err == nil && fi.Mode()&os.ModeSocket != 0 {
			return nil
		}
		if exec.CommandContext(ctx, "systemctl", "--user", "is-failed", "--quiet", unit).Run() == nil {
			return fmt.Errorf("%s failed: %s", unit, journal(unit))
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("%s did not create %s: %w: %s", unit, socket, ctx.Err(), journal(unit))
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// journal returns the last log lines of a user unit, for error messages.
func journal(unit string) string {
	out, err := exec.Command("journalctl", "--user", "--unit", unit, "--lines", "15", "--no-pager", "--output", "cat").Output()
	if err != nil {
		return "see journalctl --user -u " + unit
	}
	return strings.TrimSpace(string(out))
}

// Stop stops the VM's network unit if it runs. passt usually exits on its
// own when the VM shuts down.
func Stop(ctx context.Context, vm string) {
	unit := UnitName(vm)
	// Errors only mean that the unit is not loaded. Like qemu-img's, these
	// commands are kept from the terminal's Ctrl-C, for rollbacks.
	for _, args := range [][]string{{"stop", unit}, {"reset-failed", unit}} {
		cmd := exec.CommandContext(ctx, "systemctl", append([]string{"--user"}, args...)...)
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		_ = cmd.Run()
	}
}
