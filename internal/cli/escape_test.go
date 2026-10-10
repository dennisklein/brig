// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dennisklein/brig/internal/ocsync"
)

// title is a terminal title sequence, which a hostile text may hold.
const title = "\x1b]0;pwned\x07"

func TestShowEscapesUntrustedText(t *testing.T) {
	a := testApp(t)
	v := testVM(t, a, "dev")
	v.OpenShellConfigs = []string{"/cfg/" + title + "dir"}
	if err := a.vms.Save(v); err != nil {
		t.Fatal(err)
	}
	st := &ocsync.State{LastSync: &ocsync.Record{Time: time.Now().UTC(), Error: "failed " + title}}
	if err := st.Save(a.vmFile(v, syncStateFile)); err != nil {
		t.Fatal(err)
	}

	out, err := run(t, "show", "dev")
	if err != nil {
		t.Fatal(err)
	}
	if strings.ContainsAny(out, "\x1b\x07") {
		t.Errorf("brig show wrote a control character:\n%q", out)
	}
	for _, want := range []string{`\x1b]0;pwned`, "/cfg/", "Last sync", "failed"} {
		if !strings.Contains(out, want) {
			t.Errorf("brig show lacks %q:\n%s", want, out)
		}
	}
}

func TestImageListEscapesUntrustedText(t *testing.T) {
	a := testApp(t)
	id := "f44-openshell0.1.2-20261007T120000Z"
	dir := filepath.Dir(a.images.Path(id))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	rec := `{"id":"` + id + `","fedora_release":44,"openshell_version":"0.1.2\u001b]0;pwned\u0007","created_at":"2026-10-07T12:00:00Z"}`
	if err := os.WriteFile(filepath.Join(dir, "image.json"), []byte(rec), 0o600); err != nil {
		t.Fatal(err)
	}

	out, err := run(t, "image", "list")
	if err != nil {
		t.Fatal(err)
	}
	if strings.ContainsAny(out, "\x1b\x07") || !strings.Contains(out, `\x1b]0;pwned`) {
		t.Errorf("brig image list:\n%q", out)
	}
}
