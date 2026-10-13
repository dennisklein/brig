// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/dennisklein/brig/internal/audit"
	"github.com/dennisklein/brig/internal/libvirt"
)

// auditFixture gives VM dev and the deleted VM old an audit trail each.
func auditFixture(t *testing.T) *app {
	t.Helper()
	a := testApp(t)
	stateHome(t)
	a.dirs.State = os.Getenv("XDG_STATE_HOME") + "/brig"
	t.Setenv(gatewayEnv, "")
	t.Setenv(workspaceEnv, "")
	testVM(t, a, "dev")
	now := time.Now()
	for _, vm := range []string{"dev", "old"} {
		w, err := a.auditStore().OpenWriter(vm)
		if err != nil {
			t.Fatal(err)
		}
		w.Now = func() time.Time { return now.Add(-2 * time.Hour) }
		w.Add(
			audit.Record{
				T: now.Add(-2 * time.Hour), Workspace: "default", Sandbox: "webapp", Kind: audit.KindPolicy, Event: audit.EventRevision,
				Version: 2, Status: "loaded", Added: []string{"pypi"}, By: audit.ByPolicyAllow,
			},
			audit.Record{
				T: now.Add(-time.Hour), Workspace: "default", Sandbox: "webapp", Kind: audit.KindRequest, Verdict: audit.VerdictAllow,
				Host: "pypi.org", Port: 443, Binary: "/usr/bin/pip", Rule: "pypi",
			},
			audit.Record{
				T: now.Add(-time.Hour), Workspace: "team", Sandbox: "api", Kind: audit.KindRequest, Verdict: audit.VerdictDeny,
				Host: "evil.example", Port: 443, Binary: "/usr/bin/curl", Reason: "no rule",
			},
		)
		if err := w.Close(nil); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := a.auditStore().MarkDeleted("old", now.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	return a
}

func TestAuditTimeline(t *testing.T) {
	auditFixture(t)
	out, _, _, err := runSplit(t, "audit", "dev")
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 3 || !strings.Contains(lines[0], "webapp") || !strings.Contains(lines[0], "policy v2 (loaded) +pypi; by brig policy allow") ||
		!strings.Contains(lines[2], "team/api") || !strings.Contains(lines[2], "deny evil.example:443 by /usr/bin/curl: no rule") {
		t.Errorf("timeline:\n%s", out)
	}

	// Every VM's, so each line names its VM.
	out, _, _, err = runSplit(t, "audit", "--denied", "--host", "example")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(out, "\n") != 2 || !strings.Contains(out, "dev team/api") || !strings.Contains(out, "old team/api") {
		t.Errorf("denials of every VM:\n%s", out)
	}

	out, _, _, err = runSplit(t, "audit", "old", "webapp", "-o", "json")
	if err != nil {
		t.Fatal(err)
	}
	var recs []audit.Record
	for _, l := range strings.Split(strings.TrimSpace(out), "\n") {
		var r audit.Record
		if err := json.Unmarshal([]byte(l), &r); err != nil {
			t.Fatalf("%v: %s", err, l)
		}
		recs = append(recs, r)
	}
	if len(recs) != 2 || recs[0].VM != "old" || recs[1].Host != "pypi.org" {
		t.Errorf("JSON records: %+v", recs)
	}

	_, stderr, _, err := runSplit(t, "audit", "dev", "--since", "30m")
	if err != nil || !strings.Contains(stderr, "No records match.") {
		t.Errorf("no records: %q, %v", stderr, err)
	}
	if _, _, _, err := runSplit(t, "audit", "nope"); err == nil || !strings.Contains(err.Error(), "no VM nope") {
		t.Errorf("unknown VM: %v", err)
	}
	if _, _, _, err := runSplit(t, "audit", "dev", "--kind", "traffic"); err == nil {
		t.Error("unknown kind accepted")
	}
}

func TestAuditSummary(t *testing.T) {
	auditFixture(t)
	t.Setenv(gatewayEnv, "brig-dev")
	out, stderr, _, err := runSplit(t, "audit", "webapp", "--summary")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stderr, "Using VM dev") || !strings.Contains(out, "pypi.org:443") || !strings.Contains(out, "pypi (brig policy allow)") ||
		strings.Contains(out, "evil") {
		t.Errorf("summary:\n%s%s", out, stderr)
	}
}

func TestAuditStatus(t *testing.T) {
	auditFixture(t)
	out, _, _, err := runSplit(t, "audit", "status")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "VM deleted") || !strings.Contains(out, "dev ") || !strings.Contains(out, "Total ") {
		t.Errorf("status:\n%s", out)
	}
	out, _, _, err = runSplit(t, "audit", "status", "dev", "-o", "json")
	if err != nil {
		t.Fatal(err)
	}
	var got auditStatusJSON
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.VMs) != 1 || got.VMs[0].Rows != 3 || got.Breakdown == nil || len(got.Breakdown.Sandboxes) != 2 || got.Disk == 0 {
		t.Errorf("status of dev: %s", out)
	}
}

