// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

package vmnet

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/dennisklein/brig/internal/config"
)

var testGateways = Gateways{
	IPv4: []netip.Addr{netip.MustParseAddr("192.168.1.1")},
	IPv6: []netip.Addr{netip.MustParseAddr("fe80::1")},
}

// testHostAddrs are a host's addresses on a LAN with public IPv4 and IPv6
// addresses, a private LAN, and a second address in the first network.
var testHostAddrs = []netip.Prefix{
	netip.MustParsePrefix("140.181.2.3/16"),
	netip.MustParsePrefix("2001:db8:1::abcd/64"),
	netip.MustParsePrefix("192.168.1.10/24"),
	netip.MustParsePrefix("140.181.7.7/24"),
	netip.MustParsePrefix("2001:db8:1::1234/64"),
	netip.MustParsePrefix("fe80::5/64"),
}

func TestRulesetHostAddrs(t *testing.T) {
	for name, tc := range map[string]struct {
		profile       config.NetworkProfile
		want, notWant []string
	}{
		"default": {
			profile: config.Builtin().NetworkProfiles["default"],
			want: []string{
				"ip daddr { 140.181.2.3, 192.168.1.10, 140.181.7.7 } drop\n",
				"ip daddr { 10.0.0.0/8, 100.64.0.0/10, 169.254.0.0/16, 172.16.0.0/12, 192.168.0.0/16, 224.0.0.0/4, 255.255.255.255/32, 140.181.0.0/16 } drop\n",
			},
			notWant: []string{"ip6 daddr {", "icmpv6"},
		},
		"ipv6 internet": {
			profile: config.NetworkProfile{Internet: true, IPv6: true},
			want: []string{
				"icmpv6 type { nd-neighbor-solicit, nd-neighbor-advert } ip6 hoplimit 255 accept\n",
				"ip6 daddr { 2001:db8:1::abcd, 2001:db8:1::1234, fe80::5 } drop\n",
				"ip6 daddr { fc00::/7, fe80::/10, ff00::/8, 2001:db8:1::/64 } drop\n",
			},
		},
		"lan without host": {
			profile: config.NetworkProfile{LAN: true, IPv6: true},
			want: []string{
				"ip daddr { 140.181.2.3, 192.168.1.10, 140.181.7.7 } drop\n",
				"ip6 daddr { 2001:db8:1::abcd, 2001:db8:1::1234, fe80::5 } drop\n",
				"140.181.0.0/16 } accept\n",
				"2001:db8:1::/64 } accept\n",
			},
		},
		"open": {
			profile: config.Builtin().NetworkProfiles["open"],
			notWant: []string{"140.181.2.3", "2001:db8:1::abcd"},
		},
	} {
		t.Run(name, func(t *testing.T) {
			got := Ruleset(tc.profile, testGateways, testHostAddrs, nil)
			for _, s := range tc.want {
				if !strings.Contains(got, s) {
					t.Errorf("missing %q in\n%s", s, got)
				}
			}
			for _, s := range tc.notWant {
				if strings.Contains(got, s) {
					t.Errorf("unexpected %q in\n%s", s, got)
				}
			}
		})
	}
}

func TestHostAddrs(t *testing.T) {
	prefixes, err := hostAddrs()
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range prefixes {
		if !p.IsValid() || p.Addr().IsLoopback() {
			t.Errorf("hostAddrs() returned %v", p)
		}
	}
}

func TestToPrefix(t *testing.T) {
	for _, tc := range []struct {
		addr net.Addr
		want string
	}{
		{&net.IPNet{IP: net.ParseIP("140.181.2.3"), Mask: net.CIDRMask(16, 32)}, "140.181.2.3/16"},
		{&net.IPNet{IP: net.ParseIP("140.181.2.3"), Mask: net.CIDRMask(112, 128)}, "140.181.2.3/16"},
		{&net.IPNet{IP: net.ParseIP("2001:db8::1"), Mask: net.CIDRMask(64, 128)}, "2001:db8::1/64"},
	} {
		if got, ok := toPrefix(tc.addr); !ok || got.String() != tc.want {
			t.Errorf("toPrefix(%v) = %v, %v; want %s", tc.addr, got, ok, tc.want)
		}
	}
	if _, ok := toPrefix(&net.IPAddr{IP: net.ParseIP("10.0.0.1")}); ok {
		t.Error("toPrefix accepted an address without a mask")
	}
}

