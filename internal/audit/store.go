// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

package audit

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/dennisklein/brig/internal/paths"
)

// Files in a VM's audit directory, besides the day files.
const (
	indexFile     = "index.json"
	positionsFile = "positions.json"
	// collectorLock is held by the one collector that writes the
	// directory, writeLock around each change to the files.
	collectorLock = "collector.lock"
	writeLock     = "write.lock"
	dayLayout     = time.DateOnly
	plainExt      = ".jsonl"
	gzipExt       = ".jsonl.gz"
)

// Store is the audit directory of every VM, under brig's state directory.
type Store struct{ Root string }

// NewStore returns the audit store below brig's state directory.
func NewStore(d paths.Dirs) Store { return Store{Root: filepath.Join(d.State, "audit")} }

// Dir is the audit directory of the VM.
func (s Store) Dir(vm string) string { return filepath.Join(s.Root, vm) }

// VMs lists the VMs that have an audit directory, sorted.
func (s Store) VMs() ([]string, error) {
	ents, err := os.ReadDir(s.Root)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range ents {
		if e.IsDir() && !strings.HasPrefix(e.Name(), ".") {
			out = append(out, e.Name())
		}
	}
	return out, nil
}

// Count is how much of a day's file a sandbox's records of a kind take.
type Count struct {
	Rows  int64 `json:"rows"`
	Bytes int64 `json:"bytes"`
}

// Day describes a day's file: the records written on that day, in UTC.
type Day struct {
	// File is the file's name: DATE.jsonl while it is written, then
	// DATE.jsonl.gz.
	File string `json:"file"`
	// First and Last are the earliest and latest record times in the file,
	// which can lie before the day, as records that a collector read late.
	First time.Time `json:"first,omitzero"`
	Last  time.Time `json:"last,omitzero"`
	// Bytes counts the records' bytes, Disk the file's size.
	Bytes int64 `json:"bytes"`
	Disk  int64 `json:"disk"`
	// Counts are per sandbox (WORKSPACE/NAME, empty for the VM) and kind.
	Counts map[string]map[string]Count `json:"counts"`
}

// Rows is how many records the day has.
func (d *Day) Rows() int64 {
	var n int64
	for _, kinds := range d.Counts {
		for _, c := range kinds {
			n += c.Rows
		}
	}
	return n
}

func (d *Day) add(r Record, size int64) {
	if d.First.IsZero() || r.T.Before(d.First) {
		d.First = r.T
	}
	if r.T.After(d.Last) {
		d.Last = r.T
	}
	d.Bytes += size
	if d.Counts == nil {
		d.Counts = map[string]map[string]Count{}
	}
	k := r.SandboxKey()
	if d.Counts[k] == nil {
		d.Counts[k] = map[string]Count{}
	}
	c := d.Counts[k][r.Kind]
	c.Rows++
	c.Bytes += size
	d.Counts[k][r.Kind] = c
}

// CollectorState is the state of a VM's collector as it last wrote it.
type CollectorState struct {
	// Started is when the running collector started; Heartbeat when it
	// last wrote the index, which it does at least every HeartbeatEvery.
	Started   time.Time `json:"started,omitzero"`
	Heartbeat time.Time `json:"heartbeat,omitzero"`
	// LastEvent is the time of the latest record written.
	LastEvent time.Time `json:"last_event,omitzero"`
	// Stopped is when it ended, zero while it runs or after it crashed.
	Stopped time.Time `json:"stopped,omitzero"`
	// Err is the latest reason it could not read the gateway, empty once
	// it reads again.
	Err string `json:"error,omitempty"`
	// Gaps are the times of the gap records of the last week.
	Gaps []time.Time `json:"gaps,omitempty"`
}

// HeartbeatEvery is how often a running collector writes its index at least.
const HeartbeatEvery = 10 * time.Second

