// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

// Package store holds what brig ui shows, in memory: the collectors write it,
// the pages and the live updates read it.
//
// A Snapshot is never changed once it is published. Each Set replaces the
// part it sets with a new value and notifies the subscribers of that part's
// topic, so a reader may keep a Snapshot as long as it likes.
package store

import (
	"slices"
	"sync"
	"time"

	"github.com/dennisklein/brig/internal/config"
	"github.com/dennisklein/brig/internal/doctor"
	"github.com/dennisklein/brig/internal/vm"
)

// Topic names a part of the snapshot that changes as a whole.
type Topic string

// The topics of a Snapshot.
const (
	TopicVMs      Topic = "vms"
	TopicHost     Topic = "host"
	TopicActivity Topic = "activity"
	// TopicTasks changes when a task starts, ends or is dismissed.
	TopicTasks Topic = "tasks"
	// TopicOutput changes while a running task writes output, which only
	// the task cards show.
	TopicOutput Topic = "output"
)

// Snapshot is everything brig ui knows at one moment.
type Snapshot struct {
	Host Host
	// VMs are sorted by name.
	VMs []VM
	// VMsErr says why VMs may be incomplete or their states unknown, such
	// as a record that does not load or a libvirt daemon that does not
	// answer.
	VMsErr string
	// NewestImage is the newest base image on the host, if any.
	NewestImage *Image
	// Images are the base images on the host, newest first.
	Images []Image
	// Config is what config.yaml sets that the dialogs offer.
	Config Config
	// Profiles are the network profiles of config.yaml.
	Profiles map[string]config.NetworkProfile
	// Doctor is the outcome of the last host check, nil before the first.
	Doctor *DoctorReport
	// Activity holds the runs of brig commands, newest first.
	Activity []Run
	// Tasks are the commands that brig ui started, in the order they
	// started: running ones, and finished ones until they are dismissed.
	Tasks []Task
	// Gateways are the OpenShell gateways of the VMs, sorted by VM name.
	Gateways []Gateway
	// Traffic is what the gateways' sandboxes requested, by VM name.
	Traffic []Traffic
}

// Config is the part of config.yaml that the dialogs prefill.
type Config struct {
	// Defaults are the settings of a new VM.
	Defaults config.Defaults
	// OpenShellConfigs are the config directories of a new VM.
	OpenShellConfigs []string
}

// Host describes the machine brig ui runs on.
type Host struct {
	Name         string
	BrigVersion  string
	OpenShellCLI string
	// Addr is where brig ui listens, such as 127.0.0.1:7878.
	Addr string
}

// VM is a VM record with what brig knows about it at run time.
type VM struct {
	vm.VM
	// State is the domain state as brig list shows it: running, shut off,
	// paused, missing, other or unknown.
	State string
	// OpenShell is the OpenShell version of the VM's base image, empty
	// when the image is gone.
	OpenShell string
	// LastSync is the outcome of the last sync of the VM's OpenShell
	// config directories, nil if there was none.
	LastSync *SyncRecord
	// Up is when the VM booted; zero when unknown.
	Up time.Time
	// Stats are the latest usage numbers of a running VM, if known.
	Stats *Stats
}

// GatewayName is the name of the VM's OpenShell gateway on the host.
func (v VM) GatewayName() string { return v.VM.GatewayName() }

// DomainName is the name of the VM's libvirt domain.
func (v VM) DomainName() string { return v.VM.DomainName() }

// Running reports whether the VM's domain runs.
func (v VM) Running() bool { return v.State == "running" }

// SyncRecord is the outcome of a sync.
type SyncRecord struct {
	Time  time.Time
	Error string
}

// Stats are a VM's usage numbers. Each is nil when unknown.
type Stats struct {
	// CPU is the share of the VM's vCPUs in use, in percent.
	CPU *float64
	// Memory is the share of the VM's memory in use, in percent.
	Memory *float64
	// MemoryFromHost says that Memory is the QEMU process's resident
	// memory, because the guest has not reported its own use: it counts
	// memory the guest touched once and has freed since.
	MemoryFromHost bool
	// Data is the share of the data disk in use, in percent.
	Data *float64
}

// Image is a base image.
type Image struct {
	ID        string
	Fedora    int
	OpenShell string
	CreatedAt time.Time
	// UsedBy are the VMs that run on the image.
	UsedBy []string
}

// DoctorReport is the outcome of brig doctor's checks.
type DoctorReport struct {
	Time    time.Time
	Results []doctor.Result
	// Running is set while the checks run again.
	Running bool
}

// Run is one brig command, from the activity log.
type Run struct {
	ID string
	// Trace is the trace the run belongs to: the one a caller handed on in
	// TRACEPARENT, which is how brig ui finds the run of a task it started.
	Trace string
	// Title is the command as a user would type it, such as
	// "brig start dev".
	Title   string
	Start   time.Time
	End     *time.Time
	Status  string // "", ok, failed, canceled or skipped
	Err     string
	Program string
	Version string
	// Steps are the spans below the command a terminal display would
	// show.
	Steps []Span
}

// Done reports whether the run has ended.
func (r Run) Done() bool { return r.End != nil }

// Task states.
const (
	TaskRunning = "running"
	TaskOK      = "ok"
	TaskFailed  = "failed"
)

