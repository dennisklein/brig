// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

package vmnet

import (
	"fmt"
	"net/netip"
	"os"
	"os/exec"
	"strings"
	"syscall"
)

// HelperOptions are the arguments of `brig net-helper`.
type HelperOptions struct {
	Socket                         string
	GuestSSHPort, GuestGatewayPort int
	Profile                        string
	// HostAddrs are the host's addresses with prefix lengths, as in
	// Config.HostAddrs.
	HostAddrs []string
}

// RunHelper runs inside pasta's namespace: it loads the firewall for the
// profile and then replaces itself with passt. If the firewall cannot be
// loaded, passt never starts and the VM has no network.
func RunHelper(o HelperOptions) error {
	profile, err := decodeProfile(o.Profile)
	if err != nil {
		return err
	}
	c := Config{
		VM: "helper", Socket: o.Socket, SSHPort: 1, GatewayPort: 1,
		GuestSSHPort: o.GuestSSHPort, GuestGatewayPort: o.GuestGatewayPort, Profile: profile,
	}
	if err := c.validate(); err != nil {
		return err
	}
	hostAddrs := make([]netip.Prefix, len(o.HostAddrs))
	for i, s := range o.HostAddrs {
		if hostAddrs[i], err = netip.ParsePrefix(s); err != nil {
			return err
		}
	}
	gateways, err := ReadGateways()
	if err != nil {
		return err
	}
	nftPath, err := lookPath("nft")
	if err != nil {
		return err
	}
	nft := exec.Command(nftPath, "-f", "-")
	nft.Stdin = strings.NewReader(Ruleset(profile, gateways, hostAddrs))
	if out, err := nft.CombinedOutput(); err != nil {
		return fmt.Errorf("loading the firewall: %w: %s", err, strings.TrimSpace(string(out)))
	}
	passt, err := lookPath("passt")
	if err != nil {
		return err
	}
	argv := c.passtArgs(passt)
	return syscall.Exec(passt, argv, os.Environ())
}

// lookPath finds a program in PATH or, as on systems that keep it out of
// users' PATH, in /usr/sbin.
func lookPath(name string) (string, error) {
	if p, err := exec.LookPath(name); err == nil {
		return p, nil
	}
	return exec.LookPath("/usr/sbin/" + name)
}
