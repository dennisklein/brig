// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

package audit

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"syscall"
	"time"
)

// gapsKept is how long the index keeps the times of gaps.
const gapsKept = 7 * 24 * time.Hour

// Writer appends records to a VM's audit directory. It is the one writer of
// the directory while it is open, which its lock ensures.
type Writer struct {
	s  Store
	vm string
	// Now returns the present; tests replace it.
	Now func() time.Time

	unlock func()
	mu     sync.Mutex
	queue  []Record
	index  *Index
}

// ErrCollecting refuses to open a VM's directory that another collector
// writes.
var ErrCollecting = errors.New("another collector writes this VM's audit trail")

// OpenWriter takes the VM's directory for writing and reads its index.
func (s Store) OpenWriter(vm string) (*Writer, error) {
	unlock, err := s.lock(vm, collectorLock, false)
	if errors.Is(err, syscall.EWOULDBLOCK) {
		return nil, ErrCollecting
	}
	if err != nil {
		return nil, err
	}
	x, err := s.clearDeleted(vm)
	if err != nil {
		unlock()
		return nil, err
	}
	return &Writer{s: s, vm: vm, Now: time.Now, unlock: unlock, index: x}, nil
}

// Add queues records for the next Flush, completing their version and VM.
func (w *Writer) Add(rs ...Record) {
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, r := range rs {
		r.V, r.VM = Version, w.vm
		if r.T.IsZero() {
			r.T = w.Now()
		}
		w.queue = append(w.queue, r)
	}
}

// Update changes the collector's state in the index at the next Flush.
func (w *Writer) Update(f func(*CollectorState)) {
	w.mu.Lock()
	defer w.mu.Unlock()
	f(&w.index.Collector)
}

// Flush writes the queued records to today's file and the index, and then
// positions, if not nil, as where the collector resumes. It writes the index
// also without records, as the collector's heartbeat.
func (w *Writer) Flush(positions any) error {
	w.mu.Lock()
	queue := w.queue
	w.queue = nil
	w.mu.Unlock()
	unlock, err := w.s.lock(w.vm, writeLock, true)
	if err != nil {
		w.requeue(queue)
		return err
	}
	defer unlock()
	// Prune may have changed the index since.
	x, err := w.s.ReadIndex(w.vm)
	if err != nil {
		w.requeue(queue)
		return err
	}
	w.mu.Lock()
	x.Collector = w.index.Collector
	w.mu.Unlock()
	now := w.Now()
	day := dayOf(now)
	var lines bytes.Buffer
	d := x.Days[day]
	if d == nil {
		d = &Day{File: day + plainExt}
	}
	for _, r := range queue {
		b, err := json.Marshal(r)
		if err != nil {
			return fmt.Errorf("writing an audit record: %w", err)
		}
		if len(b) >= maxLine {
			// No reader would take it; a request's raw line is the only
			// field that long.
			r.Raw = r.Raw[:min(len(r.Raw), 4096)]
			if b, err = json.Marshal(r); err != nil {
				return err
			}
		}
		b = append(b, '\n')
		lines.Write(b)
		d.add(r, int64(len(b)))
		if r.T.After(x.Collector.LastEvent) {
			x.Collector.LastEvent = r.T
		}
		if r.Kind == KindGap {
			x.Collector.Gaps = append(x.Collector.Gaps, r.T)
		}
		if k := r.SandboxKey(); k != "" {
			if x.Sandboxes == nil {
				x.Sandboxes = map[string]time.Time{}
			}
			deleted := x.Sandboxes[k]
			switch {
			case r.Kind == KindSandbox && r.Event == EventDeleted:
				deleted = r.T
			case r.Kind == KindSandbox && r.Event == EventCreated:
				deleted = time.Time{}
			}
			x.Sandboxes[k] = deleted
		}
	}
	x.Collector.Gaps = slices.DeleteFunc(x.Collector.Gaps, func(t time.Time) bool { return now.Sub(t) > gapsKept })
	if lines.Len() > 0 {
		size, err := w.s.appendDay(w.vm, day, lines.Bytes())
		if err != nil {
			w.requeue(queue)
			return err
		}
		d.Disk = size
		x.Days[day] = d
	}
	sizes, err := w.s.compress(w.vm, day)
	for day, size := range sizes {
		if d := x.Days[day]; d != nil {
			d.File, d.Disk = day+gzipExt, size
		}
	}
	x.Collector.Heartbeat = now
	w.mu.Lock()
	w.index = x
	w.mu.Unlock()
	err = errors.Join(err, w.s.writeIndex(x))
	if positions != nil {
		err = errors.Join(err, writeJSONFile(w.s.positionsPath(w.vm), positions))
	}
	return err
}

func (w *Writer) requeue(rs []Record) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.queue = append(rs, w.queue...)
}

// Close flushes, marks the collector stopped, and gives the directory up.
func (w *Writer) Close(positions any) error {
	w.Update(func(c *CollectorState) { c.Stopped = w.Now() })
	return w.release(positions)
}

// release flushes and gives the directory up.
func (w *Writer) release(positions any) error {
	err := w.Flush(positions)
	w.unlock()
	return err
}

// clearDeleted reads the VM's index, and clears its deletion: a VM of the
// same name was created, whose records join the old ones.
func (s Store) clearDeleted(vm string) (*Index, error) {
	unlock, err := s.lock(vm, writeLock, true)
	if err != nil {
		return nil, err
	}
	defer unlock()
	x, err := s.ReadIndex(vm)
	if err != nil || x.Deleted.IsZero() {
		return x, err
	}
	x.Deleted = time.Time{}
	return x, s.writeIndex(x)
}

// MarkDeleted records in the VM's index that brig delete deleted it, and
// reports whether the VM has an audit trail at all.
func (s Store) MarkDeleted(vm string, at time.Time) (bool, error) {
	if _, err := os.Stat(s.Dir(vm)); err != nil {
		return false, nil
	}
	unlock, err := s.lock(vm, writeLock, true)
	if err != nil {
		return true, err
	}
	defer unlock()
	x, err := s.ReadIndex(vm)
	if err != nil {
		return true, err
	}
	x.Deleted = at
	return true, s.writeIndex(x)
}

func (s Store) positionsPath(vm string) string { return filepath.Join(s.Dir(vm), positionsFile) }

// ReadPositions reads where the VM's collector resumes into v; without a
// file, v stays as it is.
func (s Store) ReadPositions(vm string, v any) error {
	b, err := os.ReadFile(s.positionsPath(vm))
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	return json.Unmarshal(b, v)
}