// Task is a command that brig ui started for a user. The steps it shows come
// from the run in the activity log with the same Trace.
type Task struct {
	ID string
	// Action is the action that built the command.
	Action string
	// Display is the command as the dialog showed it.
	Display string
	// VM is the VM the command works on, empty for none.
	VM string
	// Trace is the trace ID that the command got in TRACEPARENT.
	Trace string
	Start time.Time
	End   *time.Time
	// Status is TaskRunning, TaskOK or TaskFailed.
	Status string
	// Err says in one line why a failed command failed.
	Err string
	// Output is the end of what the command wrote to standard output and
	// standard error, escaped for display.
	Output string
	// ShowOutput shows the output also when the command succeeded, as for
	// a preview whose output is the point.
	ShowOutput bool
	// Next names the action to offer once the command succeeded, such as
	// the sync after its preview.
	Next string
	// NextForm fills the fields of Next's dialog, as key and value pairs.
	NextForm []string
}

// Done reports whether the command has ended.
func (t Task) Done() bool { return t.End != nil }

// Span is a step of a run, and the steps below it.
type Span struct {
	Kind    string
	Name    string
	Status  string // "", ok, failed, canceled or skipped
	Message string
	Err     string
	Done    bool
	Steps   []Span
}

// Store holds the current Snapshot and notifies subscribers of changes.
type Store struct {
	mu   sync.Mutex
	snap Snapshot
	subs map[chan Topic]struct{}
	// seq is the number of the last request added.
	seq uint64
}

// New returns an empty store.
func New() *Store {
	return &Store{subs: map[chan Topic]struct{}{}}
}

// Snapshot returns the current snapshot.
func (s *Store) Snapshot() Snapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.snap
}

// Subscribe returns a channel that receives the topic of each change, and a
// function that ends the subscription. A subscriber that falls behind misses
// notifications of topics it already has pending, never the last one.
func (s *Store) Subscribe() (<-chan Topic, func()) {
	ch := make(chan Topic, 8)
	s.mu.Lock()
	s.subs[ch] = struct{}{}
	s.mu.Unlock()
	return ch, func() {
		s.mu.Lock()
		delete(s.subs, ch)
		s.mu.Unlock()
	}
}

// update changes the snapshot with fn under the lock and notifies the
// subscribers of t.
func (s *Store) update(t Topic, fn func(*Snapshot)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	fn(&s.snap)
	s.notify(t)
}

// notify tells the subscribers that t changed. The caller holds the lock.
func (s *Store) notify(t Topic) {
	for ch := range s.subs {
		select {
		case ch <- t:
		default:
			// The subscriber has notices pending and renders from the
			// snapshot it reads then, which includes this change.
		}
	}
}

// SetHost replaces the host description.
func (s *Store) SetHost(h Host) {
	s.update(TopicHost, func(sn *Snapshot) { sn.Host = h })
}

// VMState is what one poll of brig's state reads: the VMs, why they may be
// incomplete, the base images and what config.yaml sets.
type VMState struct {
	VMs []VM
	// Err says why VMs may be incomplete or their states unknown.
	Err string
	// Images are the base images, newest first.
	Images   []Image
	Profiles map[string]config.NetworkProfile
	Config   Config
}

// SetVMs replaces what the brig collector reads.
func (s *Store) SetVMs(v VMState) {
	s.update(TopicVMs, func(sn *Snapshot) {
		sn.VMs, sn.VMsErr, sn.Images, sn.Profiles, sn.Config = v.VMs, v.Err, v.Images, v.Profiles, v.Config
		sn.NewestImage = nil
		if len(v.Images) > 0 {
			sn.NewestImage = &v.Images[0]
		}
	})
}

// SetDoctor replaces the outcome of the host check.
func (s *Store) SetDoctor(r *DoctorReport) {
	s.update(TopicHost, func(sn *Snapshot) { sn.Doctor = r })
}

// SetActivity replaces the runs.
func (s *Store) SetActivity(runs []Run) {
	s.update(TopicActivity, func(sn *Snapshot) { sn.Activity = runs })
}

// VM returns the named VM of the snapshot.
func (sn Snapshot) VM(name string) (VM, bool) {
	for _, v := range sn.VMs {
		if v.Name == name {
			return v, true
		}
	}
	return VM{}, false
}

// AddTask adds a task to the tasks shown.
func (s *Store) AddTask(t Task) {
	s.update(TopicTasks, func(sn *Snapshot) { sn.Tasks = append(slices.Clone(sn.Tasks), t) })
}

// UpdateTask changes the task with the ID id with fn and notifies the
// subscribers of topic, which is TopicTasks unless fn only changes the
// output. A task that is gone is left alone.
func (s *Store) UpdateTask(id string, topic Topic, fn func(*Task)) {
	s.update(topic, func(sn *Snapshot) {
		i := slices.IndexFunc(sn.Tasks, func(t Task) bool { return t.ID == id })
		if i < 0 {
			return
		}
		sn.Tasks = slices.Clone(sn.Tasks)
		fn(&sn.Tasks[i])
	})
}

// RemoveTask removes the task with the ID id from the tasks shown.
func (s *Store) RemoveTask(id string) {
	s.update(TopicTasks, func(sn *Snapshot) {
		sn.Tasks = slices.DeleteFunc(slices.Clone(sn.Tasks), func(t Task) bool { return t.ID == id })
	})
}

// Task returns the task with the ID id.
func (sn Snapshot) Task(id string) (Task, bool) {
	for _, t := range sn.Tasks {
		if t.ID == id {
			return t, true
		}
	}
	return Task{}, false
}
