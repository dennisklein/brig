// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"strings"
	"testing"
)

// TestShotListOfTheRepo checks the list that the docs use.
func TestShotListOfTheRepo(t *testing.T) {
	l, err := loadShots("../../docs/images/shots.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if len(l.Shots) == 0 {
		t.Fatal("no shots")
	}
}

func TestShotListChecks(t *testing.T) {
	sizes := map[string]Size{"desktop": {1440, 900}}
	for name, c := range map[string]struct {
		shots Shots
		want  string
	}{
		"no shots":     {Shots{Sizes: sizes}, "no shots"},
		"no path":      {Shots{Sizes: sizes, Shots: []Shot{{Name: "a", Alt: "A"}}}, "needs a name, a path and an alt"},
		"twice":        {Shots{Sizes: sizes, Shots: []Shot{{Name: "a", Alt: "A", Path: "/"}, {Name: "a", Alt: "A", Path: "/"}}}, "listed twice"},
		"unknown size": {Shots{Sizes: sizes, Shots: []Shot{{Name: "a", Alt: "A", Path: "/", Sizes: []string{"tv"}}}}, `no size "tv"`},
		"two things":   {Shots{Sizes: sizes, Shots: []Shot{{Name: "a", Alt: "A", Path: "/", Steps: []Step{{Click: "x", Wait: "y"}}}}}, "exactly one"},
		"type alone":   {Shots{Sizes: sizes, Shots: []Shot{{Name: "a", Alt: "A", Path: "/", Steps: []Step{{Type: "x"}}}}}, "go together"},
	} {
		err := c.shots.check()
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: got %v, want an error with %q", name, err, c.want)
		}
	}
}

func TestShotFiles(t *testing.T) {
	s := Shot{Name: "overview", Sizes: []string{"desktop", "phone"}}
	got := strings.Join(s.Files(), " ")
	want := "overview-light.png overview-dark.png overview-phone-light.png overview-phone-dark.png"
	if got != want {
		t.Errorf("files are %q, want %q", got, want)
	}
}
