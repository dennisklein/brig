// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

package ui

import (
	"net/http"
	"strings"
	"testing"
)

func TestAuditPages(t *testing.T) {
	ts := newTestServer(t)
	ts.login()
	for target, want := range map[string][]string{
		"/host": {
			"Audit trail", "~/.local/state/brig/audit", `hx-get="/a/audit-prune?vm=dev"`, "deleted ", "stopped (VM stopped)",
			`sse-swap="host-audit"`, "the VM itself",
		},
		"/traffic?vm=dev&range=7d": {
			`class="on" href="/traffic?range=7d&amp;vm=dev"`, "paste.example.net:443", "DENY", "From the audit trail",
			// A deleted sandbox can still be picked.
			`href="/traffic?range=7d&amp;sandbox=spike&amp;vm=dev&amp;workspace=default"`,
		},
		"/traffic?vm=dev&workspace=default&sandbox=spike": {
			// Picking a deleted sandbox shows all of its history.
			"spike", "Last hour",
		},
		"/traffic?vm=dev&workspace=default&sandbox=spike&range=all": {
			"pypi.org:443/simple/requests/", `hx-get="/sandboxes/dev/default/spike/history"`,
		},
		"/sandboxes/dev/default/webapp?tab=history": {`hx-get="/sandboxes/dev/default/webapp/history"`},
		"/sandboxes/dev/default/webapp/history": {
			"policy v4 (loaded)", "by draft c-7f3a approved", "by brig policy allow", "by unknown (openshell CLI or another client)",
			"brig audit dev webapp -w default --since all",
		},
		"/sandboxes/dev/default/spike/history": {"paste.example.net:443", "/usr/bin/curl", "created", "deleted"},
		"/a/audit-prune?vm=scratch":            {`name="what" value="deleted" checked`, "Prune the audit history of scratch"},
		"/a/audit-prune?vm=dev":                {`name="what" value="before" checked`, `<option value="7d" selected>`},
	} {
		w := ts.get(target)
		if w.Code != http.StatusOK {
			t.Errorf("%s: %d", target, w.Code)
			continue
		}
		body := w.Body.String()
		for _, s := range want {
			if !strings.Contains(body, s) {
				t.Errorf("%s lacks %q", target, s)
			}
		}
	}
	// History rows have no Allow…: the dialog finds a request by its place
	// in the live log.
	if body := ts.get("/traffic?vm=dev&range=all").Body.String(); strings.Contains(body, "/a/allow?") {
		t.Error("a request from the audit trail offers Allow…")
	}
	// The dev VM's live range follows the gateways; history does not.
	if body := ts.get("/traffic?vm=dev&range=24h").Body.String(); strings.Contains(body, `sse-swap="traffic-table"`) {
		t.Error("a range of the audit trail updates live")
	}
	if w := ts.get("/sandboxes/Bad..VM/default/x/history"); w.Code != http.StatusNotFound {
		t.Errorf("bad history path: %d", w.Code)
	}
	// A range brig ui does not know is the live hour.
	if body := ts.get("/traffic?range=1y").Body.String(); !strings.Contains(body, `sse-swap="traffic-table"`) {
		t.Error("an unknown range was not the live one")
	}
}
