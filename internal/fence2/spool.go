// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

package fence2

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

// SpoolFile is the name of the spool in a VM's audit directory. The
// listener appends to it, the VM's audit collector reads it, and only the
// collector writes the audit trail.
const SpoolFile = "fence2.spool.jsonl"

// SpoolLimit is the size above which the spool is moved aside to
// SpoolFile.1, replacing an older copy, before the next append.
const SpoolLimit = 8 << 20

// Entry is a line of the spool: the listener's state, or the drops of a
// window, one entry per reason, protocol, address and port, and one per
// reason for the drops it counted but did not itemise.
type Entry struct {
	// T is when the state began, or the time of the window's first drop;
	// Last that of its last drop.
	T    time.Time `json:"t"`
	Last time.Time `json:"last,omitzero"`
	// State and Why are the listener's mode and why it is not Itemised,
	// on a state line.
	State Mode   `json:"state,omitempty"`
	Why   string `json:"why,omitempty"`
	// Reason, Proto, Addr and Port say what was dropped and why, and
	// Hits how often. Drops that were counted only have no Proto, Addr
	// and Port, and Why says why they are not itemised.
	Reason string `json:"reason,omitempty"`
	Proto  string `json:"proto,omitempty"`
	Addr   string `json:"addr,omitempty"`
	Port   int    `json:"port,omitempty"`
	Hits   int    `json:"hits,omitempty"`
}

// Spool appends entries to the spool at Path. One listener writes it.
type Spool struct {
	Path string
	// Limit is the size to rotate at, zero for SpoolLimit.
	Limit int64
}

// Append writes the entries as lines, in one write so that a reader never
// sees part of a line in a rotated copy.
func (s Spool) Append(es ...Entry) error {
	if len(es) == 0 {
		return nil
	}
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	for _, e := range es {
		if err := enc.Encode(e); err != nil {
			return err
		}
	}
	limit := s.Limit
	if limit == 0 {
		limit = SpoolLimit
	}
	if fi, err := os.Stat(s.Path); err == nil && fi.Size() > limit {
		if err := os.Rename(s.Path, s.Path+".1"); err != nil {
			return err
		}
	}
	if err := os.MkdirAll(filepath.Dir(s.Path), 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(s.Path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	_, err = f.Write(b.Bytes())
	return errors.Join(err, f.Close())
}

// Position is how far a reader has read the spool: the file it read last,
// by its inode and the time of its first entry, since a deleted file's
// inode is soon reused, and the offset after its last whole line.
type Position struct {
	Ino   uint64    `json:"ino,omitempty"`
	First time.Time `json:"first,omitzero"`
	Off   int64     `json:"off,omitempty"`
}

// spoolFile identifies a file of the spool, zero when it does not exist.
type spoolFile struct {
	ino   uint64
	first time.Time
	size  int64
}

// is reports whether the file is the one that pos was in. A file that
// nothing was read of yet may have had no whole first line.
func (f spoolFile) is(pos Position) bool {
	return f.ino != 0 && f.ino == pos.Ino && (pos.Off == 0 || f.first.Equal(pos.First))
}

// ReadSpool calls each with the entries of the spool at path after pos,
// those of its rotated copy first, and returns the position after them. It
// reports lost when the spool was rotated twice since pos, so that entries
// are missing. On an error, the position is still after the entries read.
func ReadSpool(path string, pos Position, each func(Entry)) (next Position, lost bool, err error) {
	cur, err := identify(path)
	if err != nil {
		return pos, false, err
	}
	old, err := identify(path + ".1")
	if err != nil {
		return pos, false, err
	}
	at := func(f spoolFile, off int64) Position { return Position{Ino: f.ino, First: f.first, Off: off} }
	switch {
	case cur.is(pos):
		off := pos.Off
		if cur.size < off {
			// Cut short by hand: read it from the start.
			off = 0
		}
		off, err = readLines(path, off, each)
		return at(cur, off), false, err
	case old.is(pos):
		// Rotated since: the rest of the copy, then the new spool.
		off, err := readLines(path+".1", pos.Off, each)
		if next = at(old, off); err != nil {
			return next, false, err
		}
	default:
		// Read for the first time, or rotated twice since.
		lost = pos.Ino != 0
		if old.ino != 0 {
			off, err := readLines(path+".1", 0, each)
			if next = at(old, off); err != nil {
				return next, lost, err
			}
		}
	}
	if cur.ino == 0 {
		return next, lost, nil
	}
	off, err := readLines(path, 0, each)
	return at(cur, off), lost, err
}

// identify returns what identifies the file at path.
func identify(path string) (spoolFile, error) {
	f, err := os.Open(path)
	if errors.Is(err, fs.ErrNotExist) {
		return spoolFile{}, nil
	}
	if err != nil {
		return spoolFile{}, err
	}
	defer func() { _ = f.Close() }()
	fi, err := f.Stat()
	if err != nil {
		return spoolFile{}, err
	}
	id := spoolFile{size: fi.Size()}
	if s, ok := fi.Sys().(*syscall.Stat_t); ok {
		id.ino = s.Ino
	}
	line, _ := bufio.NewReader(f).ReadBytes('\n')
	var e Entry
	if json.Unmarshal(line, &e) == nil {
		id.first = e.T
	}
	return id, nil
}

// readLines calls each with the entries of the whole lines of the file at
// path after off, and returns the offset after the last of them. Lines that
// are no entry are skipped.
func readLines(path string, off int64, each func(Entry)) (int64, error) {
	f, err := os.Open(path)
	if errors.Is(err, fs.ErrNotExist) {
		return off, nil
	}
	if err != nil {
		return off, err
	}
	defer func() { _ = f.Close() }()
	if _, err := f.Seek(off, io.SeekStart); err != nil {
		return off, err
	}
	br := bufio.NewReader(f)
	for {
		line, err := br.ReadBytes('\n')
		if errors.Is(err, io.EOF) {
			// A line without its newline is still being written.
			return off, nil
		}
		if err != nil {
			return off, err
		}
		off += int64(len(line))
		var e Entry
		if json.Unmarshal(line, &e) == nil && !e.T.IsZero() {
			each(e)
		}
	}
}
