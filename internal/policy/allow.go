// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

package policy

import (
	"errors"
	"fmt"
	"regexp"
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
