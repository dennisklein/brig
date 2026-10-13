// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"encoding/json"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/dennisklein/brig/internal/ui/store"
)

func TestSandboxClean(t *testing.T) {
	a := testApp(t)
	testVM(t, a, "dev")
	g := &fakeRunGateway{}
	fakeRun(t, g)
	past, future := strconv.FormatInt(time.Now().Add(-time.Minute).Unix(), 10), strconv.FormatInt(time.Now().Add(time.Hour).Unix(), 10)
	fakeSandboxes(t, map[string][]store.Sandbox{"dev": {
		{Workspace: "default", Name: "old", Labels: map[string]string{runLabel: "a", expiresLabel: past}},
		{Workspace: "default", Name: "live", Labels: map[string]string{runLabel: "b", expiresLabel: future}},
		{Workspace: "default", Name: "mine", Labels: map[string]string{expiresLabel: past}},
		{Workspace: "default", Name: "odd", Labels: map[string]string{runLabel: "c", expiresLabel: "soon"}},
		{Workspace: "team", Name: "old", Labels: map[string]string{runLabel: "d", expiresLabel: past}},
	}})
	if _, err := run(t, "sandbox", "clean", "dev"); err == nil || !strings.Contains(err.Error(), "--runs") {
		t.Errorf("clean without --runs: %v", err)
	}
	out, err := run(t, "sandbox", "clean", "dev", "--runs", "-w", "team")
	if err != nil || !slices.Equal(g.deleted, []string{"team/old"}) || !strings.Contains(out, "Deleted sandbox old of VM dev (workspace team).") {
		t.Errorf("clean -w team: %v, deleted %q: %s", err, g.deleted, out)
	}
	g.deleted = nil
	out, err = run(t, "sandbox", "clean", "dev", "--runs", "-o", "json")
	if err != nil || !slices.Equal(g.deleted, []string{"default/old", "team/old"}) {
		t.Errorf("clean: %v, deleted %q", err, g.deleted)
	}
	if i := strings.Index(out, "["); i < 0 || !json.Valid([]byte(out[i:])) {
		t.Errorf("clean -o json printed %q", out)
	}
}
