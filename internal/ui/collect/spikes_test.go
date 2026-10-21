// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

package collect

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dennisklein/brig/internal/service"
	"github.com/dennisklein/brig/internal/vmrunner"
)

func TestReadSpikes(t *testing.T) {
	dirs := testDirs(t)
	store := service.NewStore(dirs)
	for _, svc := range []service.Service{
		{Name: "vms", Type: vmrunner.ServiceType, Placement: service.PlacementHost, Port: 40001},
		{Name: "down", Type: vmrunner.ServiceType, Placement: service.PlacementHost, Port: 40002},
		{Name: "llm", Type: "llm-proxy", Placement: service.PlacementVM, Port: 40003},
	} {
		if err := store.Save(&svc); err != nil {
			t.Fatal(err)
		}
	}
	// A UNIX socket's path is short; t.TempDir's may be too long.
	runtime, err := os.MkdirTemp("", "brig")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(runtime) })
	sock := vmrunner.AdminSocket(runtime, "vms")
	if err := os.MkdirAll(filepath.Dir(sock), 0o700); err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	t0 := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	vms := []vmrunner.AdminVM{
		{Record: vmrunner.Record{ID: "b04d77c1", Owner: vmrunner.Owner{Gateway: "dev", Workspace: "default", Sandbox: "b"}, Image: "u", CreatedAt: t0.Add(time.Minute)}, State: "shut off"},
		{Record: vmrunner.Record{ID: "3f9c1a2e", Owner: vmrunner.Owner{Gateway: "dev", Workspace: "default", Sandbox: "a"}, Image: "f", CreatedAt: t0}, State: "running"},
	}
	srv := &http.Server{ReadHeaderTimeout: time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/vms" {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(vms)
	})}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })

	spikes, err := ReadSpikes(context.Background(), dirs, runtime)
	if err == nil || !strings.Contains(err.Error(), "service down") || strings.Contains(err.Error(), "llm") {
		t.Errorf("error = %v, want one of service down only", err)
	}
	if len(spikes) != 2 || spikes[0].ID != "3f9c1a2e" || spikes[0].Service != "vms" || spikes[0].Sandbox != "dev/default/a" ||
		spikes[0].State != "running" || spikes[1].ID != "b04d77c1" {
		t.Errorf("spikes = %+v, want both VMs of vms, oldest first", spikes)
	}
}
