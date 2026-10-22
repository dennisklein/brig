// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

// Package activity reads brig's activity log, the clikit event log that brig
// commands append to and that brig ui writes the spans of the openshell
// commands it runs into: it turns the lines into runs, each a command with
// the tree of its steps.
// Package activity reads brig's activity log, the clikit event log that brig
// commands append to and that brig ui writes the spans of the openshell
// commands it runs into: it turns the lines into runs, each a command with
// the tree of its steps.
package activity

import (
	"github.com/GSI-HPC/go-clikit/progress"
	"github.com/GSI-HPC/go-clikit/progress/eventlog"

	"github.com/dennisklein/brig/internal/ui/store"
)

// Reducer turns the lines of clikit event logs into runs and their span
// trees.
type Reducer struct{ red *eventlog.Reducer }

// NewReducer returns an empty reducer that keeps the newest keep runs.
func NewReducer(keep int) *Reducer {
	return &Reducer{red: eventlog.NewReducer(eventlog.Options{Keep: keep})}
}

// Runs returns the runs with a command span, newest first.
func (r *Reducer) Runs() []store.Run {
	var out []store.Run
	for _, ru := range r.red.Runs() {
		program := ru.Program
		if program == "" {
			program = "brig"
		}
		root := ru.Root
		sr := store.Run{
			ID: ru.ID, Trace: ru.Trace.Trace.String(), Title: program + " " + root.Name, Start: root.Queued,
			Status: statusOf(root), Err: root.Err, Program: program, Version: ru.Version,
			VM: root.Node, Steps: steps(root),
		}
		if root.Done() {
			end := root.Ended
			sr.End = &end
		}
		out = append(out, sr)
	}
	return out
}

// steps returns the visible spans below sp.
func steps(sp eventlog.Span) []store.Span {
	var out []store.Span
	for _, c := range sp.Children {
		if c.Flags&progress.Hidden != 0 {
			continue
		}
		out = append(out, store.Span{
			Kind: c.Kind.String(), Name: c.Name, Status: statusOf(c), Message: c.Message,
			Err: c.Err, Done: c.Done(), Steps: steps(c),
		})
	}
	return out
}

// statusOf names how a span ended, or "" while it has not.
func statusOf(sp eventlog.Span) string {
	if !sp.Done() || sp.Status == 0 {
		return ""
	}
	return sp.Status.String()
}

// Tail reads a log as it grows, and from the start again after it was
// rotated or truncated, reading the rotated copy, path.1, first.
type Tail struct{ t *eventlog.Tail }

// NewTail returns a reader of the log at path that feeds red.
func NewTail(path string, red *Reducer) *Tail {
	return &Tail{t: eventlog.NewTail(path, red.red)}
}

// Poll reads what was appended since the last poll and reports whether
// anything changed. A missing log is an empty one.
func (t *Tail) Poll() bool {
	changed, _ := t.t.Poll()
	return changed
}
