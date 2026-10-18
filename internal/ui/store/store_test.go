// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"testing"
	"time"
)

func TestSubscribe(t *testing.T) {
	s := New()
	sub := s.Subscribe()
	s.SetHost(Host{Name: "zeta"})
	s.SetActivity([]Run{{ID: "a"}})
	select {
	case <-sub.Changed():
	default:
		t.Fatal("no wake after a change")
	}
	if got := sub.Take(); len(got) != 2 || !got[TopicHost] || !got[TopicActivity] {
		t.Errorf("topics %v", got)
	}
	if got := sub.Take(); len(got) != 0 {
		t.Errorf("topics again: %v", got)
	}
	if sn := s.Snapshot(); sn.Host.Name != "zeta" || len(sn.Activity) != 1 {
		t.Errorf("snapshot %+v", sn)
	}
	sub.Close()
	s.SetHost(Host{})
	select {
	case <-sub.Changed():
		t.Error("woken after closing")
	default:
	}
}

// A subscriber that does not read never blocks the writers, and loses no
// topic: the one change of a kind it has not taken yet stays pending, however
// many others follow.
func TestSlowSubscriber(t *testing.T) {
	s := New()
	sub := s.Subscribe()
	defer sub.Close()
	done := make(chan struct{})
	go func() {
		s.SetVMs(VMState{VMs: []VM{{State: "running"}}})
		for i := range 100 {
			s.SetActivity(make([]Run, i))
		}
		for i := range 100 {
			s.SetHost(Host{Name: string(rune('a' + i%26))})
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
	got := sub.Take()
	if !got[TopicVMs] || !got[TopicActivity] || !got[TopicHost] {
		t.Errorf("topics %v, want vms, activity and host", got)
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

func TestTasks(t *testing.T) {
	s := New()
	sub := s.Subscribe()
	defer sub.Close()
	s.AddTask(Task{ID: "t1", Status: TaskRunning})
	s.AddTask(Task{ID: "t2", Status: TaskRunning})
	before := s.Snapshot()
	end := time.Now()
	s.UpdateTask("t1", TopicTasks, func(t *Task) { t.End, t.Status = &end, TaskOK })
	s.UpdateTask("t2", TopicOutput, func(t *Task) { t.Output = "hello" })
	s.UpdateTask("gone", TopicTasks, func(*Task) { t.Error("updated a task that is not there") })
	sn := s.Snapshot()
	if t1, _ := sn.Task("t1"); !t1.Done() || t1.Status != TaskOK {
		t.Errorf("t1: %+v", t1)
	}
	if t2, _ := sn.Task("t2"); t2.Output != "hello" || t2.Done() {
		t.Errorf("t2: %+v", t2)
	}
	// A snapshot is never changed once published.
	if t1, _ := before.Task("t1"); t1.Done() || t1.Status != TaskRunning {
		t.Errorf("an earlier snapshot changed: %+v", t1)
	}
	if got := sub.Take(); len(got) != 2 || !got[TopicTasks] || !got[TopicOutput] {
		t.Errorf("topics %v, want tasks and output", got)
	}
	s.RemoveTask("t1")
	if _, ok := s.Snapshot().Task("t1"); ok || len(s.Snapshot().Tasks) != 1 {
		t.Errorf("tasks after removing t1: %+v", s.Snapshot().Tasks)
	}
}