func TestAuditPrune(t *testing.T) {
	a := auditFixture(t)
	if _, _, _, err := runSplit(t, "audit", "prune"); err == nil {
		t.Error("prune without a selection")
	}
	_, stderr, _, err := runSplit(t, "audit", "prune", "--deleted-vms", "--dry-run")
	if err != nil || !strings.Contains(stderr, "VM old: the whole history, 3 records") {
		t.Fatalf("dry run: %q, %v", stderr, err)
	}
	if vms, _ := a.auditStore().VMs(); len(vms) != 2 {
		t.Fatalf("dry run deleted: %v", vms)
	}
	out, _, _, err := runSplit(t, "audit", "prune", "--deleted-vms")
	if err != nil || !strings.Contains(out, "Deleted 3 records") {
		t.Fatalf("prune: %q, %v", out, err)
	}
	if vms, _ := a.auditStore().VMs(); len(vms) != 1 || vms[0] != "dev" {
		t.Errorf("left: %v", vms)
	}
	_, stderr, _, err = runSplit(t, "audit", "prune", "dev", "--sandbox", "api", "-w", "team", "--yes")
	if err != nil || !strings.Contains(stderr, "the records of sandbox team/api, 1 records") {
		t.Fatalf("prune a sandbox: %q, %v", stderr, err)
	}
	if out, _, _, _ := runSplit(t, "audit", "dev"); strings.Contains(out, "evil") {
		t.Errorf("sandbox not pruned:\n%s", out)
	}
}

func TestParseWhen(t *testing.T) {
	now := time.Date(2026, 10, 13, 12, 0, 0, 0, time.Local)
	for in, want := range map[string]time.Time{
		"24h":                  now.Add(-24 * time.Hour),
		"7d":                   now.Add(-7 * 24 * time.Hour),
		"2026-10-01":           time.Date(2026, 10, 1, 0, 0, 0, 0, time.Local),
		"2026-10-01 08:30":     time.Date(2026, 10, 1, 8, 30, 0, 0, time.Local),
		"2026-10-01T08:30:00Z": time.Date(2026, 10, 1, 8, 30, 0, 0, time.UTC),
		"all":                  {},
	} {
		if got, err := parseWhen(in, now); err != nil || !got.Equal(want) {
			t.Errorf("parseWhen(%q) = %v, %v; want %v", in, got, err, want)
		}
	}
	if _, err := parseWhen("yesterday", now); err == nil {
		t.Error("yesterday accepted")
	}
}

func TestStopStopsTheCollector(t *testing.T) {
	a := testApp(t)
	v := testVM(t, a, "dev")
	var stopped []string
	orig := stopAudit
	stopAudit = func(_ context.Context, vm string) { stopped = append(stopped, vm) }
	t.Cleanup(func() { stopAudit = orig })
	if err := a.stop(context.Background(), &fakeDomains{state: libvirt.StateRunning}, v, false); err != nil {
		t.Fatal(err)
	}
	if len(stopped) != 1 || stopped[0] != "dev" {
		t.Errorf("stopped collectors %v", stopped)
	}
}
