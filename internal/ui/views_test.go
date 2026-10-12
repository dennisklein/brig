// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

package ui

import (
	"testing"
	"time"

	"github.com/dennisklein/brig/internal/bytesize"
	"github.com/dennisklein/brig/internal/doctor"
	"github.com/dennisklein/brig/internal/ui/demo"
	"github.com/dennisklein/brig/internal/ui/store"
)

func TestNeeds(t *testing.T) {
	now := time.Now()
	newest := demo.Images(now)[0]
	sn := store.Snapshot{
		VMs: demo.VMs(now), NewestImage: &newest, VMsErr: "vm.json: broken",
		Doctor: &store.DoctorReport{Results: []doctor.Result{
			{Name: "kvm", Status: doctor.OK},
			{Name: "subids", Status: doctor.Warn, Detail: "few", Hint: "add more\nsecond line"},
			{Name: "passt", Status: doctor.Fail, Detail: "missing"},
		}},
	}
	var got []string
	for _, n := range needs(sn) {
		got = append(got, n.Sev+" "+n.Title+" | "+n.Detail)
	}
	want := []string{
		"bad brig cannot read everything about its VMs | vm.json: broken",
		"bad The last sync of review failed | " + sn.VMs[1].LastSync.Error,
		"bad Host check passt: missing | ",
		"warn Host check subids: few | add more",
		"info airgap runs an older base image | f44-openshell0.1.2-20261010T091500Z (OpenShell 0.1.2) is newer. brig upgrade airgap keeps the data disk.",
	}
	if len(got) != len(want) {
		t.Fatalf("got %q", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("item %d:\ngot  %q\nwant %q", i, got[i], want[i])
		}
	}
	if n := needs(store.Snapshot{}); len(n) != 0 {
		t.Errorf("an empty host needs %v", n)
	}
}

func TestSince(t *testing.T) {
	for d, want := range map[time.Duration]string{
		10 * time.Second:    "under a minute",
		7 * time.Minute:     "7 min",
		7 * time.Hour:       "7 h",
		47 * time.Hour:      "47 h",
		50 * time.Hour:      "2 d",
		21 * 24 * time.Hour: "21 d",
	} {
		if got := since(time.Now().Add(-d)); got != want {
			t.Errorf("%v: %q, want %q", d, got, want)
		}
	}
	if got := rel(time.Time{}); got != "never" {
		t.Errorf("zero time: %q", got)
	}
}

func TestSize(t *testing.T) {
	for s, want := range map[bytesize.Size]string{8 << 30: "8 GiB", 1536 << 20: "1536 MiB", 100: "100 B"} {
		if got := size(s); got != want {
			t.Errorf("%d: %q, want %q", s, got, want)
		}
	}
}

func TestPalette(t *testing.T) {
	sn := store.Snapshot{VMs: demo.VMs(time.Now())}
	if items := palette(sn, "REV"); len(items) == 0 || items[0].Link != "/vms/review" {
		t.Errorf("REV: %+v", items)
	}
	if items := palette(sn, "dev · mounts"); len(items) != 1 || items[0].Link != "/vms/dev?tab=mounts" {
		t.Errorf("tab: %+v", items)
	}
	if items := palette(sn, "zzz"); len(items) != 0 {
		t.Errorf("zzz: %+v", items)
	}
}
