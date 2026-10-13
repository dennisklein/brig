// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

package policy

import "testing"

func TestCheckHost(t *testing.T) {
	for host, ok := range map[string]bool{
		"registry.npmjs.org": true,
		"localhost":          true,
		"10.0.2.2":           true,
		"my-host.example":    true,
		"*.example.com":      true,
		"**.example.com":     true,
		"*.eu.example.com":   true,
		"api-*.example.com":  true,
		"*":                  false,
		"**":                 false,
		"*.com":              false,
		"**.com":             false,
		"*.*.com":            false,
		"example.*":          false,
		"example.com.*":      false,
		"10.0.*.*":           false,
		"":                   false,
		"a:b":                false,
		"a.example/x":        false,
		"a b.example":        false,
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