func TestRulesetDefaultProfile(t *testing.T) {
	got := Ruleset(config.Builtin().NetworkProfiles["default"], testGateways, nil, nil)
	want := `table inet brig {
	chain output {
		type filter hook output priority filter; policy drop;
		oif "lo" accept
		ct state established,related accept
		meta l4proto udp ct state new ct count over 1024 drop
		ip daddr 169.254.1.1 meta l4proto { tcp, udp } th dport 53 accept
		ip daddr 169.254.1.1 drop
		ip daddr 192.168.1.1 drop
		ip daddr { 10.0.0.0/8, 100.64.0.0/10, 169.254.0.0/16, 172.16.0.0/12, 192.168.0.0/16, 224.0.0.0/4, 255.255.255.255/32 } drop
		meta nfproto ipv6 drop
		accept
	}
}
`
	if got != want {
		t.Fatalf("Ruleset() =\n%s\nwant\n%s", got, want)
	}
}

func TestRulesetVariants(t *testing.T) {
	for name, tc := range map[string]struct {
		profile       config.NetworkProfile
		gateways      Gateways
		want, notWant []string
	}{
		"isolated": {
			profile:  config.NetworkProfile{},
			gateways: testGateways,
			want:     []string{"ip daddr 192.168.1.1 drop", "} drop\n", "meta nfproto ipv6 drop"},
			notWant:  []string{"th dport 53", "\t\taccept\n\t}"},
		},
		"host ports": {
			profile:  config.NetworkProfile{Internet: true, HostPorts: []uint16{11434, 8080}},
			gateways: testGateways,
			want:     []string{"ip daddr 192.168.1.1 tcp dport { 11434, 8080 } accept", "ip daddr 192.168.1.1 drop"},
		},
		"open": {
			profile:  config.Builtin().NetworkProfiles["open"],
			gateways: testGateways,
			want:     []string{"ip daddr 192.168.1.1 accept", "ip6 daddr fe80::1 accept", "} accept\n", "ip6 daddr { fc00::/7, fe80::/10, ff00::/8 } accept"},
			notWant:  []string{"nfproto ipv6 drop"},
		},
		"lan without host keeps the gateway closed": {
			profile:  config.NetworkProfile{LAN: true, IPv6: true},
			gateways: testGateways,
			want:     []string{"ip daddr 192.168.1.1 drop", "ip6 daddr fe80::1 drop", "th dport 53 accept"},
		},
		"no default route": {
			profile: config.NetworkProfile{Internet: true, HostPorts: []uint16{22}},
			notWant: []string{"tcp dport { 22 }"},
		},
	} {
		t.Run(name, func(t *testing.T) {
			got := Ruleset(tc.profile, tc.gateways, nil, nil)
			for _, s := range tc.want {
				if !strings.Contains(got, s) {
					t.Errorf("missing %q in\n%s", s, got)
				}
			}
			for _, s := range tc.notWant {
				if strings.Contains(got, s) {
					t.Errorf("unexpected %q in\n%s", s, got)
				}
			}
		})
	}
}

// TestRulesetLoads checks the rulesets with nft in a throwaway namespace
// when the host allows unprivileged user namespaces.
func TestRulesetLoads(t *testing.T) {
	if _, err := exec.LookPath("nft"); err != nil {
		t.Skip("nft not installed")
	}
	if exec.Command("unshare", "-Urn", "true").Run() != nil {
		t.Skip("unprivileged user and network namespaces unavailable")
	}
	profiles := config.Builtin().NetworkProfiles
	profiles["ports"] = config.NetworkProfile{Internet: true, HostPorts: []uint16{11434}, IPv6: true}
	for name, p := range profiles {
		cmd := exec.Command("unshare", "-Urn", "nft", "-c", "-f", "-")
		cmd.Stdin = strings.NewReader(Ruleset(p, testGateways, testHostAddrs, testRoutes))
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Errorf("profile %s: nft rejected the ruleset: %v\n%s", name, err, out)
		}
	}
}

// testRoutes are a host's routes: a VPN's intranet, an on-link network of
// a DHCPv6 /128 address, and VPN routes that stand in for a default route.
var testRoutes = []netip.Prefix{
	netip.MustParsePrefix("131.169.0.0/16"),
	netip.MustParsePrefix("2001:db8:2::/64"),
	netip.MustParsePrefix("0.0.0.0/1"),
	netip.MustParsePrefix("128.0.0.0/1"),
	netip.MustParsePrefix("10.8.0.0/24"),
}

