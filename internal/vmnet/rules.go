// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

package vmnet

import (
	"encoding/json"
	"fmt"
	"net/netip"
	"os/exec"
	"slices"
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
// gateways are the namespace's IPv4 and IPv6 default gateways, all next
// hops of its default routes, one of which pasta maps to the host's
// loopback interface. A family without a default route has none; the host
// is then unreachable over it anyway. Host ports are only reachable over
// IPv4.
//
// hostAddrs are the host's interface addresses with their prefix lengths.
// pasta connects to any address on the VM's behalf, so a connection to one
// of them reaches the host's services as if from the host itself; without
// p.Host, they are blocked. Their networks count as the LAN, besides the
// special-purpose ranges, because LANs may use public addresses too, as
// IPv6 LANs mostly do. So do routes, the destinations of the host's routes
// other than default ones: pasta's connections follow them, for example
// into a VPN's intranet.
func Ruleset(p config.NetworkProfile, gateways Gateways, hostAddrs, routes []netip.Prefix) string {
	addrs4, addrs6, nets4, nets6 := hostSets(hostAddrs, routes)
	var b strings.Builder
	w := func(format string, args ...any) { fmt.Fprintf(&b, format+"\n", args...) }
	w("table inet brig {")
	w("\tchain output {")
	w("\t\ttype filter hook output priority filter; policy drop;")
	w("\t\toif \"lo\" accept")
	w("\t\tct state established,related accept")
	if p.IPv6 {
		// Neighbour discovery is untracked by conntrack, and the
		// namespace's kernel needs it to reach the gateway even
		// when the multicast and gateway rules below drop it.
		// pasta answers on the tap; nothing is forwarded.
		w("\t\ticmpv6 type { nd-neighbor-solicit, nd-neighbor-advert } ip6 hoplimit 255 accept")
	}
	if p.Internet || p.LAN || p.Host {
		w("\t\tip daddr %s meta l4proto { tcp, udp } th dport 53 accept", DNSAddr)
	}
	// pasta maps more than DNS on this address to the host's resolver,
	// e.g. DNS over TLS on port 853.
	w("\t\tip daddr %s drop", DNSAddr)
	if gws := addrList(gateways.IPv4); gws != "" {
		switch {
		case p.Host:
			w("\t\tip daddr %s accept", gws)
		case len(p.HostPorts) > 0:
			ports := make([]string, len(p.HostPorts))
			for i, port := range p.HostPorts {
				ports[i] = fmt.Sprint(port)
			}
			w("\t\tip daddr %s tcp dport { %s } accept", gws, strings.Join(ports, ", "))
		}
		w("\t\tip daddr %s drop", gws)
	}
	if gws := addrList(gateways.IPv6); gws != "" && p.IPv6 {
		verdict := "drop"
		if p.Host {
			verdict = "accept"
		}
		w("\t\tip6 daddr %s %s", gws, verdict)
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
	w("\t\tip daddr { %s } %s", strings.Join(nets4, ", "), verdict)
	if p.IPv6 {
		w("\t\tip6 daddr { %s } %s", strings.Join(nets6, ", "), verdict)
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

// minRouteBits are the shortest IPv4 and IPv6 routes that count as the
// LAN. Shorter ones stand in for a default route, such as the 0.0.0.0/1 and
// 128.0.0.0/1 that VPN clients add to route all traffic.
const minRouteBits4, minRouteBits6 = 8, 16

// hostSets splits hostAddrs into the host's IPv4 and IPv6 addresses, and
// returns the LAN: the special-purpose ranges plus the networks of
// hostAddrs and routes. A network within another one is left out, so that
// the sets hold no overlapping intervals.
func hostSets(hostAddrs, routes []netip.Prefix) (addrs4, addrs6, nets4, nets6 []string) {
	var nets []netip.Prefix
	for _, r := range slices.Concat(lan4, lan6) {
		nets = append(nets, netip.MustParsePrefix(r))
	}
	seen := map[netip.Addr]bool{}
	for _, p := range hostAddrs {
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
		nets = append(nets, p.Masked())
	}
	for _, r := range routes {
		minBits := minRouteBits6
		if r.Addr().Is4() {
			minBits = minRouteBits4
		}
		if r.IsValid() && r.Bits() >= minBits {
			nets = append(nets, r.Masked())
		}
	}
	for i, n := range nets {
		// Keep n unless a wider network, or an earlier equal one, covers it.
		if slices.ContainsFunc(nets, func(m netip.Prefix) bool {
			return m != n && m.Bits() <= n.Bits() && m.Contains(n.Addr())
		}) || slices.Index(nets, n) < i {
			continue
		}
		if n.Addr().Is4() {
			nets4 = append(nets4, n.String())
		} else {
			nets6 = append(nets6, n.String())
		}
	}
	return addrs4, addrs6, nets4, nets6
}

// addrList renders addresses as an nftables value: a single address or an
// anonymous set. It returns "" for none.
func addrList(addrs []netip.Addr) string {
	switch len(addrs) {
	case 0:
		return ""
	case 1:
		return addrs[0].String()
	}
	s := make([]string, len(addrs))
	for i, a := range addrs {
		s[i] = a.String()
	}
	return "{ " + strings.Join(s, ", ") + " }"
}

// Gateways are the default gateways of a network namespace: the next hops
// of its default routes, in the order the kernel lists them.
type Gateways struct {
	IPv4, IPv6 []netip.Addr
}

// ReadGateways reads the default gateways of the caller's network namespace
// with ip(8), which, unlike /proc/net/route, lists every next hop of a
// multipath route.
func ReadGateways() (Gateways, error) {
	var g Gateways
	routes, err := ipRoutes("-4", "default")
	if err != nil {
		return g, err
	}
	g.IPv4 = gateways(routes)
	if routes, err = ipRoutes("-6", "default"); err != nil {
		return g, err
	}
	g.IPv6 = gateways(routes)
	return g, nil
}

// ReadRoutes reads the destinations of the caller's routes in the main
// routing table, other than default routes and routes that drop traffic.
func ReadRoutes() ([]netip.Prefix, error) {
	var prefixes []netip.Prefix
	for _, family := range []string{"-4", "-6"} {
		routes, err := ipRoutes(family, "table", "main")
		if err != nil {
			return nil, err
		}
		for _, r := range routes {
			if p, ok := r.prefix(); ok {
				prefixes = append(prefixes, p)
			}
		}
	}
	return prefixes, nil
}

// ipRoute is a route as `ip -json route show` prints it.
type ipRoute struct {
	Type     string `json:"type"`
	Dst      string `json:"dst"`
	Gateway  string `json:"gateway"`
	Nexthops []struct {
		Gateway string `json:"gateway"`
	} `json:"nexthops"`
}

// prefix returns the route's destination unless it is a default route or
// a route that does not deliver traffic, such as a blackhole route.
func (r ipRoute) prefix() (netip.Prefix, bool) {
	if r.Dst == "default" || (r.Type != "" && r.Type != "unicast") {
		return netip.Prefix{}, false
	}
	if p, err := netip.ParsePrefix(r.Dst); err == nil {
		return p, true
	}
	if a, err := netip.ParseAddr(r.Dst); err == nil {
		return netip.PrefixFrom(a, a.BitLen()), true
	}
	return netip.Prefix{}, false
}

// gateways returns the next hops of routes, each once.
func gateways(routes []ipRoute) []netip.Addr {
	var addrs []netip.Addr
	add := func(s string) {
		if a, err := netip.ParseAddr(s); err == nil && !slices.Contains(addrs, a) {
			addrs = append(addrs, a)
		}
	}
	for _, r := range routes {
		add(r.Gateway)
		for _, h := range r.Nexthops {
			add(h.Gateway)
		}
	}
	return addrs
}

// ipRoutes runs `ip -json FAMILY route show ARGS...`.
func ipRoutes(family string, args ...string) ([]ipRoute, error) {
	ip, err := lookPath("ip")
	if err != nil {
		return nil, fmt.Errorf("ip is not installed (package iproute): %w", err)
	}
	cmd := exec.Command(ip, append([]string{"-json", family, "route", "show"}, args...)...)
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("%s: %w", strings.Join(cmd.Args, " "), err)
	}
	return parseRoutes(out)
}

func parseRoutes(out []byte) ([]ipRoute, error) {
	var routes []ipRoute
	if len(strings.TrimSpace(string(out))) == 0 {
		return nil, nil // the family is disabled
	}
	if err := json.Unmarshal(out, &routes); err != nil {
		return nil, fmt.Errorf("parsing ip's routes: %w", err)
	}
	return routes, nil
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