// Index sums up a VM's audit directory, so that status and the breakdown
// need not read the records. The collector keeps it up to date as it
// writes; prune rewrites it.
type Index struct {
	V  int    `json:"v"`
	VM string `json:"vm"`
	// Days are keyed by date.
	Days      map[string]*Day `json:"days"`
	Collector CollectorState  `json:"collector"`
	// Deleted is when brig delete deleted the VM; its history stays.
	Deleted time.Time `json:"deleted,omitzero"`
	// Sandboxes are the sandboxes that have records, with when they were
	// deleted, zero while they exist.
	Sandboxes map[string]time.Time `json:"sandboxes,omitempty"`
}

// DayNames are the index's dates, oldest first.
func (x *Index) DayNames() []string { return slices.Sorted(maps.Keys(x.Days)) }

// Disk is the size of the VM's day files.
func (x *Index) Disk() int64 {
	var n int64
	for _, d := range x.Days {
		n += d.Disk
	}
	return n
}

// GapsSince counts the gaps since t.
func (x *Index) GapsSince(t time.Time) int {
	n := 0
	for _, g := range x.Collector.Gaps {
		if !g.Before(t) {
			n++
		}
	}
	return n
}

// ReadIndex reads the VM's index. A VM without one has an empty index.
func (s Store) ReadIndex(vm string) (*Index, error) {
	x := &Index{V: Version, VM: vm, Days: map[string]*Day{}}
	b, err := os.ReadFile(filepath.Join(s.Dir(vm), indexFile))
	if errors.Is(err, fs.ErrNotExist) {
		return x, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(b, x); err != nil {
		return nil, fmt.Errorf("%s: %w", filepath.Join(s.Dir(vm), indexFile), err)
	}
	if x.Days == nil {
		x.Days = map[string]*Day{}
	}
	return x, nil
}

// writeIndex replaces the VM's index; the caller holds the write lock.
func (s Store) writeIndex(x *Index) error {
	return writeJSONFile(filepath.Join(s.Dir(x.VM), indexFile), x)
}

// writeJSONFile replaces the file atomically.
func writeJSONFile(path string, v any) error {
	b, err := json.MarshalIndent(v, "", " ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// lock takes the lock file name of the VM's directory, waiting for it
// unless wait is false, and returns its release.
func (s Store) lock(vm, name string, wait bool) (func(), error) {
	if err := os.MkdirAll(s.Dir(vm), 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(s.Dir(vm), name), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	how := syscall.LOCK_EX
	if !wait {
		how |= syscall.LOCK_NB
	}
	if err := syscall.Flock(int(f.Fd()), how); err != nil {
		_ = f.Close()
		return nil, err
	}
	return func() { _ = f.Close() }, nil
}

// Collecting reports whether a collector holds the VM's directory.
func (s Store) Collecting(vm string) bool {
	f, err := os.Open(filepath.Join(s.Dir(vm), collectorLock))
	if err != nil {
		return false
	}
	defer func() { _ = f.Close() }()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_SH|syscall.LOCK_NB); err != nil {
		return errors.Is(err, syscall.EWOULDBLOCK)
	}
	return false
}

// dayOf is the date of the day file that a record written at t goes to.
func dayOf(t time.Time) string { return t.UTC().Format(dayLayout) }

// dayPath returns the file that holds the VM's records of day: the plain
// one while it exists, else the gzipped one, or "" for neither.
func (s Store) dayPath(vm, day string) string {
	for _, ext := range []string{plainExt, gzipExt} {
		p := filepath.Join(s.Dir(vm), day+ext)
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return ""
}

// appendDay appends lines to the VM's file of day; the caller holds the
// write lock.
func (s Store) appendDay(vm, day string, lines []byte) (int64, error) {
	p := filepath.Join(s.Dir(vm), day+plainExt)
	if _, err := os.Stat(filepath.Join(s.Dir(vm), day+gzipExt)); err == nil {
		// The day was finished, and records for it arrive late: they go
		// to a further gzip member, which readers take as one stream.
		return s.appendGzip(filepath.Join(s.Dir(vm), day+gzipExt), lines)
	}
	f, err := os.OpenFile(p, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return 0, err
	}
	_, err = f.Write(lines)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return 0, err
	}
	fi, err := os.Stat(p)
	if err != nil {
		return 0, err
	}
	return fi.Size(), nil
}

func (s Store) appendGzip(p string, lines []byte) (int64, error) {
	f, err := os.OpenFile(p, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return 0, err
	}
	zw := gzip.NewWriter(f)
	_, err = zw.Write(lines)
	if cerr := zw.Close(); err == nil {
		err = cerr
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return 0, err
	}
	fi, err := os.Stat(p)
	if err != nil {
		return 0, err
	}
	return fi.Size(), nil
}

// compress gzips the VM's plain day files of days before today and
// returns the size of each on disk; the caller holds the write lock.
func (s Store) compress(vm string, today string) (map[string]int64, error) {
	ents, err := os.ReadDir(s.Dir(vm))
	if err != nil {
		return nil, err
	}
	out := map[string]int64{}
	for _, e := range ents {
		day, ok := strings.CutSuffix(e.Name(), plainExt)
		if !ok || day >= today {
			continue
		}
		if _, err := time.Parse(dayLayout, day); err != nil {
			continue
		}
		size, err := gzipFile(filepath.Join(s.Dir(vm), e.Name()), filepath.Join(s.Dir(vm), day+gzipExt))
		if err != nil {
			return out, err
		}
		out[day] = size
	}
	return out, nil
}

// gzipFile replaces the plain file src with the gzipped dst, which a crash
// leaves either whole or absent, and returns dst's size.
func gzipFile(src, dst string) (int64, error) {
	in, err := os.Open(src)
	if err != nil {
		return 0, err
	}
	defer func() { _ = in.Close() }()
	tmp := dst + ".tmp"
	out, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return 0, err
	}
	zw := gzip.NewWriter(out)
	_, err = io.Copy(zw, in)
	if cerr := zw.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = out.Sync()
	}
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp, dst)
	}
	if err != nil {
		_ = os.Remove(tmp)
		return 0, err
	}
	if err := os.Remove(src); err != nil {
		return 0, err
	}
	fi, err := os.Stat(dst)
	if err != nil {
		return 0, err
	}
	return fi.Size(), nil
}

