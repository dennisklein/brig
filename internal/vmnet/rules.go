// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

package vmnet

import (
	"bufio"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/netip"
	"os"
	"slices"
	"strconv"
	"strings"

	"github.com/dennisklein/brig/internal/config"
)

// Special-purpose IPv4 and IPv6 ranges that the lan setting covers: private,
// shared (CGNAT), link-local, multicast and broadcast addresses.
var (
	lan4 = []string{"10.0.0.0/8", "100.64.0.0/10", "169.254.0.0/16", "172.16.0.0/12", "192.168.0.0/16", "224.0.0.0/4", "255.255.255.255/32"}
	lan6 = []string{"fc00::/7", "fe80::/10", "ff00::/8"}
)

// Ruleset renders the nftables rules that enforce p in the VM's network
// namespace. All traffic there originates from passt on behalf of the VM, so
// only the output hook is filtered.
//
// gateways are the namespace's IPv4 and IPv6 default gateways, which pasta
// maps to the host's loopback interface. A family without a default route
// has an invalid Addr; the host is then unreachable over it anyway. Host
// ports are only reachable over IPv4.
//
// hostAddrs are the host's interface addresses with their prefix lengths.
// pasta connects to any address on the VM's behalf, so a connection to one
// of them reaches the host's services as if from the host itself; without
// p.Host, they are blocked. Their networks count as the LAN, besides the
// special-purpose ranges, because LANs may use public addresses too, as
// IPv6 LANs mostly do.
func Ruleset(p config.NetworkProfile, gateways Gateways, hostAddrs []netip.Prefix) string {
	addrs4, addrs6, nets4, nets6 := hostSets(hostAddrs)
	var b strings.Builder
	w := func(format string, args ...any) { fmt.Fprintf(&b, format+"\n", args...) }
	w("table inet brig {")
	w("\tchain output {")
	w("\t\ttype filter hook output priority filter; policy drop;")
	w("\t\toif \"lo\" accept")
	w("\t\tct state established,related accept")
	if p.Internet || p.LAN || p.Host {
		w("\t\tip daddr %s meta l4proto { tcp, udp } th dport 53 accept", DNSAddr)
	}
	if gw := gateways.IPv4; gw.IsValid() {
		switch {
		case p.Host:
			w("\t\tip daddr %s accept", gw)
		case len(p.HostPorts) > 0:
			ports := make([]string, len(p.HostPorts))
			for i, port := range p.HostPorts {
				ports[i] = fmt.Sprint(port)
			}
			w("\t\tip daddr %s tcp dport { %s } accept", gw, strings.Join(ports, ", "))
		}
		w("\t\tip daddr %s drop", gw)
	}
	if gw := gateways.IPv6; gw.IsValid() && p.IPv6 {
		verdict := "drop"
		if p.Host {
			verdict = "accept"
		}
		w("\t\tip6 daddr %s %s", gw, verdict)
	}
	if !p.Host && len(addrs4) > 0 {
		w("\t\tip daddr { %s } drop", strings.Join(addrs4, ", "))
	}
	if !p.Host && p.IPv6 && len(addrs6) > 0 {
		w("\t\tip6 daddr { %s } drop", strings.Join(addrs6, ", "))
	}
	verdict := "drop"
	if p.LAN {
		verdict = "accept"
	}
	w("\t\tip daddr { %s } %s", strings.Join(slices.Concat(lan4, nets4), ", "), verdict)
	if p.IPv6 {
		w("\t\tip6 daddr { %s } %s", strings.Join(slices.Concat(lan6, nets6), ", "), verdict)
	} else {
		w("\t\tmeta nfproto ipv6 drop")
	}
	if p.Internet {
		w("\t\taccept")
	}
	w("\t}")
	w("}")
	return b.String()
}

