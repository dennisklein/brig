// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

package policy

import (
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"golang.org/x/net/publicsuffix"
)

// hostRE matches the characters of a host name, an IPv4 address or a
// wildcard such as *.example.com.
var hostRE = regexp.MustCompile(`^[a-z0-9*_][a-z0-9*_.-]{0,252}$`)

// CheckHost reports why host, in lower case, is no host that a rule for one
// service may name. It admits host names, IPv4 addresses and wildcards that
// stand above a registrable domain: *.example.com is a rule for a service,
// while *, ** and *.com would let a sandbox reach any host, or any host of a
// top-level domain. A public suffix such as co.uk or github.io is no
// registrable domain either, since the hosts below it belong to different
// owners.
func CheckHost(host string) error {
	if !hostRE.MatchString(host) {
		return errors.New("write a host name or IPv4 address, such as registry.npmjs.org")
	}
	labels := strings.Split(host, ".")
	if slices.Contains(labels, "") {
		return errors.New("write the host without empty labels or a dot at the end, such as registry.npmjs.org")
	}
	if !strings.Contains(host, "*") {
		return nil
	}
	if n := len(labels); n < 3 || strings.Contains(labels[n-1], "*") || strings.Contains(labels[n-2], "*") {
		return fmt.Errorf("a wildcard needs a domain of two labels below it, such as *.example.com; %s would allow far more than one service", host)
	}
	if domain := strings.Join(labels[lastWild(labels)+1:], "."); isPublicSuffix(domain) {
		return fmt.Errorf("%s is a public suffix, below which hosts belong to different owners; %s would allow far more than one service", domain, host)
	}
	return nil
}

// lastWild returns the index of the last label with a wildcard, or -1.
func lastWild(labels []string) int {
	for i := len(labels) - 1; i >= 0; i-- {
		if strings.Contains(labels[i], "*") {
			return i
		}
	}
	return -1
}

// isPublicSuffix reports whether domain is on the public suffix list, as
// com, co.uk and github.io are.
func isPublicSuffix(domain string) bool {
	suffix, _ := publicsuffix.PublicSuffix(domain)
	return suffix == domain
}

// RuleName returns the key that a rule for host and port gets when the user
// names none: allow_HOST_PORT, after the host that the rule is for, such as
// allow_registry_npmjs_org_443. The dots of the host become _, a * becomes
// ANY and a _ becomes U, so that each host and port has a key of its own:
// a-b.example.com, a.b.example.com and a_b.example.com do not share a rule,
// nor do *.example.com (allow_ANY_example_com_443) and
// any.example.com. Host names are in lower case, so the capitals cannot
// stand for a letter of one.
func RuleName(host string, port int) string {
	var b strings.Builder
	b.WriteString("allow_")
	for _, r := range host {
		switch {
		case r == '.':
			b.WriteByte('_')
		case r == '*':
			b.WriteString("ANY")
		case r == '_':
			b.WriteByte('U')
		case r == '-' || ('a' <= r && r <= 'z') || ('0' <= r && r <= '9'):
			b.WriteRune(r)
		}
	}
	return fmt.Sprintf("%s_%d", b.String(), port)
}

// Broad lists the ways in which a rule lets a sandbox do more
// than its user is likely to expect, for the user to confirm: the
// protocol is "rest" to inspect HTTP requests, or empty for the whole
// connection, access is the HTTP access level, and binary is the program
// path. The list is empty for a rule that needs no confirmation.
func Broad(protocol, access, binary string) []string {
	var why []string
	switch {
	case protocol == "":
		why = append(why, "the whole connection is allowed, so the proxy does not look at the HTTP requests in it")
	case access == "full":
		why = append(why, "full access allows every HTTP method, including DELETE and PUT")
	}
	if strings.Contains(binary, "*") {
		why = append(why, "the program path has a wildcard, so more than one program may connect")
	}
	return why
}

// endpoint is one host and port of a rule's endpoint.
type endpoint struct {
	host, port       string
	protocol, access string
	audit            bool
}

func (e endpoint) String() string { return e.host + ":" + e.port }

