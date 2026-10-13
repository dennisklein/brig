// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

package ui

import (
	"io/fs"
	"net/http"
	"regexp"
	"strings"
	"testing"

	"github.com/dennisklein/brig/internal/help"
	"github.com/dennisklein/brig/internal/ui/store"
)

// TestHelpCoversTheUI checks that every page and every action has help, and
// that every concept the templates mark exists.
func TestHelpCoversTheUI(t *testing.T) {
	h, err := help.Load()
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range pageNames {
		if _, ok := h.Page(name); !ok {
			t.Errorf("page %s has no help in help/pages.yaml", name)
		}
	}
	for id := range dialogSpecs {
		if _, ok := h.Action(id); !ok {
			t.Errorf("action %s has no help in help/actions.yaml", id)
		}
	}
	for _, id := range h.ActionIDs() {
		if _, ok := dialogSpecs[id]; !ok {
			t.Errorf("help/actions.yaml has help for %s, which is no action", id)
		}
	}
	termRE := regexp.MustCompile(`\bterm "([^"]+)"`)
	err = fs.WalkDir(web, "web/templates", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		data, err := fs.ReadFile(web, p)
		if err != nil {
			return err
		}
		for _, m := range termRE.FindAllStringSubmatch(string(data), -1) {
			if tp, ok := h.Topic(m[1]); !ok || tp.Kind != help.Concept {
				t.Errorf("%s marks %q, which is no concept", p, m[1])
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestHelpPane(t *testing.T) {
	ts := newTestServer(t)
	ts.login()
	for target, want := range map[string][]string{
		"/help?page=vm&tab=mounts":   {"<h2>VM</h2>", `data-chip="mount"`, "/help/task/share-dir"},
		"/help?page=nope":            {"<h2>Overview</h2>"},
		"/help/concept/fence-2":      {"Fence 2, the network profile", "brig update dev --profile default", "brig help fence-2"},
		"/help/task/unblock":         {`<ol class="hp-steps">`, `href="/traffic?verdict=deny#show-allow"`},
		"/help/action/delete":        {"Delete VM", "brig delete VM", `class="callout bad"`},
		"/help/hint/concept/fence-2": {"Fence 2, the network profile", "outside the VM"},
		"/help/hint/action/stop":     {"undo: Start", "brig stop VM"},
		"/help/search?q=token":       {"/help/concept/provider", "/help/task/rotate-token"},
		"/help/search?q=zzzz":        {"Nothing in help matches"},
		"/help/all":                  {"/help/task/unblock", `data-chip="vm"`},
	} {
		w := ts.get(target)
		if w.Code != http.StatusOK {
			t.Errorf("%s: %d", target, w.Code)
			continue
		}
		for _, s := range want {
			if !strings.Contains(w.Body.String(), s) {
				t.Errorf("%s lacks %q:\n%s", target, s, w.Body.String())
			}
		}
	}
	for _, target := range []string{"/help/concept/unblock", "/help/task/nope", "/help/action/nope", "/help/other/vm", "/help/hint/concept/nope"} {
		if w := ts.get(target); w.Code != http.StatusNotFound {
			t.Errorf("%s: %d, want 404", target, w.Code)
		}
	}
}

func TestPagesMarkHelp(t *testing.T) {
	ts := newTestServer(t)
	ts.login()
	for target, want := range map[string][]string{
		"/":                          {`data-help-page="overview"`, `data-term="needs-you"`, `data-help="create"`, `id="helppane"`},
		"/vms/dev?tab=network":       {`data-help-page="vm" data-help-tab="network"`, `data-term="fence-2"`, `data-help="profile"`},
		"/sandboxes/dev/team/webapp": {`data-help-page="sandbox"`, `data-term="placeholder"`},
		"/a/delete?vm=dev":           {`data-help-open="/help/action/delete"`},
	} {
		body := ts.get(target).Body.String()
		for _, s := range want {
			if !strings.Contains(body, s) {
				t.Errorf("%s lacks %s", target, s)
			}
		}
	}
}

func TestWelcome(t *testing.T) {
	ts := newTestServer(t)
	ts.login()
	if body := ts.get("/").Body.String(); strings.Contains(body, "Welcome to brig") {
		t.Error("a host with VMs gets the welcome")
	}
	ts.st.SetVMs(store.VMState{})
	body := ts.get("/").Body.String()
	for _, s := range []string{"Welcome to brig", `href="/host"`, `data-help="create"`, `data-term="gateway"`} {
		if !strings.Contains(body, s) {
			t.Errorf("the overview without VMs lacks %s", s)
		}
	}
}

func TestHelpPalette(t *testing.T) {
	h, err := help.Load()
	if err != nil {
		t.Fatal(err)
	}
	sn := store.Snapshot{}
	links := func(items []paletteItem) string {
		var s []string
		for _, it := range items {
			s = append(s, it.Link)
		}
		return strings.Join(s, " ")
	}
	if got := links(helpPalette(h, sn, "?fence 2")); !strings.HasPrefix(got, "/help/concept/fence-2") {
		t.Errorf("?fence 2: %s", got)
	}
	if got := links(helpPalette(h, sn, "image")); !strings.HasPrefix(got, "/images ") || !strings.Contains(got, "/help/concept/base-image") {
		t.Errorf("image: pages first, then help: %s", got)
	}
	if got := helpPalette(h, sn, "?"); len(got) != len(h.Topics(help.Task))+len(h.Topics(help.Concept)) {
		t.Errorf("?: %d entries", len(got))
	}
	if got := links(helpPalette(h, sn, "")); strings.Contains(got, "/help/") {
		t.Errorf("an empty query lists help: %s", got)
	}
}
