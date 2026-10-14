// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"context"
	"strings"
	"testing"
)

func TestSnapshotAll(t *testing.T) {
	g := newFakeSnapGW()
	g.add("webapp", "Ready", "localhost/claude-agent:1", "w")
	g.add("api", "Stopped", "localhost/claude-agent:1", "a")
	g.add("broken", "Error", "localhost/claude-agent:1", "b")
	s, _, errw := engine(t, g)
	if err := s.all(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(g.snaps) != 2 || g.sandboxes["default/webapp"].Phase != "Ready" || g.sandboxes["default/api"].Phase != "Stopped" {
		t.Errorf("snapshots %v", g.snaps)
	}
	if !strings.Contains(errw.String(), "sandbox broken (workspace default) is error, so it gets no snapshot") {
		t.Errorf("printed %s", errw)
	}
}
