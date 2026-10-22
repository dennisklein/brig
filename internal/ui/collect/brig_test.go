// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

package collect

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/dennisklein/brig/internal/libvirt"
	"github.com/dennisklein/brig/internal/ocsync"
	"github.com/dennisklein/brig/internal/paths"
	"github.com/dennisklein/brig/internal/ui/store"
	"github.com/dennisklein/brig/internal/vm"
)

// fakeDomains answers State from a map, where a missing domain fails, and
// Stats with the samples in stats.
type fakeDomains struct {
	states map[string]libvirt.State
	stats  map[string]libvirt.Stats
	closed bool
}

func (f *fakeDomains) Stats([]string) (map[string]libvirt.Stats, error) {
	if f.stats == nil {
		return nil, errors.New("no stats")
	}
	return f.stats, nil
}

func (f *fakeDomains) State(domain string) (libvirt.State, error) {
	s, ok := f.states[domain]
	if !ok {
		return 0, errors.New("connection lost")
	}
	return s, nil
}

func (f *fakeDomains) Close() error { f.closed = true; return nil }

func testDirs(t *testing.T) paths.Dirs {
	t.Helper()
	d := t.TempDir()
	return paths.Dirs{Config: filepath.Join(d, "config"), Data: filepath.Join(d, "data"), Cache: filepath.Join(d, "cache")}
}

