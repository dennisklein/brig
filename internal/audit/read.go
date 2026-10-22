// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

package audit

import (
	"bufio"
	"compress/gzip"
	"context"
	"errors"
	"io"
	"io/fs"
	"maps"
	"net/netip"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

// Query selects records. Empty fields select everything.
type Query struct {
	// VMs are the VMs to read; none reads every VM of the store.
	VMs []string
	// Workspace and Sandbox select a sandbox's records; Workspace alone
	// those of a workspace.
	Workspace, Sandbox string
	// Since and Until bound the record times; Until is exclusive.
	Since, Until time.Time
	// Kinds select records of these kinds.
	Kinds []string
	// Denied selects denied requests and drops, and with Kinds the other
	// kinds too.
	Denied bool
	// Host selects requests, drafts and drops by host: a pattern as
	// path.Match takes it, where one without wildcards matches the host
	// and its subdomains. A drop's host is an address, which matches an
	// address, a network such as 192.168.0.0/16, or a pattern.
	Host string
}

// Match reports whether the query selects the record.
func (q Query) Match(r Record) bool {
	switch {
	case q.Workspace != "" && r.Workspace != q.Workspace:
		return false
	case q.Sandbox != "" && r.Sandbox != q.Sandbox:
		return false
	case !q.Since.IsZero() && r.T.Before(q.Since):
		return false
	case !q.Until.IsZero() && !r.T.Before(q.Until):
		return false
	case len(q.Kinds) > 0 && !slices.Contains(q.Kinds, r.Kind):
		return false
	case q.Denied && r.Kind == KindRequest && !r.Denied():
		return false
	case q.Denied && len(q.Kinds) == 0 && !r.Denied():
		return false
	}
	if q.Host == "" {
		return true
	}
	switch r.Kind {
	case KindDraft:
		return slices.ContainsFunc(r.Endpoints, func(ep string) bool { return MatchHost(q.Host, splitHost(ep)) })
	case KindDrop:
		return MatchAddr(q.Host, r.Host)
	}
	return r.Kind == KindRequest && MatchHost(q.Host, r.Host)
}

// MatchAddr reports whether the address addr matches pattern: the same
// address, a network that holds it, or a pattern as path.Match takes it.
func MatchAddr(pattern, addr string) bool {
	a, err := netip.ParseAddr(addr)
	if err != nil {
		return false
	}
	if p, err := netip.ParseAddr(pattern); err == nil {
		return p.Unmap() == a.Unmap()
	}
	if p, err := netip.ParsePrefix(pattern); err == nil {
		return p.Contains(a)
	}
	ok, _ := path.Match(strings.ToLower(pattern), addr)
	return ok
}

// MatchHost reports whether host matches pattern, as Query.Host says.
func MatchHost(pattern, host string) bool {
	pattern, host = strings.ToLower(pattern), strings.ToLower(host)
	if host == "" {
		return false
	}
	if !strings.ContainsAny(pattern, "*?[") {
		return host == pattern || strings.HasSuffix(host, "."+pattern)
	}
	ok, _ := path.Match(pattern, host)
	return ok
}

func splitHost(endpoint string) string {
	if i := strings.LastIndexByte(endpoint, ':'); i >= 0 {
		return endpoint[:i]
	}
	return endpoint
}

// mayHold reports whether the day has records of the sandboxes and kinds
// that q selects, as far as its counts tell.
func (q Query) mayHold(d *Day) bool {
	if d.Counts == nil || (q.Workspace == "" && len(q.Kinds) == 0) {
		return true
	}
	for key, kinds := range d.Counts {
		ws, name, _ := strings.Cut(key, "/")
		if (q.Workspace != "" && ws != q.Workspace) || (q.Sandbox != "" && name != q.Sandbox) {
			continue
		}
		if len(q.Kinds) == 0 {
			return true
		}
		for k := range kinds {
			if slices.Contains(q.Kinds, k) {
				return true
			}
		}
	}
	return false
}

// Reader reads records as Store.Read does.
type Reader interface {
	Read(q Query, each func(Record) bool) error
}

// Records are records held in memory, which Read selects from as a Store
// does; brig ui's demo has its history so.
type Records []Record

// Read calls each with the records that q selects, oldest first.
func (rs Records) Read(q Query, each func(Record) bool) error {
	var sel []Record
	for _, r := range rs {
		if (len(q.VMs) == 0 || slices.Contains(q.VMs, r.VM)) && q.Match(r) {
			sel = append(sel, r)
		}
	}
	slices.SortStableFunc(sel, func(a, b Record) int { return a.T.Compare(b.T) })
	for _, r := range sel {
		if !each(r) {
			break
		}
	}
	return nil
}

// vmsOf returns the VMs that q reads.
func (s Store) vmsOf(q Query) ([]string, error) {
	if len(q.VMs) > 0 {
		return q.VMs, nil
	}
	return s.VMs()
}

// days returns the dates of the VMs' files whose records may lie in q's
// time range, oldest first.
func (s Store) days(vms []string, q Query) ([]string, map[string][]string, error) {
	byDay := map[string][]string{}
	for _, vm := range vms {
		x, err := s.ReadIndex(vm)
		if err != nil {
			return nil, nil, err
		}
		for name, d := range x.Days {
			if !q.Since.IsZero() && !d.Last.IsZero() && d.Last.Before(q.Since) {
				continue
			}
			if !q.Until.IsZero() && !d.First.IsZero() && !d.First.Before(q.Until) {
				continue
			}
			if !q.mayHold(d) {
				continue
			}
			byDay[name] = append(byDay[name], vm)
		}
	}
	names := make([]string, 0, len(byDay))
	for name := range byDay {
		names = append(names, name)
	}
	slices.Sort(names)
	return names, byDay, nil
}

// Read calls each with the records that q selects, oldest first, until
// each returns false.
func (s Store) Read(q Query, each func(Record) bool) error {
	vms, err := s.vmsOf(q)
	if err != nil {
		return err
	}
	names, byDay, err := s.days(vms, q)
	if err != nil {
		return err
	}
	for _, day := range names {
		var recs []Record
		for _, vm := range byDay[day] {
			err := s.readDay(vm, day, func(r Record) bool {
				if q.Match(r) {
					recs = append(recs, r)
				}
				return true
			})
			if err != nil {
				return err
			}
		}
		// A day's records are in the order the collector read them,
		// which backfills can mix.
		slices.SortStableFunc(recs, func(a, b Record) int { return a.T.Compare(b.T) })
		for _, r := range recs {
			if !each(r) {
				return nil
			}
		}
	}
	return nil
}

// Oldest is the time of the oldest record of the VMs, zero for none.
func (s Store) Oldest(vms []string) (time.Time, error) {
	var oldest time.Time
	for _, vm := range vms {
		x, err := s.ReadIndex(vm)
		if err != nil {
			return time.Time{}, err
		}
		for _, d := range x.Days {
			if !d.First.IsZero() && (oldest.IsZero() || d.First.Before(oldest)) {
				oldest = d.First
			}
		}
	}
	return oldest, nil
}

// FollowEvery is how often Follow looks for new records.
var FollowEvery = time.Second

// Follow calls each with the records that q selects as they are written,
// from now on, until ctx ends.
func (s Store) Follow(ctx context.Context, q Query, each func(Record)) error {
	type tail struct {
		day    string
		offset int64
	}
	tails := map[string]*tail{}
	start := func(vm, day string) *tail {
		t := &tail{day: day}
		if fi, err := os.Stat(filepath.Join(s.Dir(vm), day+plainExt)); err == nil {
			t.offset = fi.Size()
		}
		return t
	}
	vms, err := s.vmsOf(q)
	if err != nil {
		return err
	}
	today := dayOf(time.Now())
	for _, vm := range vms {
		tails[vm] = start(vm, today)
	}
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(FollowEvery):
		}
		if len(q.VMs) == 0 {
			// New VMs start with their first record.
			all, err := s.VMs()
			if err != nil {
				return err
			}
			for _, vm := range all {
				if tails[vm] == nil {
					tails[vm] = &tail{day: dayOf(time.Now())}
				}
			}
		}
		for _, vm := range slices.Sorted(maps.Keys(tails)) {
			t := tails[vm]
			for {
				n, err := s.readFrom(vm, t.day, t.offset, func(r Record) {
					if q.Match(r) {
						each(r)
					}
				})
				if err != nil {
					return err
				}
				t.offset += n
				now := dayOf(time.Now())
				if t.day >= now {
					break
				}
				// The collector writes to a new day's file now; read the
				// rest of the old one, as it may still be plain.
				t.day, t.offset = nextDay(t.day), 0
			}
		}
	}
}

