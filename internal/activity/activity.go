// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

// Package activity reads brig's activity log, the clikit event log that brig
// commands append to and that brig ui writes the spans of the openshell
// commands it runs into: it turns the lines into runs, each a command with
// the tree of its steps.
package activity

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"slices"
	"syscall"
	"time"

	"github.com/GSI-HPC/go-clikit/termtext"

	"github.com/dennisklein/brig/internal/ui/store"
)

// maxLine is the longest log line read; longer ones are skipped.
const maxLine = 1 << 20

// logLine is what the reducer reads of a line of a clikit event log (v1):
// the trace line that starts a run, or an event of one of its spans.
type logLine struct {
	V       int      `json:"v"`
	Type    string   `json:"type"`
	Run     string   `json:"run"`
	Trace   string   `json:"trace"`
	Program string   `json:"program"`
	Version string   `json:"version"`
	Time    string   `json:"time"`
	Span    string   `json:"span"`
	Parent  string   `json:"parent"`
	Kind    string   `json:"kind"`
	Name    string   `json:"name"`
	Node    string   `json:"node"`
	Flags   []string `json:"flags"`
	State   string   `json:"state"`
	Message string   `json:"message"`
	Status  string   `json:"status"`
	Err     string   `json:"err"`
}

// Reducer turns the lines of clikit event logs into runs and their span
// trees. Lines of several runs may interleave, as when commands append to
// one log at once.
type Reducer struct {
	runs map[string]*run
	// order lists the runs as they started.
	order []string
	// max is how many runs it keeps, the newest.
	max int
}

type run struct {
	id, trace        string
	program, version string
	root             string
	spans            map[string]*span
	start            time.Time
}

type span struct {
	id, parent, kind, name, node string
	hidden                       bool
	status, message, err         string
	done                         bool
	start, end                   time.Time
	children                     []string
}

// NewReducer returns an empty reducer that keeps the newest keep runs.
func NewReducer(keep int) *Reducer {
	return &Reducer{runs: map[string]*run{}, max: keep}
}

// Line reads one line of a log. Lines that are not JSON, of another schema
// version or of a kind it does not know are left out.
func (r *Reducer) Line(b []byte) {
	var l logLine
	if json.Unmarshal(b, &l) != nil || l.V != 1 || l.Run == "" {
		return
	}
	ru := r.runs[l.Run]
	if ru == nil {
		ru = &run{id: l.Run, spans: map[string]*span{}}
		r.runs[l.Run] = ru
		r.order = append(r.order, l.Run)
		r.trim()
	}
	if l.Type == "trace" {
		ru.program, ru.version = clean(l.Program), clean(l.Version)
		ru.trace = l.Trace
		return
	}
	if l.Span == "" {
		return
	}
	t, _ := time.Parse(time.RFC3339Nano, l.Time)
	sp := ru.spans[l.Span]
	if sp == nil {
		sp = &span{id: l.Span, parent: l.Parent, start: t}
		ru.spans[l.Span] = sp
		if p := ru.spans[l.Parent]; p != nil {
			p.children = append(p.children, l.Span)
		}
		if l.Kind == "command" && l.Parent == "" && ru.root == "" {
			ru.root, ru.start = l.Span, t
		}
	}
	sp.kind, sp.name = l.Kind, clean(l.Name)
	if l.Node != "" {
		sp.node = clean(l.Node)
	}
	sp.hidden = slices.Contains(l.Flags, "hidden")
	if l.Message != "" {
		sp.message = clean(l.Message)
	}
	if l.Type == "end" || l.State == "ended" {
		sp.done, sp.end = true, t
		sp.status, sp.err = l.Status, clean(l.Err)
	}
}

// trim forgets the oldest runs beyond max.
func (r *Reducer) trim() {
	for len(r.order) > r.max {
		delete(r.runs, r.order[0])
		r.order = r.order[1:]
	}
}