func writeImage(t *testing.T, dirs paths.Dirs, id, version string, created time.Time) {
	t.Helper()
	dir := filepath.Join(dirs.ImagesDir(), id)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(map[string]any{"id": id, "fedora_release": 44, "openshell_version": version, "created_at": created})
	if err := os.WriteFile(filepath.Join(dir, "image.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestBrigCollect(t *testing.T) {
	dirs := testDirs(t)
	const oldImg, newImg = "f44-openshell0.1.1-20260921T143000Z", "f44-openshell0.1.2-20261010T091500Z"
	writeImage(t, dirs, oldImg, "0.1.1", time.Now().Add(-48*time.Hour))
	writeImage(t, dirs, newImg, "0.1.2", time.Now())
	vms := vm.NewStore(dirs)
	for _, v := range []*vm.VM{
		{Name: "dev", Image: newImg, NetworkProfile: "default", CPUs: 4},
		{Name: "old", Image: oldImg, NetworkProfile: "default", CPUs: 2},
	} {
		if err := vms.Save(v); err != nil {
			t.Fatal(err)
		}
	}
	syncTime := time.Date(2026, 10, 12, 9, 0, 0, 0, time.UTC)
	st := &ocsync.State{LastSync: &ocsync.Record{Time: syncTime, Error: "provider x: no secret"}}
	if err := st.Save(filepath.Join(vms.Dir("old"), ocsync.StateFile)); err != nil {
		t.Fatal(err)
	}

	conn := &fakeDomains{states: map[string]libvirt.State{"brig-dev": libvirt.StateRunning, "brig-old": libvirt.StateShutoff}}
	b := &Brig{Dirs: dirs, Connect: func(context.Context) (Domains, error) { return conn, nil }}
	s := store.New()
	sub := s.Subscribe()
	defer sub.Close()

	next := b.Collect(context.Background(), s, nil)
	if next != conn {
		t.Error("the connection is not kept")
	}
	sn := s.Snapshot()
	if sn.VMsErr != "" || len(sn.VMs) != 2 {
		t.Fatalf("snapshot: %q, %d VMs", sn.VMsErr, len(sn.VMs))
	}
	dev, old := sn.VMs[0], sn.VMs[1]
	if dev.State != "running" || dev.OpenShell != "0.1.2" || dev.LastSync != nil {
		t.Errorf("dev: %+v", dev)
	}
	if old.State != "shut off" || old.OpenShell != "0.1.1" || old.LastSync == nil || !old.LastSync.Time.Equal(syncTime) || old.LastSync.Error != "provider x: no secret" {
		t.Errorf("old: %+v", old)
	}
	if sn.NewestImage == nil || sn.NewestImage.ID != newImg {
		t.Errorf("newest image: %+v", sn.NewestImage)
	}
	if len(sn.Images) != 2 || sn.Images[0].ID != newImg || !slices.Equal(sn.Images[0].UsedBy, []string{"dev"}) ||
		sn.Images[1].Fedora != 44 || !slices.Equal(sn.Images[1].UsedBy, []string{"old"}) {
		t.Errorf("images: %+v", sn.Images)
	}
	if sn.Config.Defaults.CPUs == 0 || sn.Config.Defaults.NetworkProfile == "" {
		t.Errorf("config defaults: %+v", sn.Config)
	}
	if _, ok := sn.Profiles["default"]; !ok {
		t.Error("no built-in profiles without a config file")
	}
	if got := sub.Take(); !slices.Equal(got, []store.Topic{store.TopicVMs}) {
		t.Errorf("notified %v", got)
	}

	// Nothing changed: nobody is told.
	b.Collect(context.Background(), s, conn)
	if got := sub.Take(); len(got) != 0 {
		t.Errorf("an unchanged poll notified %v", got)
	}

	// A lost connection makes states unknown, says why, and is dropped.
	delete(conn.states, "brig-old")
	if next := b.Collect(context.Background(), s, conn); next != nil || !conn.closed {
		t.Error("a failed connection is kept")
	}
	sn = s.Snapshot()
	if sn.VMs[1].State != "unknown" || !strings.Contains(sn.VMsErr, "connection lost") {
		t.Errorf("after a lost connection: %q, %q", sn.VMs[1].State, sn.VMsErr)
	}
}

func TestBrigCollectWithoutLibvirt(t *testing.T) {
	dirs := testDirs(t)
	if err := vm.NewStore(dirs).Save(&vm.VM{Name: "dev", Image: "gone", NetworkProfile: "default"}); err != nil {
		t.Fatal(err)
	}
	b := &Brig{Dirs: dirs, Connect: func(context.Context) (Domains, error) { return nil, errors.New("no session daemon") }}
	s := store.New()
	if conn := b.Collect(context.Background(), s, nil); conn != nil {
		t.Error("got a connection")
	}
	sn := s.Snapshot()
	if len(sn.VMs) != 1 || sn.VMs[0].State != "unknown" || !strings.Contains(sn.VMsErr, "try brig doctor") {
		t.Errorf("snapshot: %+v, %q", sn.VMs, sn.VMsErr)
	}
}

func TestBrigCollectNoVMsNeedsNoLibvirt(t *testing.T) {
	b := &Brig{Dirs: testDirs(t), Connect: func(context.Context) (Domains, error) {
		t.Error("connected without VMs")
		return nil, errors.New("unused")
	}}
	s := store.New()
	b.Collect(context.Background(), s, nil)
	if sn := s.Snapshot(); len(sn.VMs) != 0 || sn.VMsErr != "" {
		t.Errorf("snapshot: %+v", sn)
	}
}

func TestBrigCollectStats(t *testing.T) {
	dirs := testDirs(t)
	if err := vm.NewStore(dirs).Save(&vm.VM{Name: "dev", Image: "gone", NetworkProfile: "default", CPUs: 4}); err != nil {
		t.Fatal(err)
	}
	t0 := time.Now()
	conn := &fakeDomains{
		states: map[string]libvirt.State{"brig-dev": libvirt.StateRunning},
		stats: map[string]libvirt.Stats{"brig-dev": {
			SampledAt: t0, CPUTimeNS: 1e9,
			Memory: &libvirt.MemoryStats{UsedKiB: 1 << 20, TotalKiB: 4 << 20, Source: libvirt.MemoryFromHost},
		}},
	}
	b := &Brig{Dirs: dirs, Connect: func(context.Context) (Domains, error) { return conn, nil }}
	s := store.New()

	// One sample gives the memory share but no CPU share yet.
	b.Collect(context.Background(), s, nil)
	got := s.Snapshot().VMs[0].Stats
	if got == nil || got.CPU != nil || got.Memory == nil || *got.Memory != 25 || !got.MemoryFromHost {
		t.Fatalf("first sample: %+v", got)
	}

	// 2 s later, 3 s of CPU time on 4 vCPUs is 37.5%, shown as 38%.
	conn.stats = map[string]libvirt.Stats{"brig-dev": {
		SampledAt: t0.Add(2 * time.Second), CPUTimeNS: 4e9,
		Memory: &libvirt.MemoryStats{UsedKiB: 3 << 20, TotalKiB: 4 << 20, Source: libvirt.MemoryFromGuest},
	}}
	b.Collect(context.Background(), s, conn)
	got = s.Snapshot().VMs[0].Stats
	if got == nil || got.CPU == nil || *got.CPU != 38 || *got.Memory != 75 || got.MemoryFromHost {
		t.Fatalf("second sample: %+v", got)
	}

	// A restarted domain starts its CPU time again: no share from that.
	conn.stats["brig-dev"] = libvirt.Stats{SampledAt: t0.Add(4 * time.Second), CPUTimeNS: 1e8}
	b.Collect(context.Background(), s, conn)
	if got = s.Snapshot().VMs[0].Stats; got == nil || got.CPU != nil || got.Memory != nil {
		t.Errorf("after a restart: %+v", got)
	}

	// Failing to sample leaves the numbers unknown, with no message.
	conn.stats = nil
	b.Collect(context.Background(), s, conn)
	if sn := s.Snapshot(); sn.VMs[0].Stats != nil || sn.VMsErr != "" {
		t.Errorf("without stats: %+v, %q", sn.VMs[0].Stats, sn.VMsErr)
	}
}

// libvirt does not tell when a domain booted, so the collector notes the
// boots it sees.
func TestBrigCollectUp(t *testing.T) {
	dirs := testDirs(t)
	if err := vm.NewStore(dirs).Save(&vm.VM{Name: "dev", Image: "gone", NetworkProfile: "default"}); err != nil {
		t.Fatal(err)
	}
	conn := &fakeDomains{states: map[string]libvirt.State{"brig-dev": libvirt.StateRunning}}
	b := &Brig{Dirs: dirs, Connect: func(context.Context) (Domains, error) { return conn, nil }}
	s := store.New()
	up := func() time.Time {
		t.Helper()
		b.Collect(context.Background(), s, conn)
		return s.Snapshot().VMs[0].Up
	}
	if got := up(); !got.IsZero() {
		t.Errorf("a VM that ran before the first poll is up since %v", got)
	}
	conn.states["brig-dev"] = libvirt.StateShutoff
	if got := up(); !got.IsZero() {
		t.Errorf("a VM that is off is up since %v", got)
	}
	before := time.Now()
	conn.states["brig-dev"] = libvirt.StateRunning
	booted := up()
	if booted.Before(before) || booted.After(time.Now()) {
		t.Fatalf("a VM that booted between polls is up since %v", booted)
	}
	if got := up(); !got.Equal(booted) {
		t.Errorf("up moved from %v to %v", booted, got)
	}
}

func TestUsageCapsCPU(t *testing.T) {
	t0 := time.Now()
	got := usage(libvirt.Stats{SampledAt: t0}, libvirt.Stats{SampledAt: t0.Add(time.Second), CPUTimeNS: 3e9}, 2)
	if got.CPU == nil || *got.CPU != 100 {
		t.Errorf("CPU share %v, want 100", got.CPU)
	}
	if got = usage(libvirt.Stats{SampledAt: t0}, libvirt.Stats{SampledAt: t0.Add(time.Second), CPUTimeNS: 5e8}, 0); got.CPU == nil || *got.CPU != 50 {
		t.Errorf("a VM without a CPU count: %v, want 50", got.CPU)
	}
}
