// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

package vmnet

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// The host's networks change while a VM runs: a new DHCP lease, a VPN, an
// IPv6 privacy address. The firewall blocks the host's addresses and counts
// its networks as the LAN, so it must follow them. The namespace does not
// see the host's network, so two processes share the work through a file
// next to the VM's socket:
//
//	host       brig net-helper watch  ──writes──▶  host-net.json
//	namespace  brig net-helper reload ◀──reads───  (reloads the firewall)
//
// The watcher is the unit's main process and runs pasta; the helper starts
// the reloader before it becomes passt.

// hostNetFile is the file, next to the VM's socket, that holds the host's
// network.
const hostNetFile = "host-net.json"

// HostNet is what the firewall needs of the host's network: the addresses
// of its interfaces, as in Config.HostAddrs, and the destinations of its
// routes, as in Config.Routes.
type HostNet struct {
	Addrs  []netip.Prefix `json:"addrs"`
	Routes []netip.Prefix `json:"routes"`
}

func (h HostNet) equal(o HostNet) bool {
	return slices.Equal(h.Addrs, o.Addrs) && slices.Equal(h.Routes, o.Routes)
}

// readHostNet reads the host's network; tests replace it.
var readHostNet = func() (HostNet, error) {
	addrs, err := hostAddrs()
	if err != nil {
		return HostNet{}, err
	}
	routes, err := ReadRoutes("all")
	return HostNet{Addrs: addrs, Routes: routes}, err
}

// writeHostNet replaces the file at path with h, so that a reader sees
// either the old file or the new one.
func writeHostNet(path string, h HostNet) error {
	b, err := json.Marshal(h)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+hostNetFile+"-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	_, err = tmp.Write(b)
	if err = errors.Join(err, tmp.Close()); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// watchSettle is how long the watcher waits after a change for more of
// them, as one DHCP lease or VPN brings several.
var watchSettle = 250 * time.Millisecond

// RunWatcher writes the host's network to path, runs argv, the VM's pasta,
// and writes the host's network again whenever it changes. It returns when
// argv exits, with its error. When ctx is done, it passes SIGTERM on to
// argv and waits for it.
func RunWatcher(ctx context.Context, path string, argv []string) error {
	if len(argv) == 0 || !filepath.IsAbs(path) {
		return errors.New("a command and an absolute path are required")
	}
	last, err := readHostNet()
	if err != nil {
		return err
	}
	if err := writeHostNet(path, last); err != nil {
		return err
	}
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := cmd.Start(); err != nil {
		return err
	}
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	changes := netChanges()
	var settle <-chan time.Time
	done := ctx.Done()
	for {
		select {
		case err := <-exited:
			return err
		case <-done:
			_ = cmd.Process.Signal(syscall.SIGTERM)
			done = nil
		case <-changes:
			settle = time.After(watchSettle)
		case <-settle:
			h, err := readHostNet()
			if err != nil {
				fmt.Fprintf(os.Stderr, "brig: reading the host's network: %v\n", err)
				continue
			}
			if h.equal(last) {
				continue
			}
			if err := writeHostNet(path, h); err != nil {
				fmt.Fprintf(os.Stderr, "brig: writing the host's network: %v\n", err)
				continue
			}
			last = h
		}
	}
}

// netPoll is how often the watcher reads the host's network when it cannot
// listen for changes.
const netPoll = 2 * time.Second

// netChanges signals changes of the host's addresses and routes, as the
// kernel announces them over netlink, or every netPoll if it cannot listen
// to them.
func netChanges() <-chan struct{} {
	ch := make(chan struct{}, 1)
	notify := func() {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
	poll := func(err error) {
		fmt.Fprintf(os.Stderr, "brig: reading the host's network every %s: cannot listen for its changes: %v\n", netPoll, err)
		for range time.Tick(netPoll) {
			notify()
		}
	}
	fd, err := unix.Socket(unix.AF_NETLINK, unix.SOCK_RAW|unix.SOCK_CLOEXEC, unix.NETLINK_ROUTE)
	if err != nil {
		go poll(err)
		return ch
	}
	groups := unix.RTMGRP_IPV4_IFADDR | unix.RTMGRP_IPV6_IFADDR | unix.RTMGRP_IPV4_ROUTE | unix.RTMGRP_IPV6_ROUTE
	if err := unix.Bind(fd, &unix.SockaddrNetlink{Family: unix.AF_NETLINK, Groups: uint32(groups)}); err != nil {
		_ = unix.Close(fd)
		go poll(err)
		return ch
	}
	go func() {
		// What changed does not matter: the watcher reads it all again.
		buf := make([]byte, 64<<10)
		for {
			_, err := unix.Read(fd, buf)
			switch {
			case err == nil, errors.Is(err, unix.EINTR):
			case errors.Is(err, unix.ENOBUFS):
				// Announcements were lost; changes among them too.
			default:
				_ = unix.Close(fd)
				poll(err)
				return
			}
			notify()
		}
	}()
	return ch
}

// reloadEvery is how often the reloader looks for a new host network.
var reloadEvery = 500 * time.Millisecond

// reloader loads the firewall again when the file at path holds another
// host network than current.
type reloader struct {
	path    string
	current HostNet
	// load loads the firewall for a host network.
	load func(HostNet) error
	// parentGone reports whether the reloader's parent has exited.
	parentGone func() bool
}

func (r *reloader) run(ctx context.Context) error {
	var seen os.FileInfo
	tick := time.NewTicker(reloadEvery)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-tick.C:
		}
		if r.parentGone() {
			return nil
		}
		// The watcher replaces the file, so a new one is a change.
		fi, err := os.Stat(r.path)
		if err != nil || (seen != nil && os.SameFile(fi, seen)) {
			continue
		}
		seen = fi
		b, err := os.ReadFile(r.path)
		if err != nil {
			fmt.Fprintf(os.Stderr, "brig: reading the host's network: %v\n", err)
			continue
		}
		var h HostNet
		if err := json.Unmarshal(b, &h); err != nil {
			fmt.Fprintf(os.Stderr, "brig: reading the host's network: %v\n", err)
			continue
		}
		if h.equal(r.current) {
			continue
		}
		if err := r.load(h); err != nil {
			fmt.Fprintf(os.Stderr, "brig: the firewall still blocks the host's previous network: %v\n", err)
			continue
		}
		r.current = h
		fmt.Fprintln(os.Stderr, "brig: the firewall follows a change of the host's network")
	}
}