func nextDay(day string) string {
	t, err := time.Parse(dayLayout, day)
	if err != nil {
		return day
	}
	return t.AddDate(0, 0, 1).Format(dayLayout)
}

// readFrom reads the whole lines of the VM's file of day after offset in
// its records, and returns how many bytes it read.
func (s Store) readFrom(vm, day string, offset int64, each func(Record)) (int64, error) {
	var r io.Reader
	f, err := os.Open(filepath.Join(s.Dir(vm), day+plainExt))
	switch {
	case err == nil:
		if _, err := f.Seek(offset, io.SeekStart); err != nil {
			_ = f.Close()
			return 0, err
		}
		r = f
	case errors.Is(err, fs.ErrNotExist):
		// Gzipped since it was last read.
		if f, err = os.Open(filepath.Join(s.Dir(vm), day+gzipExt)); errors.Is(err, fs.ErrNotExist) {
			return 0, nil
		} else if err != nil {
			return 0, err
		}
		zr, err := gzip.NewReader(f)
		if err != nil {
			_ = f.Close()
			return 0, err
		}
		if _, err := io.CopyN(io.Discard, zr, offset); err != nil && !errors.Is(err, io.EOF) {
			_ = f.Close()
			return 0, err
		}
		r = zr
	default:
		return 0, err
	}
	defer func() { _ = f.Close() }()
	br := bufio.NewReaderSize(r, 64<<10)
	var n int64
	for {
		line, err := br.ReadBytes('\n')
		if errors.Is(err, io.EOF) {
			// A line without its newline is still being written.
			return n, nil
		}
		if err != nil {
			return n, err
		}
		n += int64(len(line))
		if r, ok := parseLine(line); ok {
			each(r)
		}
	}
}