// hostSets splits hostAddrs into the host's IPv4 and IPv6 addresses and
// their networks. Networks within the special-purpose LAN ranges or within
// each other are left out, so that every network is listed once.
func hostSets(hostAddrs []netip.Prefix) (addrs4, addrs6, nets4, nets6 []string) {
	var nets []netip.Prefix
	for _, r := range slices.Concat(lan4, lan6) {
		nets = append(nets, netip.MustParsePrefix(r))
	}
	// Wider networks first, so that they absorb narrower ones.
	sorted := slices.Clone(hostAddrs)
	slices.SortStableFunc(sorted, func(a, b netip.Prefix) int { return a.Bits() - b.Bits() })
	seen := map[netip.Addr]bool{}
	for _, p := range sorted {
		if !p.IsValid() {
			continue
		}
		if a := p.Addr(); !seen[a] {
			seen[a] = true
			if a.Is4() {
				addrs4 = append(addrs4, a.String())
			} else {
				addrs6 = append(addrs6, a.String())
			}
		}
		n := p.Masked()
		if slices.ContainsFunc(nets, n.Overlaps) {
			continue
		}
		nets = append(nets, n)
		if n.Addr().Is4() {
			nets4 = append(nets4, n.String())
		} else {
			nets6 = append(nets6, n.String())
		}
	}
	return addrs4, addrs6, nets4, nets6
}

// Gateways are the default gateways of a network namespace.
type Gateways struct {
	IPv4, IPv6 netip.Addr
}

// ReadGateways reads the default gateways of the caller's network namespace.
func ReadGateways() (Gateways, error) {
	var g Gateways
	var err error
	if g.IPv4, err = readRouteFile("/proc/net/route", defaultGateway4); err != nil {
		return g, err
	}
	g.IPv6, err = readRouteFile("/proc/net/ipv6_route", defaultGateway6)
	return g, err
}

func readRouteFile(path string, parse func(io.Reader) (netip.Addr, error)) (netip.Addr, error) {
	f, err := os.Open(path)
	if errors.Is(err, fs.ErrNotExist) {
		return netip.Addr{}, nil // family disabled
	}
	if err != nil {
		return netip.Addr{}, err
	}
	defer func() { _ = f.Close() }()
	return parse(f)
}

// defaultGateway4 picks the IPv4 default gateway with the lowest metric from
// a /proc/net/route table. It returns the zero Addr when there is none.
func defaultGateway4(r io.Reader) (netip.Addr, error) {
	s := bufio.NewScanner(r)
	best, bestMetric := netip.Addr{}, uint64(0)
	for first := true; s.Scan(); first = false {
		f := strings.Fields(s.Text())
		// Iface Destination Gateway Flags RefCnt Use Metric ...
		if first || len(f) < 7 || f[1] != "00000000" || f[2] == "00000000" {
			continue
		}
		raw, err := hex.DecodeString(f[2])
		metric, merr := strconv.ParseUint(f[6], 10, 32)
		if err != nil || len(raw) != 4 || merr != nil {
			return netip.Addr{}, fmt.Errorf("malformed default route %q", s.Text())
		}
		// The kernel prints the address as a little-endian word.
		var a [4]byte
		binary.BigEndian.PutUint32(a[:], binary.LittleEndian.Uint32(raw))
		if !best.IsValid() || metric < bestMetric {
			best, bestMetric = netip.AddrFrom4(a), metric
		}
	}
	return best, s.Err()
}

// defaultGateway6 picks the IPv6 default gateway with the lowest metric from
// a /proc/net/ipv6_route table. It returns the zero Addr when there is none.
func defaultGateway6(r io.Reader) (netip.Addr, error) {
	s := bufio.NewScanner(r)
	best, bestMetric := netip.Addr{}, uint64(0)
	const zero = "00000000000000000000000000000000"
	for s.Scan() {
		f := strings.Fields(s.Text())
		// Destination PrefixLen Source SrcPrefixLen NextHop Metric ...
		if len(f) < 6 || f[0] != zero || f[1] != "00" || f[4] == zero {
			continue
		}
		raw, err := hex.DecodeString(f[4])
		metric, merr := strconv.ParseUint(f[5], 16, 32)
		if err != nil || len(raw) != 16 || merr != nil {
			return netip.Addr{}, fmt.Errorf("malformed default route %q", s.Text())
		}
		a := netip.AddrFrom16([16]byte(raw))
		if !best.IsValid() || metric < bestMetric {
			best, bestMetric = a, metric
		}
	}
	return best, s.Err()
}

func encodeProfile(p config.NetworkProfile) (string, error) {
	b, err := json.Marshal(p)
	return string(b), err
}

func decodeProfile(s string) (config.NetworkProfile, error) {
	var p config.NetworkProfile
	dec := json.NewDecoder(strings.NewReader(s))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&p); err != nil {
		return p, fmt.Errorf("invalid network profile: %w", err)
	}
	return p, nil
}