func TestRulesetRoutesAndWideNetworks(t *testing.T) {
	hostAddrs := []netip.Prefix{
		netip.MustParsePrefix("100.20.30.40/8"),    // contains 100.64.0.0/10
		netip.MustParsePrefix("2001:db8:2::5/128"), // DHCPv6
	}
	got := Ruleset(config.Builtin().NetworkProfiles["default"], testGateways, hostAddrs, testRoutes)
	for _, s := range []string{
		"ip daddr { 10.0.0.0/8, 169.254.0.0/16, 172.16.0.0/12, 192.168.0.0/16, 224.0.0.0/4, 255.255.255.255/32, 100.0.0.0/8, 131.169.0.0/16 } drop\n",
		"ip daddr 169.254.1.1 drop\n",
	} {
		if !strings.Contains(got, s) {
			t.Errorf("missing %q in\n%s", s, got)
		}
	}
	if strings.Contains(got, "0.0.0.0/1") || strings.Contains(got, "128.0.0.0/1") {
		t.Errorf("a route standing in for the default route counts as the LAN:\n%s", got)
	}
	got = Ruleset(config.NetworkProfile{Internet: true, IPv6: true}, testGateways, hostAddrs, testRoutes)
	if !strings.Contains(got, "ip6 daddr { fc00::/7, fe80::/10, ff00::/8, 2001:db8:2::/64 } drop") {
		t.Errorf("the on-link network of a /128 address is not the LAN:\n%s", got)
	}
}

func TestRulesetMultipathGateways(t *testing.T) {
	gws := Gateways{IPv4: []netip.Addr{netip.MustParseAddr("192.168.1.1"), netip.MustParseAddr("192.168.2.1")}}
	got := Ruleset(config.NetworkProfile{Internet: true, HostPorts: []uint16{8080}}, gws, nil, nil)
	for _, s := range []string{
		"ip daddr { 192.168.1.1, 192.168.2.1 } tcp dport { 8080 } accept",
		"ip daddr { 192.168.1.1, 192.168.2.1 } drop",
	} {
		if !strings.Contains(got, s) {
			t.Errorf("missing %q in\n%s", s, got)
		}
	}
}

func TestParseRoutes(t *testing.T) {
	out := `[{"dst":"default","nexthops":[{"gateway":"192.0.2.1","dev":"eth0"},{"gateway":"198.51.100.1","dev":"eth1"}]},
		{"dst":"default","gateway":"192.0.2.1","dev":"eth0","metric":600},
		{"dst":"192.0.2.0/24","dev":"eth0","protocol":"kernel","scope":"link"},
		{"dst":"203.0.113.7","gateway":"192.0.2.1","dev":"eth0"},
		{"type":"blackhole","dst":"198.18.0.0/15"}]`
	routes, err := parseRoutes([]byte(out))
	if err != nil {
		t.Fatal(err)
	}
	if got := gateways(routes); !reflect.DeepEqual(got, []netip.Addr{netip.MustParseAddr("192.0.2.1"), netip.MustParseAddr("198.51.100.1")}) {
		t.Errorf("gateways = %v", got)
	}
	var prefixes []string
	for _, r := range routes {
		if p, ok := r.prefix(); ok {
			prefixes = append(prefixes, p.String())
		}
	}
	if want := []string{"192.0.2.0/24", "203.0.113.7/32"}; !reflect.DeepEqual(prefixes, want) {
		t.Errorf("prefixes = %v, want %v", prefixes, want)
	}
	if routes, err := parseRoutes([]byte("\n")); err != nil || routes != nil {
		t.Errorf("empty output: %v, %v", routes, err)
	}
}

