// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"strings"
	"testing"

	"github.com/dennisklein/brig/internal/help"
)

func TestHelpTopic(t *testing.T) {
	out, err := run(t, "help", "fence-2")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(out, "Fence 2, the network profile\n") || !strings.Contains(out, "brig help fence-1") {
		t.Errorf("brig help fence-2 = %q", out)
	}
	out, err = run(t, "help", "topics")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "unblock") || !strings.Contains(out, "fence-2") {
		t.Errorf("brig help topics = %q", out)
	}
	out, err = run(t, "--help")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "fence-2") || !strings.Contains(out, "topics") {
		t.Errorf("brig --help should list topics but no single topic:\n%s", out)
	}
}

// TestHelpCommandsExist checks that the brig commands in the help texts are
// commands that brig has, with flags that they take.
func TestHelpCommandsExist(t *testing.T) {
	h, err := help.Load()
	if err != nil {
		t.Fatal(err)
	}
	root := newRootCmd(&progressRun{})
	var lines []string
	for _, k := range []help.Kind{help.Concept, help.Task} {
		for _, tp := range h.Topics(k) {
			if c, _, err := root.Find([]string{tp.ID}); err == nil && c != root && !c.Hidden {
				t.Errorf("topic %s is named like the command brig %s, so brig help %s cannot show it", tp.ID, c.Name(), tp.ID)
			}
			lines = append(lines, tp.Commands...)
		}
	}
	for _, id := range h.ActionIDs() {
		a, _ := h.Action(id)
		lines = append(lines, a.Command)
	}
	for _, line := range lines {
		words := strings.Fields(line)
		if len(words) < 2 || words[0] != "brig" {
			continue
		}
		c, args, err := root.Find(words[1:])
		if err != nil || c == root {
			t.Errorf("%q: no such command", line)
			continue
		}
		for _, a := range args {
			if a == "--" {
				break
			}
			name, _, _ := strings.Cut(strings.TrimLeft(a, "-"), "=")
			switch {
			case strings.HasPrefix(a, "--"):
				if c.Flags().Lookup(name) == nil {
					t.Errorf("%q: brig %s has no flag --%s", line, c.Name(), name)
				}
			case strings.HasPrefix(a, "-") && len(a) == 2:
				if c.Flags().ShorthandLookup(name) == nil {
					t.Errorf("%q: brig %s has no flag -%s", line, c.Name(), name)
				}
			}
		}
	}
}
