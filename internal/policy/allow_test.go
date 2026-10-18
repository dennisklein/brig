// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

package policy

import (
	"fmt"
	"slices"
	"strings"
	"testing"
)

func TestCheckHost(t *testing.T) {
	for host, ok := range map[string]bool{
		"registry.npmjs.org":  true,
		"localhost":           true,
		"10.0.2.2":            true,
		"my-host.example":     true,
		"*.example.com":       true,
		"**.example.com":      true,
		"*.eu.example.com":    true,
		"api-*.example.com":   true,
		"*":                   false,
		"**":                  false,
		"*.com":               false,
		"**.com":              false,
		"*.*.com":             false,
		"example.*":           false,
		"example.com.*":       false,
		"10.0.*.*":            false,
		"":                    false,
		"a:b":                 false,
		"a.example/x":         false,
		"a b.example":         false,
		"*.com.":              false,
		"*.example.com.":      false,
		"registry.npmjs.org.": false,
		".example.com":        false,
		"a..example.com":      false,
		"*..example.com":      false,
		"*.co.uk":             false,
		"*.github.io":         false,
		"api.*.co.uk":         false,
		"*.example.co.uk":     true,
		"*.alice.github.io":   true,
		"github.io":           true,
	} {
		if err := CheckHost(host); (err == nil) != ok {
			t.Errorf("CheckHost(%q) = %v, want ok = %v", host, err, ok)
		}
	}
}

func TestRuleName(t *testing.T) {
	for host, want := range map[string]string{
		"registry.npmjs.org": "allow_registry_npmjs_org_443",
		"my-host.example":    "allow_my-host_example_443",
		"my_host.example":    "allow_myUhost_example_443",
		"*.example.com":      "allow_ANY_example_com_443",
		"**.example.com":     "allow_ANYANY_example_com_443",
		"api-*.example.com":  "allow_api-ANY_example_com_443",
		"*.eu.example.com":   "allow_ANY_eu_example_com_443",
		"10.0.2.2":           "allow_10_0_2_2_443",
	} {
		if got := RuleName(host, 443); got != want {
			t.Errorf("RuleName(%q) = %q, want %q", host, got, want)
		}
	}
	// Different hosts and ports get different keys.
	seen := map[string]string{}
	for _, hp := range []struct {
		host string
		port int
	}{
		{"a-b.example.com", 443},
		{"a.b.example.com", 443},
		{"a_b.example.com", 443},
		{"*.example.com", 443},
		{"any.example.com", 443},
		{"wildcard.example.com", 443},
		{"**.example.com", 443},
		{"api-*.example.com", 443},
		{"example.com", 443},
		{"a.example.com", 1443},
		{"a.example.com_1", 443},
		{"a.example.com", 443},
	} {
		key := RuleName(hp.host, hp.port)
		if other, ok := seen[key]; ok {
			t.Errorf("RuleName gives %s to %s:%d and %s", key, hp.host, hp.port, other)
		}
		seen[key] = fmt.Sprintf("%s:%d", hp.host, hp.port)
	}
}

func TestBroad(t *testing.T) {
	for _, c := range []struct {
		protocol, access, binary string
		n                        int
	}{
		{"rest", "read-only", "/usr/bin/node", 0},
		{"rest", "read-write", "/usr/bin/node", 0},
		{"rest", "full", "/usr/bin/node", 1},
		{"", "", "/usr/bin/node", 1},
		{"", "full", "/usr/bin/node", 1},
		{"rest", "read-only", "/usr/bin/*", 1},
		{"", "", "/**", 2},
	} {
		if got := Broad(c.protocol, c.access, c.binary); len(got) != c.n {
			t.Errorf("Broad(%q, %q, %q) = %q, want %d reasons", c.protocol, c.access, c.binary, got, c.n)
		}
	}
}

func TestEndpoints(t *testing.T) {
	raw, err := RawRules([]byte(`network_policies:
  r:
    endpoints:
      - {host: registry.npmjs.org, port: 443, protocol: rest, enforcement: enforce}
      - {host: pypi.org, ports: [80, 443], enforcement: audit}
      - {host: example.org}
`))
	if err != nil {
		t.Fatal(err)
	}
	got := Endpoints(raw["r"])
	want := []string{"registry.npmjs.org:443", "pypi.org:80 (audit)", "pypi.org:443 (audit)", "example.org:443"}
	if len(got) != len(want) {
		t.Fatalf("Endpoints = %q, want %q", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("Endpoints = %q, want %q", got, want)
			break
		}
	}
}

func TestOtherEndpoints(t *testing.T) {
	raw, err := RawRules([]byte(`network_policies:
  r:
    endpoints:
      - {host: registry.npmjs.org, port: 443}
      - {host: registry.npmjs.org, ports: [80, 443]}
      - {host: evil.example.com}
`))
	if err != nil {
		t.Fatal(err)
	}
	if got, want := OtherEndpoints(raw["r"], "registry.npmjs.org", 443), []string{"registry.npmjs.org:80", "evil.example.com:443"}; !slices.Equal(got, want) {
		t.Errorf("OtherEndpoints = %q, want %q", got, want)
	}
	if got := OtherEndpoints(map[string]any{}, "registry.npmjs.org", 443); len(got) != 0 {
		t.Errorf("OtherEndpoints of no endpoints = %q", got)
	}
}

func TestMergeBroad(t *testing.T) {
	raw, err := RawRules([]byte(`network_policies:
  l4:
    endpoints: [{host: api.example.com, port: 443}]
    binaries: [{path: /usr/bin/curl}]
  full:
    endpoints: [{host: api.example.com, port: 443, protocol: rest, access: full, enforcement: enforce}]
    binaries: [{path: /usr/bin/curl}]
  narrow:
    endpoints: [{host: api.example.com, port: 443, protocol: rest, access: read-only, enforcement: enforce}]
    binaries: [{path: /usr/bin/curl}]
  wild:
    endpoints: [{host: api.example.com, port: 443, protocol: rest, access: read-only, enforcement: enforce}]
    binaries: [{path: /usr/bin/*}]
`))
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		rule, binary string
		want         string // part of the one reason, or "" for none
	}{
		{"l4", "/usr/bin/node", "the whole connection to api.example.com:443"},
		{"l4", "/usr/bin/curl", ""},
		{"full", "/usr/bin/node", "every HTTP method to api.example.com:443"},
		{"full", "/usr/bin/curl", ""},
		{"narrow", "/usr/bin/node", ""},
		{"wild", "/usr/bin/node", "the programs /usr/bin/*"},
		{"wild", "/usr/bin/*", ""},
	} {
		got := MergeBroad(raw[c.rule], "pypi.org", 443, c.binary)
		switch {
		case c.want == "" && len(got) != 0:
			t.Errorf("MergeBroad(%s, %s) = %q, want none", c.rule, c.binary, got)
		case c.want != "" && (len(got) != 1 || !strings.Contains(got[0], c.want)):
			t.Errorf("MergeBroad(%s, %s) = %q, want %q", c.rule, c.binary, got, c.want)
		}
	}
}