// RunReloader runs in the namespace beside passt: it loads the firewall
// again whenever the watcher writes another host network to o.HostNet. It
// returns when its parent, passt by then, exits.
func RunReloader(ctx context.Context, o HelperOptions) error {
	if !filepath.IsAbs(o.HostNet) {
		return errors.New("the host network's path must be absolute")
	}
	profile, err := decodeProfile(o.Profile)
	if err != nil {
		return err
	}
	addrs, err := parsePrefixes(o.HostAddrs)
	if err != nil {
		return err
	}
	routes, err := parsePrefixes(o.Routes)
	if err != nil {
		return err
	}
	nftPath, err := lookPath("nft")
	if err != nil {
		return err
	}
	parent := os.Getppid()
	r := reloader{
		path:    o.HostNet,
		current: HostNet{Addrs: addrs, Routes: routes},
		load: func(h HostNet) error {
			// The namespace's gateways stay as pasta set them up.
			gateways, err := ReadGateways()
			if err != nil {
				return err
			}
			return loadNft(nftPath, reloadRuleset(Ruleset(profile, gateways, h.Addrs, h.Routes)))
		},
		parentGone: func() bool { return os.Getppid() != parent },
	}
	return r.run(ctx)
}

// reloadRuleset makes a ruleset replace the table it defines in one
// transaction, so that no packet passes between the old rules and the new.
func reloadRuleset(ruleset string) string {
	return "table inet brig\nflush table inet brig\n" + ruleset
}

func loadNft(nftPath, ruleset string) error {
	nft := exec.Command(nftPath, "-f", "-")
	nft.Stdin = strings.NewReader(ruleset)
	if out, err := nft.CombinedOutput(); err != nil {
		return fmt.Errorf("loading the firewall: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// startReloader starts RunReloader as a child that stays in the namespace
// when the helper becomes passt. Without it, the firewall keeps the host's
// network as it was when the VM started, so it only warns.
func startReloader(o HelperOptions) {
	brig, err := os.Executable()
	if err == nil {
		args := []string{"net-helper", "reload", "--host-net", o.HostNet, "--profile", o.Profile}
		for _, a := range o.HostAddrs {
			args = append(args, "--host-addr", a)
		}
		for _, r := range o.Routes {
			args = append(args, "--route", r)
		}
		cmd := exec.Command(brig, args...)
		cmd.Stdout, cmd.Stderr = os.Stderr, os.Stderr
		err = cmd.Start()
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "brig: the firewall will not follow changes of the host's network: %v\n", err)
	}
}
