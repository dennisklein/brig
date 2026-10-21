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
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	"github.com/dennisklein/brig/internal/config"
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
// host network than current, or the file at portsPath other service ports.
type reloader struct {
	path    string
	current HostNet
	// portsPath is the file that lists the host ports of the services
	// attached to the VM's sandboxes (see WriteServicePorts), empty for
	// none; ports are the ones the firewall was loaded with.
	portsPath string
	ports     []uint16
	// load loads the firewall for a host network and service ports.
	load func(HostNet, []uint16) error
	// parentGone reports whether the reloader's parent has exited.
	parentGone func() bool
}

func (r *reloader) run(ctx context.Context) error {
	var seen, seenPorts os.FileInfo
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
		h, hostChanged := r.current, false
		// The watcher replaces the file, so a new one is a change.
		if fi, err := os.Stat(r.path); err == nil && (seen == nil || !os.SameFile(fi, seen)) {
			seen = fi
			if h, err = readHostNetFile(r.path); err != nil {
				fmt.Fprintf(os.Stderr, "brig: reading the host's network: %v\n", err)
				h = r.current
			} else {
				hostChanged = !h.equal(r.current)
			}
		}
		ports, portsChanged := r.ports, false
		if r.portsPath != "" {
			fi, err := os.Stat(r.portsPath)
			switch {
			case errors.Is(err, os.ErrNotExist):
				// brig removes the file once no service is attached.
				portsChanged, seenPorts, ports = len(r.ports) > 0, nil, nil
			case err == nil && (seenPorts == nil || !os.SameFile(fi, seenPorts)):
				seenPorts = fi
				if ports, err = ReadServicePorts(r.portsPath); err != nil {
					fmt.Fprintf(os.Stderr, "brig: reading the service ports: %v\n", err)
					ports = r.ports
				} else {
					portsChanged = !slices.Equal(ports, r.ports)
				}
			}
		}
		if !hostChanged && !portsChanged {
			continue
		}
		if err := r.load(h, ports); err != nil {
			fmt.Fprintf(os.Stderr, "brig: the firewall still allows what it allowed before: %v\n", err)
			continue
		}
		r.current, r.ports = h, ports
		if hostChanged {
			fmt.Fprintln(os.Stderr, "brig: the firewall follows a change of the host's network")
		}
		if portsChanged {
			fmt.Fprintln(os.Stderr, "brig: the firewall follows a change of the attached services")
		}
	}
}

// readHostNetFile reads a host network as writeHostNet wrote it.
func readHostNetFile(path string) (HostNet, error) {
	var h HostNet
	b, err := os.ReadFile(path)
	if err != nil {
		return h, err
	}
	err = json.Unmarshal(b, &h)
	return h, err
}

// servicePortsFile is the file, next to the VM's socket, that lists the
// host ports of the services attached to the VM's sandboxes.
const servicePortsFile = "service-ports.json"

// ServicePortsPath is the file that lists the host ports of the services
// attached to the sandboxes of the VM whose socket is socket.
func ServicePortsPath(socket string) string {
	return filepath.Join(filepath.Dir(socket), servicePortsFile)
}

// servicePorts is the content of a service ports file.
type servicePorts struct {
	Ports []uint16 `json:"ports"`
}

// WriteServicePorts replaces the service ports file at path, as
// ServicePortsPath names it, with ports, so that the VM's firewall lets the
// VM reach them on the host's loopback like a profile's host_ports: at the
// VM's next start, and at once while it runs. No ports remove the file.
func WriteServicePorts(path string, ports []uint16) error {
	ports = slices.Compact(slices.Sorted(slices.Values(ports)))
	if len(ports) == 0 {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return nil
	}
	if slices.Contains(ports, 0) {
		return errors.New("service port 0 is not a TCP port")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	b, err := json.Marshal(servicePorts{Ports: ports})
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+servicePortsFile+"-*")
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

// ReadServicePorts reads a service ports file; a missing file lists none.
func ReadServicePorts(path string) ([]uint16, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var sp servicePorts
	if err := json.Unmarshal(b, &sp); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if slices.Contains(sp.Ports, 0) {
		return nil, fmt.Errorf("%s: port 0 is not a TCP port", path)
	}
	return slices.Compact(slices.Sorted(slices.Values(sp.Ports))), nil
}

// withServicePorts returns p allowing ports on the host's loopback too.
func withServicePorts(p config.NetworkProfile, ports []uint16) config.NetworkProfile {
	if len(ports) == 0 {
		return p
	}
	p.HostPorts = slices.Compact(slices.Sorted(slices.Values(slices.Concat(p.HostPorts, ports))))
	return p
}

// RunReloader runs in the namespace beside passt: it loads the firewall
// again whenever the watcher writes another host network to o.HostNet, or
// brig other service ports to o.ServicePorts. It returns when its parent,
// passt by then, exits.
func RunReloader(ctx context.Context, o HelperOptions) error {
	if !filepath.IsAbs(o.HostNet) {
		return errors.New("the host network's path must be absolute")
	}
	if o.ServicePorts != "" && !filepath.IsAbs(o.ServicePorts) {
		return errors.New("the service ports' path must be absolute")
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
		path:      o.HostNet,
		current:   HostNet{Addrs: addrs, Routes: routes},
		portsPath: o.ServicePorts,
		ports:     o.LoadedServicePorts,
		load: func(h HostNet, ports []uint16) error {
			// The namespace's gateways stay as pasta set them up.
			gateways, err := ReadGateways()
			if err != nil {
				return err
			}
			return loadNft(nftPath, reloadRuleset(Ruleset(withServicePorts(profile, ports), gateways, h.Addrs, h.Routes)))
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
		if o.ServicePorts != "" {
			args = append(args, "--service-ports", o.ServicePorts)
		}
		for _, p := range o.LoadedServicePorts {
			args = append(args, "--loaded-service-port", strconv.Itoa(int(p)))
		}
		cmd := exec.Command(brig, args...)
		cmd.Stdout, cmd.Stderr = os.Stderr, os.Stderr
		err = cmd.Start()
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "brig: the firewall will not follow changes of the host's network: %v\n", err)
	}
}