// Runs returns the runs with a command span, newest first.
func (r *Reducer) Runs() []store.Run {
	var out []store.Run
	for i := len(r.order) - 1; i >= 0; i-- {
		ru := r.runs[r.order[i]]
		root := ru.spans[ru.root]
		if root == nil {
			continue
		}
		program := ru.program
		if program == "" {
			program = "brig"
		}
		sr := store.Run{
			ID: ru.id, Trace: ru.trace, Title: program + " " + root.name, Start: ru.start,
			Status: root.status, Err: root.err, Program: program, Version: ru.version,
			VM: root.node, Steps: ru.steps(root),
		}
		if root.done {
			end := root.end
			sr.End = &end
		}
		out = append(out, sr)
	}
	return out
}

// steps returns the visible spans below sp.
func (ru *run) steps(sp *span) []store.Span {
	var out []store.Span
	for _, id := range sp.children {
		c := ru.spans[id]
		if c == nil || c.hidden {
			continue
		}
		out = append(out, store.Span{
			Kind: c.kind, Name: c.name, Status: c.status, Message: c.message,
			Err: c.err, Done: c.done, Steps: ru.steps(c),
		})
	}
	return out
}

// clean escapes control characters, as a terminal display would. clikit
// sanitises what it logs; this covers a log something else wrote.
func clean(s string) string { return termtext.Escape(s) }

// Tail reads a log as it grows, and from the start again after it was
// rotated or truncated.
type Tail struct {
	path    string
	red     *Reducer
	ino     uint64
	off     int64
	partial []byte
	started bool
}

// NewTail returns a reader of the log at path that feeds red. It reads the
// rotated copy, path.1, first, so that the runs before the last rotation
// count too.
func NewTail(path string, red *Reducer) *Tail {
	t := &Tail{path: path, red: red}
	t.readOld()
	return t
}

// readOld reads the rotated copy of the log, if any.
func (t *Tail) readOld() {
	f, err := os.Open(t.path + ".1")
	if err != nil {
		return
	}
	defer func() { _ = f.Close() }()
	readLines(f, t.red, nil)
}

// Poll reads what was appended since the last poll and reports whether
// anything changed. A missing log is an empty one.
func (t *Tail) Poll() bool {
	fi, err := os.Stat(t.path)
	if errors.Is(err, fs.ErrNotExist) {
		changed := !t.started
		t.started = true
		return changed
	}
	if err != nil {
		return false
	}
	var ino uint64
	if s, ok := fi.Sys().(*syscall.Stat_t); ok {
		ino = s.Ino
	}
	if ino != t.ino || fi.Size() < t.off {
		// A new file: rotated, or truncated.
		t.ino, t.off, t.partial = ino, 0, nil
	}
	if fi.Size() == t.off && t.started {
		return false
	}
	t.started = true
	f, err := os.Open(t.path)
	if err != nil {
		return false
	}
	defer func() { _ = f.Close() }()
	if _, err := f.Seek(t.off, io.SeekStart); err != nil {
		return false
	}
	n, partial := readLines(f, t.red, t.partial)
	t.off += n
	t.partial = partial
	return true
}

// readLines feeds the whole lines of r, after partial, to red. It returns
// the bytes read and the unfinished last line.
func readLines(r io.Reader, red *Reducer, partial []byte) (int64, []byte) {
	br := bufio.NewReader(r)
	var n int64
	buf := partial
	for {
		chunk, err := br.ReadSlice('\n')
		n += int64(len(chunk))
		buf = append(buf, chunk...)
		if err == nil {
			if len(buf) <= maxLine {
				red.Line(bytes.TrimSpace(buf))
			}
			buf = buf[:0]
			continue
		}
		if errors.Is(err, bufio.ErrBufferFull) {
			if len(buf) > maxLine {
				// Keep reading to the line's end, but not what it holds.
				buf = buf[:maxLine+1]
			}
			continue
		}
		// io.EOF or a read error: what is left is a line still being
		// written.
		return n, slices.Clone(buf)
	}
}
