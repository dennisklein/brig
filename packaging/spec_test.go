// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

package packaging

import (
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/dennisklein/brig/internal/deps"
)

// specDeps returns the package names that the main package of
// brig/brig.spec lists under tag ("Requires" or "Recommends"), sorted. It
// leaves out rich dependencies, whose names are conditions.
func specDeps(t *testing.T, tag string) []string {
	t.Helper()
	data, err := os.ReadFile("brig/brig.spec")
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, "%package") || strings.HasPrefix(line, "%description") {
			break
		}
		rest, ok := strings.CutPrefix(line, tag+":")
		if !ok || strings.HasPrefix(strings.TrimSpace(rest), "(") {
			continue
		}
		fields := strings.Fields(rest)
		if len(fields) != 1 {
			t.Fatalf("%s: want a bare package name: %q", tag, line)
		}
		names = append(names, fields[0])
	}
	slices.Sort(names)
	return names
}

func groupPackages(g deps.Group) []string {
	var names []string
	for _, p := range g.Packages {
		names = append(names, p.Name)
	}
	return names
}

// TestSpecDeps keeps brig.spec's dependencies equal to internal/deps, which
// `brig print-fedora-deps` and `brig doctor` use.
func TestSpecDeps(t *testing.T) {
	groups := deps.Groups()

	// openshell is the only dependency that is not a host package of its own:
	// the CLI that must match the VMs' OpenShell, from the same repository.
	want := append(groupPackages(groups[0]), "openshell")
	slices.Sort(want)
	if got := specDeps(t, "Requires"); !slices.Equal(got, want) {
		t.Errorf("Requires = %v, want the required group and openshell: %v", got, want)
	}

	var optional []string
	for _, g := range groups[1:] {
		optional = append(optional, groupPackages(g)...)
	}
	slices.Sort(optional)
	if got := specDeps(t, "Recommends"); !slices.Equal(got, optional) {
		t.Errorf("Recommends = %v, want the optional groups: %v", got, optional)
	}
}

// TestSpecGo requires the Go that go.mod asks for.
func TestSpecGo(t *testing.T) {
	mod, err := os.ReadFile("../go.mod")
	if err != nil {
		t.Fatal(err)
	}
	spec, err := os.ReadFile("brig/brig.spec")
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`(?m)^go (\S+)$`).FindSubmatch(mod)
	if m == nil {
		t.Fatal("no go directive in go.mod")
	}
	if want := "BuildRequires:  golang >= " + string(m[1]); !strings.Contains(string(spec), "\n"+want+"\n") {
		t.Errorf("brig.spec lacks %q", want)
	}
}
