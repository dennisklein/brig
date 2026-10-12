// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"testing"
	"time"
)

func TestSubscribe(t *testing.T) {
	s := New()
	ch, unsubscribe := s.Subscribe()
	s.SetHost(Host{Name: "zeta"})
	s.SetActivity([]Run{{ID: "a"}})
	if got := <-ch; got != TopicHost {
		t.Errorf("first notice %q", got)
	}
	if got := <-ch; got != TopicActivity {
		t.Errorf("second notice %q", got)
	}
	if sn := s.Snapshot(); sn.Host.Name != "zeta" || len(sn.Activity) != 1 {
		t.Errorf("snapshot %+v", sn)
	}
	unsubscribe()
	s.SetHost(Host{})
	select {
	case got := <-ch:
		t.Errorf("notified %q after unsubscribing", got)
	default:
	}
}

// A subscriber that does not read misses notices, but never blocks the
// writers, and the snapshot it reads has every change.
func TestSlowSubscriber(t *testing.T) {
	s := New()
	_, unsubscribe := s.Subscribe()
	defer unsubscribe()
	done := make(chan struct{})
	go func() {
		for i := range 100 {
			s.SetVMs([]VM{{State: "running"}}, "", nil, nil)
			s.SetActivity(make([]Run, i))
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("a slow subscriber blocked the writers")
	}
	if n := len(s.Snapshot().Activity); n != 99 {
		t.Errorf("activity has %d runs, want 99", n)
	}
}

func TestSnapshotVM(t *testing.T) {
	var sn Snapshot
	sn.VMs = []VM{{State: "running"}}
	sn.VMs[0].Name = "dev"
	if v, ok := sn.VM("dev"); !ok || !v.Running() || v.GatewayName() != "brig-dev" || v.DomainName() != "brig-dev" {
		t.Errorf("dev: %+v, %v", v, ok)
	}
	if _, ok := sn.VM("nope"); ok {
		t.Error("found a VM that is not there")
	}
}
