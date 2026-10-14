// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/dennisklein/brig/internal/help"
)

// repoRoot returns the root of the source tree that the tests run in.
func repoRoot(t *testing.T) string {
	t.Helper()
	root, err := findRepo(".")
	if err != nil {
		t.Fatal(err)
	}
	return root
}

func itoa(n int) string { return strconv.Itoa(n) }

// TestReferenceIsCurrent checks that docs/reference holds what docgen makes
// of the code today, so that the reference cannot drift from it.
func TestReferenceIsCurrent(t *testing.T) {
	root := repoRoot(t)
	want, err := generate(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range want.paths() {
		got, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(p)))
		switch {
		case err != nil:
			t.Errorf("%s is missing; run go generate ./...", p)
		case string(got) != want[p]:
			t.Errorf("%s is out of date; run go generate ./...\n%s", p, firstDifference(string(got), want[p]))
		}
	}
	stale, err := want.stale(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range stale {
		t.Errorf("%s is generated no more; run go generate ./...", p)
	}
}

// firstDifference shows the first line where the texts differ.
func firstDifference(got, want string) string {
	g, w := strings.Split(got, "\n"), strings.Split(want, "\n")
	for i := 0; i < len(g) || i < len(w); i++ {
		var gl, wl string
		if i < len(g) {
			gl = g[i]
		}
		if i < len(w) {
			wl = w[i]
		}
		if gl != wl {
			return "line " + itoa(i+1) + ":\n  file:     " + gl + "\n  generated: " + wl
		}
	}
	return ""
}

func TestEveryCommandHasAPage(t *testing.T) {
	h, err := help.Load()
	if err != nil {
		t.Fatal(err)
	}
	pages := cliPages(commandTree(), h)
	for _, p := range []string{"brig.md", "brig-sandbox-create.md", "brig-policy-allow.md", "README.md"} {
		if _, ok := pages[cliDir+"/"+p]; !ok {
			t.Errorf("there is no page %s", p)
		}
	}
	for p := range pages {
		if strings.Contains(p, "topics") || strings.Contains(p, "net-helper") {
			t.Errorf("%s documents a command that is hidden", p)
		}
	}
}
