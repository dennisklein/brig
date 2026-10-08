// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

package deps

import (
	"slices"
	"testing"
)

func TestPackages(t *testing.T) {
	required, err := Packages()
	if err != nil {
		t.Fatal(err)
	}
	if !slices.IsSorted(required) || !slices.Contains(required, "passt") || slices.Contains(required, "virtiofsd") {
		t.Fatalf("Packages() = %v", required)
	}
	withMounts, err := Packages("mounts")
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(withMounts, "virtiofsd") || len(withMounts) != len(required)+1 {
		t.Fatalf("Packages(mounts) = %v", withMounts)
	}
	all, err := Packages("all")
	if err != nil || !slices.Contains(all, "podman") || !slices.Contains(all, "libsecret") || len(all) != len(withMounts)+2 {
		t.Fatalf("Packages(all) = %v, %v", all, err)
	}
	if _, err := Packages("bogus"); err == nil {
		t.Fatal("Packages(bogus) succeeded")
	}
}

func TestGroupsAreWellFormed(t *testing.T) {
	groups := Groups()
	if groups[0].Name != "required" {
		t.Fatal(`first group must be "required"`)
	}
	seen := map[string]bool{}
	for _, g := range groups {
		for _, p := range g.Packages {
			if p.Name == "" || p.Reason == "" || seen[p.Name] {
				t.Errorf("bad or duplicate package %+v in group %s", p, g.Name)
			}
			seen[p.Name] = true
		}
	}
	if got := OptionalGroupNames(); !slices.Equal(got, []string{"mounts", "push", "secrets"}) {
		t.Errorf("OptionalGroupNames() = %v", got)
	}
}
