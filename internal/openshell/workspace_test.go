// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

package openshell

import (
	"context"
	"reflect"
	"testing"
)

func TestGatewayInWorkspace(t *testing.T) {
	c, log := fakeCLI(t, `echo '[]'`)
	g, _ := c.Gateway("brig-dev")
	team, err := g.In("team")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := team.Profiles(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := team.DeleteProvider(context.Background(), "x"); err != nil {
		t.Fatal(err)
	}
	want := "-g brig-dev --workspace team profile list -o json\n-g brig-dev --workspace team provider delete x\n"
	if got := readFile(t, log); got != want {
		t.Errorf("argv:\n%s\nwant:\n%s", got, want)
	}
	for _, name := range []string{"", "../x", "-w"} {
		if _, err := g.In(name); err == nil {
			t.Errorf("In(%q) succeeded", name)
		}
	}
}

func TestWorkspaces(t *testing.T) {
	for _, tc := range []struct{ name, out string }{
		{"envelope", `{"workspaces":[{"name":"default"},{"name":"team"}],"next_page_token":""}`},
	} {
		c, log := fakeCLI(t, "echo '"+tc.out+"'")
		g, _ := c.Gateway("brig-dev")
		got, err := g.Workspaces(context.Background())
		if err != nil || !reflect.DeepEqual(got, []string{"default", "team"}) {
			t.Errorf("%s: Workspaces() = %q, %v", tc.name, got, err)
		}
		if l := readFile(t, log); l != "-g brig-dev workspace list -o json\n" {
			t.Errorf("%s: argv %q", tc.name, l)
		}
	}
	c, log := fakeCLI(t, "")
	g, _ := c.Gateway("brig-dev")
	if err := g.CreateWorkspace(context.Background(), "team"); err != nil {
		t.Fatal(err)
	}
	if l := readFile(t, log); l != "-g brig-dev workspace create --name team\n" {
		t.Errorf("argv %q", l)
	}
	for _, name := range []string{"-x", "Team", "my_team", "a.b", "team-", "abcdefghijklmnopqrst"} {
		if err := g.CreateWorkspace(context.Background(), name); err == nil {
			t.Errorf("CreateWorkspace accepted %q", name)
		}
	}
	if err := g.CreateWorkspace(context.Background(), "abcdefghijklmnopqrs"); err != nil {
		t.Errorf("CreateWorkspace rejected a 19-character name: %v", err)
	}
}