func TestCommand(t *testing.T) {
	c := Config{
		VM: "dev", Socket: "/run/user/1000/brig/dev/net.sock",
		SSHPort: 40022, GatewayPort: 40670, GuestSSHPort: 22, GuestGatewayPort: 17671,
		Profile:   config.NetworkProfile{Internet: true},
		HostAddrs: testHostAddrs[:2],
	}
	argv, err := c.Command("/usr/bin/pasta", "/home/u/bin/brig")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"/usr/bin/pasta", "--config-net", "--quiet", "--dns-forward", "169.254.1.1",
		"-t", "127.0.0.1/40022:2222", "-t", "127.0.0.1/40670:17671",
		"-u", "none", "-T", "none", "-U", "none", "-4", "--",
		"/bin/sh", "-c", `exec "$0" "$@"`, "/home/u/bin/brig",
		"net-helper", "--socket", "/run/user/1000/brig/dev/net.sock",
		"--guest-ssh-port", "22", "--guest-gateway-port", "17671",
		"--profile", `{"internet":true,"host":false,"lan":false,"ipv6":false}`,
		"--host-addr", "140.181.2.3/16", "--host-addr", "2001:db8:1::abcd/64",
	}
	if !reflect.DeepEqual(argv, want) {
		t.Fatalf("Command() =\n%q\nwant\n%q", argv, want)
	}

	passt := c.passtArgs("/usr/bin/passt")
	for _, s := range []string{"--vhost-user", "--one-off", "--map-host-loopback none", "-t 2222:22", "-t 17671:17671", "-4"} {
		if !strings.Contains(strings.Join(passt, " "), s) {
			t.Errorf("passt args %q lack %q", passt, s)
		}
	}

	bad := c
	bad.Socket = "relative.sock"
	if _, err := bad.Command("pasta", "brig"); err == nil {
		t.Error("relative socket path accepted")
	}
}

func TestProfileRoundTrip(t *testing.T) {
	p := config.NetworkProfile{Internet: true, HostPorts: []uint16{11434}}
	s, err := encodeProfile(p)
	if err != nil {
		t.Fatal(err)
	}
	got, err := decodeProfile(s)
	if err != nil || !reflect.DeepEqual(got, p) {
		t.Fatalf("decodeProfile(%s) = %+v, %v", s, got, err)
	}
	if _, err := decodeProfile(`{"bogus":true}`); err == nil {
		t.Error("unknown profile field accepted")
	}
}

// fakeUnit puts a systemctl script with the given body and a journalctl
// that prints one line first in PATH.
func fakeUnit(t *testing.T, body string) {
	t.Helper()
	dir := t.TempDir()
	scripts := map[string]string{
		"systemctl":  "#!/bin/sh\n" + body + "\n",
		"journalctl": "#!/bin/sh\necho 'pasta: nft failed'\n",
	}
	for name, script := range scripts {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(script), 0o755); err != nil { //nolint:gosec // G306: the fakes must be executable
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func TestWaitForSocket(t *testing.T) {
	socket := filepath.Join(t.TempDir(), "net.sock")
	unit := UnitName("test")

	// A collected unit is not active any more, so the failure is reported
	// at once instead of after the timeout.
	fakeUnit(t, `exit 3`)
	start := time.Now()
	err := waitForSocket(context.Background(), socket, unit)
	if err == nil || !strings.Contains(err.Error(), "exited before it created") || !strings.Contains(err.Error(), "pasta: nft failed") {
		t.Fatalf("waitForSocket() = %v, want the exit with the journal", err)
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Errorf("waitForSocket() took %v after the unit exited", elapsed)
	}

	// A unit that runs and makes no socket is waited for until the context ends.
	fakeUnit(t, `exit 0`)
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	if err := waitForSocket(ctx, socket, unit); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("waitForSocket() = %v, want the context's deadline", err)
	}

	ln, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	socketSettle = 300 * time.Millisecond
	defer func() { socketSettle = time.Second }()
	start = time.Now()
	if err := waitForSocket(context.Background(), socket, unit); err != nil {
		t.Fatalf("waitForSocket() with a listening socket = %v", err)
	}
	if elapsed := time.Since(start); elapsed < socketSettle {
		t.Errorf("waitForSocket() returned after %v, before passt settled", elapsed)
	}

	// passt binds its socket before it sandboxes itself, which may fail.
	fakeUnit(t, `exit 3`)
	err = waitForSocket(context.Background(), socket, unit)
	if err == nil || !strings.Contains(err.Error(), "exited after it created") || !strings.Contains(err.Error(), "pasta: nft failed") {
		t.Fatalf("waitForSocket() = %v, want the exit with the journal", err)
	}
	if err := Exited(context.Background(), "test"); err == nil || !strings.Contains(err.Error(), "has exited: pasta: nft failed") {
		t.Errorf("Exited() = %v", err)
	}
	fakeUnit(t, `exit 0`)
	if err := Exited(context.Background(), "test"); err != nil {
		t.Errorf("Exited() of a running unit = %v", err)
	}
}