// endpoints lists the endpoints of a rule as RawRules or Parse returns the
// rule, one entry per port.
func endpoints(rule map[string]any) []endpoint {
	var out []endpoint
	eps, _ := rule["endpoints"].([]any)
	for _, e := range eps {
		ep, ok := e.(map[string]any)
		if !ok {
			continue
		}
		var ports []any
		if p, ok := ep["port"]; ok && p != nil {
			ports = append(ports, p)
		}
		if l, ok := ep["ports"].([]any); ok {
			ports = append(ports, l...)
		}
		if len(ports) == 0 {
			ports = []any{defaultPort}
		}
		host, _ := ep["host"].(string)
		protocol, _ := ep["protocol"].(string)
		access, _ := ep["access"].(string)
		enf, _ := ep["enforcement"].(string)
		for _, p := range ports {
			out = append(out, endpoint{host: host, port: fmt.Sprint(p), protocol: protocol, access: access, audit: enf == "audit"})
		}
	}
	return out
}

// binaries lists the program paths of a rule as RawRules or Parse returns
// the rule.
func binaries(rule map[string]any) []string {
	var out []string
	bins, _ := rule["binaries"].([]any)
	for _, b := range bins {
		switch b := b.(type) {
		case string:
			out = append(out, b)
		case map[string]any:
			if p, ok := b["path"].(string); ok {
				out = append(out, p)
			}
		}
	}
	return out
}

// Endpoints lists the endpoints of a rule as host:port, as RawRules returns
// the rule. An endpoint with a port list gives one entry per port, and one
// that only logs says so.
func Endpoints(rule map[string]any) []string {
	var out []string
	for _, e := range endpoints(rule) {
		s := e.String()
		if e.audit {
			s += " (audit)"
		}
		out = append(out, s)
	}
	return out
}

// OtherEndpoints lists the endpoints of rule, as RawRules or Parse returns
// it, that are not for host and port, as host:port.
func OtherEndpoints(rule map[string]any, host string, port int) []string {
	var out []string
	for _, e := range endpoints(rule) {
		if s := e.String(); !strings.EqualFold(e.host, host) || e.port != strconv.Itoa(port) {
			if !slices.Contains(out, s) {
				out = append(out, s)
			}
		}
	}
	return out
}

// MergeBroad lists the ways in which adding an endpoint for host and port
// and the program binary to rule, a sandbox's rule as RawRules or Parse
// returns it, lets a sandbox do more than Broad says of the endpoint and
// program on their own: the gateway merges them into the rule, in which
// every program reaches every endpoint. So binary gets the whole connection
// or full access that an endpoint of the rule has, and a program path of
// the rule with a wildcard reaches the new endpoint. The list is empty for a
// merge that needs no confirmation beyond Broad's.
func MergeBroad(rule map[string]any, host string, port int, binary string) []string {
	var why []string
	add := func(s string) {
		if !slices.Contains(why, s) {
			why = append(why, s)
		}
	}
	bins := binaries(rule)
	if !slices.Contains(bins, binary) {
		for _, e := range endpoints(rule) {
			switch {
			case e.protocol == "":
				add(fmt.Sprintf("the rule allows the whole connection to %s, so the proxy does not look at the HTTP requests of %s to it either", e, binary))
			// openshell may spell the access level as its enum value.
			case e.access == "full" || strings.HasSuffix(e.access, "_FULL"):
				add(fmt.Sprintf("the rule allows every HTTP method to %s, which %s gets too", e, binary))
			}
		}
	}
	for _, b := range bins {
		if b != binary && strings.Contains(b, "*") {
			add(fmt.Sprintf("the rule is for the programs %s, so more than one program may connect to %s:%d", b, host, port))
		}
	}
	return why
}

// EndpointSpec spells an endpoint as openshell policy update takes it in
// --add-endpoint: HOST:PORT for the whole connection, or
// HOST:PORT:ACCESS:rest:ENFORCEMENT for HTTP requests, where protocol is
// "rest".
func EndpointSpec(host string, port int, protocol, access, enforcement string) string {
	spec := host + ":" + strconv.Itoa(port)
	if protocol == "rest" {
		spec += ":" + access + ":rest:" + enforcement
	}
	return spec
}
