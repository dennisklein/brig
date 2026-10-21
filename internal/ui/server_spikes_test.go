// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

package ui

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/dennisklein/brig/internal/ui/demo"
)

func TestSpikes(t *testing.T) {
	ts := newTestServer(t)
	ts.login()
	if body := ts.get("/vms").Body.String(); strings.Contains(body, `href="/spikes"`) {
		t.Error("the rail links to Spike VMs while there are none")
	}
	ts.st.SetSpikes(demo.Spikes(time.Now()), "")
	var asked []string
	ts.srv.spikeScreen = func(_ context.Context, service, id string) ([]byte, error) {
		asked = append(asked, service+"/"+id)
		if id == "b04d77c1" {
			return nil, errors.New("domain is not running")
		}
		return demo.SpikeScreen(context.Background(), service, id)
	}
	w := ts.get("/spikes")
	if w.Code != http.StatusOK {
		t.Fatalf("/spikes: %d", w.Code)
	}
	for _, want := range []string{"3f9c1a2e", "b04d77c1", "dev/default/webapp", "fedora-44-desktop", `href="/spikes"`} {
		if !strings.Contains(w.Body.String(), want) {
			t.Errorf("/spikes lacks %q", want)
		}
	}
	w = ts.get("/spikes/vms/3f9c1a2e/screen")
	if w.Code != http.StatusOK || w.Header().Get("Content-Type") != "image/png" || !bytes.HasPrefix(w.Body.Bytes(), []byte("\x89PNG")) {
		t.Errorf("screen: %d %q", w.Code, w.Header().Get("Content-Type"))
	}
	if w := ts.get("/spikes/vms/b04d77c1/screen"); w.Code != http.StatusBadGateway {
		t.Errorf("screen of a VM that is shut off: %d, want 502", w.Code)
	}
	for _, target := range []string{"/spikes/vms/00000000/screen", "/spikes/other/3f9c1a2e/screen"} {
		if w := ts.get(target); w.Code != http.StatusNotFound {
			t.Errorf("%s: %d, want 404", target, w.Code)
		}
	}
	if len(asked) != 2 {
		t.Errorf("asked the vm runner for %v, want only the known VMs", asked)
	}
}
