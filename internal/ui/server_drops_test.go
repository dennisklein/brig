// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

package ui

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/dennisklein/brig/internal/config"
	"github.com/dennisklein/brig/internal/fence2"
	"github.com/dennisklein/brig/internal/ui/store"
)

// addDrops adds to the test server what fence 2 dropped on dev: the
// packets of an allowed request to the NAS, and a flood it only counted.
func addDrops(ts *testServer) {
	now := time.Now()
	ts.st.AddRequests("dev", []store.Request{
		{Workspace: "default", Sandbox: "webapp", Time: now.Add(-90 * time.Second), Verdict: "allow", Process: "/usr/bin/smbclient", Host: "nas.lan", Port: 445, Policy: "nas_share"},
	})
	ts.st.AddDrops("dev", []store.Drop{
		{Time: now.Add(-89 * time.Second), Last: now.Add(-87 * time.Second), Reason: fence2.ReasonLAN, Proto: "tcp", Addr: "192.168.1.20", Port: 445, Hits: 3},
		{Time: now.Add(-30 * time.Second), Reason: fence2.ReasonUDPFlows, Hits: 18234, Why: fence2.WhyOverRate},
	})
}

func TestTrafficDrops(t *testing.T) {
	ts := newTestServer(t)
	ts.login()
	addTraffic(ts)
	addDrops(ts)
	for target, want := range map[string][]string{
		"/traffic": {
			"DROP", `<span class="chip f2"`, "tcp 192.168.1.20:445 ×3", "lan is off", "×18234 packets",
			"too many UDP flows at once <span class=\"faint\">(over the logging rate, not itemised)</span>",
			"likely dropped by the VM's network profile (lan)",
			`hx-get="/a/profile?dest=192.168.1.20%3A445&amp;port=445&amp;reason=lan&amp;vm=dev"`,
		},
		// Denied includes drops.
		"/traffic?verdict=deny&vm=dev": {"tcp 192.168.1.20:445 ×3"},
		"/sandboxes/dev/default/webapp?tab=traffic": {
			"likely dropped by the VM's network profile (lan)", `href="/traffic?verdict=deny&amp;vm=dev"`,
		},
		// The history: drops and the allowed request they likely stopped.
		"/traffic?vm=dev&range=all": {"tcp 192.168.1.20:445 ×3", "likely dropped by the VM's network profile (lan)", "port not in host_ports"},
		"/a/profile?vm=dev&reason=lan&dest=192.168.1.20:445&port=445": {
			"Fence 2 dropped 192.168.1.20:445 from dev: lan is off. A profile with lan: true lets it through, from the next start of dev.",
			`<option value="open" selected>`, `name="reason" value="lan"`, `name="port" value="445"`,
		},
		"/a/profile?vm=dev&reason=host-port&dest=10.0.2.2:11434&port=11434": {
			"port not in host_ports. A profile with host_ports: [11434] lets it through", `<option value="ollama" selected>`,
		},
		"/a/profile?vm=dev": {`<option value="ollama" selected>`},
		"/host":             {"Fence 2", "itemised", "counts only", "cannot read the drops the firewall logs"},
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
	// Drops are the VM's: a sandbox's table leaves them out, and so does
	// a table of a VM without them.
	for _, target := range []string{"/traffic?vm=dev&workspace=default&sandbox=webapp", "/traffic?vm=review", "/traffic?vm=dev&workspace=default&sandbox=spike&range=all"} {
		if body := ts.get(target).Body.String(); strings.Contains(body, ">DROP<") {
			t.Errorf("%s shows drops", target)
		}
	}
	// The dialog shows the command it runs.
	if body := ts.post("/a/profile/preview", values("vm", "dev", "profile", "open", "reason", "lan")).Body.String(); !strings.Contains(body, "brig update dev --profile open") {
		t.Errorf("preview: %s", body)
	}
	// A setting that no profile has yet.
	ts.st.SetVMs(store.VMState{VMs: ts.st.Snapshot().VMs, Profiles: map[string]config.NetworkProfile{"ollama": {Internet: true}}})
	if body := ts.get("/a/profile?vm=dev&reason=lan&dest=192.168.1.20:445&port=445").Body.String(); !strings.Contains(body, "No profile in config.yaml has lan: true yet; add one under network_profiles first.") {
		t.Errorf("profile dialog without a profile that lets the drop through: %s", body)
	}
	// A flood that no profile setting lets through has no Profile….
	if body := ts.get("/traffic?vm=dev").Body.String(); strings.Count(body, "/a/profile?") != 2 {
		t.Errorf("Profile… buttons: %d", strings.Count(body, "/a/profile?"))
	}
}