// readDay calls each with each record of the VM's file of day, in the order
// written. Lines that are no record of a version it reads are skipped.
func (s Store) readDay(vm, day string, each func(Record) bool) error {
	p := s.dayPath(vm, day)
	if p == "" {
		return nil
	}
	f, err := os.Open(p)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			// Pruned meanwhile.
			return nil
		}
		return err
	}
	defer func() { _ = f.Close() }()
	var r io.Reader = f
	if strings.HasSuffix(p, gzipExt) {
		zr, err := gzip.NewReader(f)
		if err != nil {
			return fmt.Errorf("%s: %w", p, err)
		}
		defer func() { _ = zr.Close() }()
		r = zr
	}
	br := bufio.NewReaderSize(r, 64<<10)
	for {
		line, err := readLine(br)
		if rec, ok := parseLine(line); ok && !each(rec) {
			return nil
		}
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("%s: %w", p, err)
		}
	}
}

// maxLine bounds a record's line; the collector writes none longer, and
// readers skip longer ones.
const maxLine = 4 << 20

// readLine reads br's next line. It returns nil for a line longer than
// maxLine, which it reads to its end, so that one such line keeps no
// reader from the lines after it.
func readLine(br *bufio.Reader) ([]byte, error) {
	var line []byte
	long := false
	for {
		chunk, err := br.ReadSlice('\n')
		if !long && len(line)+len(chunk) > maxLine {
			long, line = true, nil
		}
		if !long {
			line = append(line, chunk...)
		}
		if !errors.Is(err, bufio.ErrBufferFull) {
			return line, err
		}
	}
}

func parseLine(b []byte) (Record, bool) {
	b = bytes.TrimSpace(b)
	if len(b) == 0 {
		return Record{}, false
	}
	var r Record
	if json.Unmarshal(b, &r) != nil || r.V < 1 || r.V > Version || r.Kind == "" {
		return Record{}, false
	}
	return r, true
}
