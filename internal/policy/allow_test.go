// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

package policy

import "testing"

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
		"my-host.example":    "allow_my_host_example_443",
		"*.example.com":      "allow_wildcard_example_com_443",
		"**.example.com":     "allow_wildcard_example_com_443",
		"api-*.example.com":  "allow_wildcard_example_com_443",
		"*.eu.example.com":   "allow_wildcard_eu_example_com_443",
		"10.0.2.2":           "allow_10_0_2_2_443",
	} {
		if got := RuleName(host, 443); got != want {
			t.Errorf("RuleName(%q) = %q, want %q", host, got, want)
		}
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
