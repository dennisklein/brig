// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

package vmnet

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/dennisklein/brig/internal/fence2"
)

// HelperOptions are the arguments of `brig net-helper`.
type HelperOptions struct {
	Socket                         string
	GuestSSHPort, GuestGatewayPort int
	Profile                        string
	// HostAddrs are the host's addresses with prefix lengths, as in
	// Config.HostAddrs.
	HostAddrs []string
	// Routes are the host's route destinations, as in Config.Routes.
	Routes []string
	// HostNet is the file that the watcher writes the host's network to
	// when it changes, empty for none.
	HostNet string
	// Fence2 is how the firewall that the reloader replaces records drops,
	// one of the fence2 modes.
	Fence2 string
	// Spool is the listener's spool, as in Config.Spool.
	Spool string
}

// RunHelper runs inside pasta's namespace: it loads the firewall for the
// profile, starts the listener that records its drops and the reloader
// that keeps it up to date with the host's network, and then replaces
// itself with passt. If the firewall cannot be loaded, passt never starts
// and the VM has no network; recording drops never keeps it from starting.
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
	hostAddrs, err := parsePrefixes(o.HostAddrs)
	if err != nil {
		return err
	}
	routes, err := parsePrefixes(o.Routes)
	if err != nil {
		return err
	}
	gateways, err := ReadGateways()
	if err != nil {
		return err
	}
	nftPath, err := lookPath("nft")
	if err != nil {
		return err
	}
	mode, why, err := loadFirewall(nftPath, func(mode fence2.Mode) string {
		return Ruleset(profile, gateways, hostAddrs, routes, mode)
	})
	if err != nil {
		return err
	}
	if mode != fence2.Itemised {
		fmt.Fprintf(os.Stderr, "brig: fence 2 records %s: %s\n", mode, why)
	}
	if o.Spool != "" {
		startListener(o.Spool, mode, why)
	}
	if o.HostNet != "" {
		o.Fence2 = string(mode)
		startReloader(o)
	}
	passt, err := lookPath("passt")
	if err != nil {
		return err
	}
	argv := c.passtArgs(passt)
	return syscall.Exec(passt, argv, os.Environ())
}

// loadFirewall loads the ruleset that records drops as fully as this host
// allows: itemised needs the kernel's nft_log and nfnetlink_log, counts only
// its named counters. Recording drops must never keep a VM from starting,
// so the last resort records nothing. nft loads a ruleset as a whole or not
// at all, so a failed try leaves nothing behind. It returns the mode loaded
// and why it is not fence2.Itemised.
func loadFirewall(nftPath string, ruleset func(fence2.Mode) string) (fence2.Mode, string, error) {
	load := func(mode fence2.Mode) (string, error) {
		nft := exec.Command(nftPath, "-f", "-")
		nft.Stdin = strings.NewReader(ruleset(mode))
		out, err := nft.CombinedOutput()
		return strings.TrimSpace(string(out)), err
	}
	// nft's first line says what failed; the next ones point at the rule.
	firstLine := func(s string) string { line, _, _ := strings.Cut(s, "\n"); return line }
	out, err := load(fence2.Itemised)
	if err == nil {
		return fence2.Itemised, "", nil
	}
	why := "nft would not load the rules that log drops: " + firstLine(out)
	if out, err = load(fence2.CountsOnly); err == nil {
		return fence2.CountsOnly, why, nil
	}
	why = "nft would not load the rules that count drops: " + firstLine(out)
	if out, err = load(fence2.Off); err != nil {
		return fence2.Off, why, fmt.Errorf("loading the firewall: %w: %s", err, out)
	}
	return fence2.Off, why, nil
}

// listenerReady bounds the wait for the listener to bind its NFLOG group,
// so that it reads the drops from the VM's first packet on. executable is
// brig, which runs the listener. Tests replace both.
var (
	listenerReady = 5 * time.Second
	executable    = os.Executable
)

// startListener starts the listener that writes the firewall's drops to
// spool, as a child that stays in the namespace when the helper becomes
// passt, and waits until it is ready. Without counters, there is nothing
// to listen to, and the helper only notes that in the spool.
func startListener(spool string, mode fence2.Mode, why string) {
	note := func(why string) {
		if err := (fence2.Spool{Path: spool}).Append(fence2.Entry{T: time.Now(), State: fence2.Off, Why: why}); err != nil {
			fmt.Fprintf(os.Stderr, "brig: cannot write the fence 2 spool: %v\n", err)
		}
	}
	if mode == fence2.Off {
		note(why)
		return
	}
	brig, err := executable()
	if err != nil {
		note("the listener did not start: " + err.Error())
		return
	}
	r, w, err := os.Pipe()
	if err != nil {
		note("the listener did not start: " + err.Error())
		return
	}
	defer func() { _ = r.Close() }()
	cmd := exec.Command(brig, "net-helper", "fence2-log", "--spool", spool, "--mode", string(mode), "--why", why)
	cmd.Stderr = os.Stderr
	cmd.ExtraFiles = []*os.File{w}
	err = cmd.Start()
	_ = w.Close()
	if err != nil {
		note("the listener did not start: " + err.Error())
		return
	}
	ready := make(chan struct{})
	go func() {
		// A line, or the end when the listener exits.
		_, _ = bufio.NewReader(r).ReadString('\n')
		close(ready)
	}()
	select {
	case <-ready:
	case <-time.After(listenerReady):
		fmt.Fprintln(os.Stderr, "brig: the fence 2 listener is not ready; starting passt without waiting for it")
	}
}

// ListenerOptions are the arguments of `brig net-helper fence2-log`.
type ListenerOptions struct {
	Spool, Mode, Why string
}

// RunListener is the listener that startListener starts: it reports ready
// on file descriptor 3, and writes the drops to the spool until it is
// stopped or its parent, passt by then, exits.
func RunListener(ctx context.Context, o ListenerOptions) error {
	if !filepath.IsAbs(o.Spool) {
		return errors.New("the spool's path must be absolute")
	}
	nftPath, err := lookPath("nft")
	if err != nil {
		return err
	}
	l := &fence2.Listener{
		Spool: fence2.Spool{Path: o.Spool}, Mode: fence2.Mode(o.Mode), Why: o.Why,
		Counters: func() (map[string]uint64, error) { return fence2.ReadCounters(nftPath) },
		Parent:   os.Getppid(),
		Log:      slog.New(slog.NewTextHandler(os.Stderr, nil)),
	}
	return l.Run(ctx, os.NewFile(3, "ready"))
}

// lookPath finds a program in PATH or, as on systems that keep it out of
// users' PATH, in /usr/sbin.
func lookPath(name string) (string, error) {
	if p, err := exec.LookPath(name); err == nil {
		return p, nil
	}
	return exec.LookPath("/usr/sbin/" + name)
}

func parsePrefixes(ss []string) ([]netip.Prefix, error) {
	prefixes := make([]netip.Prefix, len(ss))
	for i, s := range ss {
		var err error
		if prefixes[i], err = netip.ParsePrefix(s); err != nil {
			return nil, err
		}
	}
	return prefixes, nil
}
