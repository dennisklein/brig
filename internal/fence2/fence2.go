// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

// Package fence2 records what a VM's network profile, fence 2, drops. The
// firewall that package vmnet loads in the VM's network namespace sends
// every drop to the chain of its reason, which counts it in a named counter
// and, below a rate limit, logs it to an NFLOG group. A listener in the
// same namespace reads the group and the counters, sums the drops up per
// window and appends them to a spool file in the VM's audit directory,
// which the VM's audit collector reads into the audit trail.
//
// Fence 2 sees packets, not sandboxes: every packet leaves the VM through
// passt, so a drop says where the VM tried to go and why it was dropped,
// not which program sent it.
package fence2

import "strings"

// The reasons of a drop: the profile setting that dropped it.
const (
	// ReasonInternet: an address that no other rule covers, while the
	// profile's internet is off.
	ReasonInternet = "internet"
	// ReasonLAN: a private, link-local or multicast range, or a network
	// of the host's addresses or routes, while lan is off.
	ReasonLAN = "lan"
	// ReasonHost: the host's gateway or one of its own addresses, while
	// host is off.
	ReasonHost = "host"
	// ReasonHostPort: a port of the host's gateway that host_ports does
	// not list.
	ReasonHostPort = "host-port"
	// ReasonIPv6: IPv6, while ipv6 is off.
	ReasonIPv6 = "ipv6"
	// ReasonDNS: the resolver address other than plain DNS, or DNS while
	// the profile allows nothing to resolve.
	ReasonDNS = "dns"
	// ReasonUDPFlows: more UDP flows at once than a VM may have.
	ReasonUDPFlows = "udp-flows"
)

// Reasons are the reasons of drops, in the order the firewall checks them
// roughly.
var Reasons = []string{ReasonUDPFlows, ReasonDNS, ReasonHost, ReasonHostPort, ReasonLAN, ReasonIPv6, ReasonInternet}

// Chain is the name of the nftables chain that counts and logs the drops of
// reason, and drops the packet.
func Chain(reason string) string { return "drop_" + strings.ReplaceAll(reason, "-", "_") }

// The firewall's logging of drops.
const (
	// Group is the NFLOG group the drops are logged to.
	Group = 1
	// Snaplen is how many bytes of each dropped packet are logged: enough
	// for its IP and transport headers' addresses and ports.
	Snaplen = 64
	// LogRate and LogBurst limit the drops logged per second and reason;
	// the counters count the rest.
	LogRate  = 20
	LogBurst = 100
)

// Mode is how fully the firewall records drops, and State says which mode
// a listener runs in.
type Mode string

// The modes.
const (
	// Itemised logs each drop below the rate limit with its address and
	// port, and counts every drop.
	Itemised Mode = "itemised"
	// CountsOnly counts the drops per reason only.
	CountsOnly Mode = "counts only"
	// Off records nothing: the firewall is the one from before drops
	// were recorded.
	Off Mode = "off"
)

// Explain says in a few words why the profile dropped a packet, as brig
// audit and brig ui show it.
func Explain(reason string) string {
	switch reason {
	case ReasonInternet:
		return "internet is off"
	case ReasonLAN:
		return "lan is off"
	case ReasonHost:
		return "host is off"
	case ReasonHostPort:
		return "port not in host_ports"
	case ReasonIPv6:
		return "ipv6 is off"
	case ReasonDNS:
		return "only plain DNS goes to the resolver"
	case ReasonUDPFlows:
		return "too many UDP flows at once"
	}
	return "dropped (" + reason + ")"
}

// Setting is the profile setting that would let the drops of reason
// through, empty for reasons that no setting changes.
func Setting(reason string) string {
	switch reason {
	case ReasonInternet, ReasonLAN, ReasonHost, ReasonIPv6:
		return reason
	case ReasonHostPort:
		return "host_ports"
	}
	return ""
}
