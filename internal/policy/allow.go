// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

package policy

import (
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// hostRE matches the characters of a host name, an IPv4 address or a
// wildcard such as *.example.com.
var hostRE = regexp.MustCompile(`^[a-z0-9*_][a-z0-9*_.-]{0,252}$`)

// CheckHost reports why host, in lower case, is no host that a rule for one
// service may name. It admits host names, IPv4 addresses and wildcards that
// stand above a domain of at least two labels: *.example.com is a rule for a
// service, while *, ** and *.com would let a sandbox reach any host, or any
// host of a top-level domain.
func CheckHost(host string) error {
	if !hostRE.MatchString(host) {
		return errors.New("write a host name or IPv4 address, such as registry.npmjs.org")
	}
	if !strings.Contains(host, "*") {
		return nil
	}
	labels := strings.Split(host, ".")
	if n := len(labels); n < 3 || strings.Contains(labels[n-1], "*") || strings.Contains(labels[n-2], "*") {
		return fmt.Errorf("a wildcard needs a domain of two labels below it, such as *.example.com; %s would allow far more than one service", host)
	}
	return nil
}

// RuleName returns the key that a rule for host and port gets when the user
// names none: allow_HOST_PORT, after the host that the rule is for. A
// wildcard host is named after the domain below its wildcard, with
// "wildcard" in front, so that *.example.com does not take the name of the
// rule for example.com or come out as allow__example_com_443.
func RuleName(host string, port int) string {
	labels := strings.Split(host, ".")
	wild := false
	for i := len(labels) - 1; i >= 0; i-- {
		if strings.Contains(labels[i], "*") {
			labels, wild = labels[i+1:], true
			break
		}
	}
	var b strings.Builder
	b.WriteString("allow_")
	if wild {
		b.WriteString("wildcard_")
	}
	for _, r := range strings.NewReplacer(".", "_", "-", "_").Replace(strings.Join(labels, ".")) {
		if r == '_' || ('a' <= r && r <= 'z') || ('A' <= r && r <= 'Z') || ('0' <= r && r <= '9') {
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

// Endpoints lists the endpoints of a rule as host:port, as RawRules returns
// the rule. An endpoint with a port list gives one entry per port, and one
// that only logs says so.
func Endpoints(rule map[string]any) []string {
	var out []string
	eps, _ := rule["endpoints"].([]any)
	for _, e := range eps {
		ep, ok := e.(map[string]any)
		if !ok {
			continue
		}
		host, _ := ep["host"].(string)
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
		for _, p := range ports {
			s := fmt.Sprintf("%s:%v", host, p)
			if enf, _ := ep["enforcement"].(string); enf == "audit" {
				s += " (audit)"
			}
			out = append(out, s)
		}
	}
	return out
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
