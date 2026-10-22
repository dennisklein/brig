// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

package fence2

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"os"
	"os/exec"
	"slices"
	"strings"
	"time"
)

// Window is how long the listener sums drops up before it writes them.
const Window = 5 * time.Second

// The Why of the drops that were counted but not itemised.
const (
	WhyOverRate   = "over the logging rate, not itemised"
	WhyCountsOnly = "counted only, not itemised"
)

// Listener reads the drops of the firewall in its network namespace, which
// package vmnet loaded in mode, and appends them to the spool. It runs
// beside passt in the VM's network namespace, as the net-helper's child.
type Listener struct {
	Spool Spool
	// Mode is what the firewall records, and Why why that is not
	// Itemised.
	Mode Mode
	Why  string
	// Counters reads the firewall's named counters: the packets dropped
	// by reason.
	Counters func() (map[string]uint64, error)
	// Parent is the process whose end ends the listener, zero for none:
	// the net-helper, which has become passt.
	Parent int
	Log    *slog.Logger

	// Window and Now are replaced by tests.
	Window time.Duration
	Now    func() time.Time
	// open opens the NFLOG group; tests replace it.
	open func() (packetSource, error)
}

// packetSource gives the packets the firewall logs.
type packetSource interface {
	read(timeout time.Duration, now func() time.Time) ([]Packet, error)
	close()
}

// Run writes the listener's state to the spool, tells ready, and then
// writes the drops of each window until ctx ends or the parent exits, when
// it writes the last window. ready may be nil.
func (l *Listener) Run(ctx context.Context, ready io.WriteCloser) error {
	now := l.Now
	if now == nil {
		now = time.Now
	}
	log := l.Log
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	state, why := l.Mode, l.Why
	var src packetSource
	if state == Itemised {
		open := l.open
		if open == nil {
			open = func() (packetSource, error) { return openNFLOG(Group) }
		}
		var err error
		if src, err = open(); err != nil {
			state, why = CountsOnly, "cannot read the drops the firewall logs: "+err.Error()
		}
	}
	defer func() {
		if src != nil {
			src.close()
		}
	}()
	err := l.Spool.Append(Entry{T: now(), State: state, Why: why})
	if ready != nil {
		_, _ = fmt.Fprintf(ready, "%s\n", state)
		_ = ready.Close()
	}
	if err != nil || state == Off {
		return err
	}
	if state != Itemised {
		log.Warn("fence 2 counts drops only", "why", why)
	}
	a := &aggregator{
		whyUncounted: WhyOverRate, windows: map[dropKey]*Entry{},
		itemised: map[string]uint64{}, reported: map[string]uint64{},
	}
	if state == CountsOnly {
		a.whyUncounted = WhyCountsOnly
	}
	window := cmp.Or(l.Window, Window)
	start := now()
	countersFailed := false
	flush := func() {
		counters, err := l.Counters()
		if err != nil && !countersFailed {
			log.Warn("cannot read the firewall's counters", "err", err)
		}
		countersFailed = err != nil
		if src != nil {
			// Whatever the counters counted and the firewall logged is
			// in the socket by now.
			pkts, _ := src.read(0, now)
			a.add(pkts)
		}
		end := now()
		if err := l.Spool.Append(a.flush(counters, start, end)...); err != nil {
			log.Error("cannot write the drops to the spool", "err", err)
		}
		start = end
	}
	for ctx.Err() == nil && (l.Parent == 0 || os.Getppid() == l.Parent) {
		wait := min(500*time.Millisecond, max(0, start.Add(window).Sub(now())))
		if src != nil {
			pkts, err := src.read(wait, now)
			if err != nil {
				log.Error("cannot read the drops the firewall logs", "err", err)
				src.close()
				src = nil
				_ = l.Spool.Append(Entry{T: now(), State: CountsOnly, Why: "reading the drops the firewall logs failed: " + err.Error()})
				a.whyUncounted = WhyCountsOnly
			}
			a.add(pkts)
		} else {
			select {
			case <-ctx.Done():
			case <-time.After(wait):
			}
		}
		if !now().Before(start.Add(window)) {
			flush()
		}
	}
	flush()
	return nil
}

// dropKey is what a window sums drops up by.
type dropKey struct {
	reason, proto, addr string
	port                int
}

// aggregator sums the drops of a window up, and makes up from the counters
// for those the firewall counted but did not log.
type aggregator struct {
	whyUncounted string
	windows      map[dropKey]*Entry
	// itemised are the drops read, and reported those written as not
	// itemised, by reason. The firewall's counters start at zero when it
	// is loaded, which is before the listener starts; the drops it
	// counted until then are written as not itemised.
	itemised, reported map[string]uint64
}

func (a *aggregator) add(pkts []Packet) {
	for _, p := range pkts {
		k := dropKey{p.Reason, p.Proto, p.Addr.String(), p.Port}
		e := a.windows[k]
		if e == nil {
			e = &Entry{T: p.At, Reason: p.Reason, Proto: p.Proto, Addr: k.addr, Port: p.Port}
			a.windows[k] = e
		}
		e.Hits++
		if p.At.Before(e.T) {
			e.T = p.At
		}
		if p.At.After(e.Last) {
			e.Last = p.At
		}
		a.itemised[p.Reason]++
	}
}

// flush returns the window's entries, from start to end, and starts the
// next. counters, nil when they could not be read, are the firewall's
// counters now: what they counted beyond what was itemised or written
// before becomes one entry per reason.
func (a *aggregator) flush(counters map[string]uint64, start, end time.Time) []Entry {
	out := make([]Entry, 0, len(a.windows))
	for _, e := range a.windows {
		if e.Last.Equal(e.T) {
			e.Last = time.Time{}
		}
		out = append(out, *e)
	}
	clear(a.windows)
	slices.SortFunc(out, func(x, y Entry) int {
		return cmp.Or(x.T.Compare(y.T), cmp.Compare(x.Reason, y.Reason), cmp.Compare(x.Addr, y.Addr), cmp.Compare(x.Port, y.Port))
	})
	for _, reason := range slices.Sorted(maps.Keys(counters)) {
		n := counters[reason]
		done := a.itemised[reason] + a.reported[reason]
		if n <= done {
			continue
		}
		a.reported[reason] += n - done
		out = append(out, Entry{T: start, Last: end, Reason: reason, Hits: int(n - done), Why: a.whyUncounted}) //nolint:gosec // a count
	}
	return out
}

// ReadCounters reads the named counters of brig's table with nft at path:
// the packets dropped by reason.
func ReadCounters(nft string) (map[string]uint64, error) {
	out, err := exec.Command(nft, "-j", "list", "counters", "table", "inet", "brig").Output() //nolint:gosec // G702: the nft that loaded the firewall
	if err != nil {
		var stderr string
		if ee := (*exec.ExitError)(nil); errors.As(err, &ee) {
			stderr = strings.TrimSpace(string(ee.Stderr))
		}
		return nil, fmt.Errorf("nft list counters: %w: %s", err, stderr)
	}
	return parseCounters(out)
}

// parseCounters reads the output of nft -j list counters.
func parseCounters(b []byte) (map[string]uint64, error) {
	var doc struct {
		Nftables []struct {
			Counter *struct {
				Name    string `json:"name"`
				Table   string `json:"table"`
				Packets uint64 `json:"packets"`
			} `json:"counter"`
		} `json:"nftables"`
	}
	if err := json.Unmarshal(b, &doc); err != nil {
		return nil, fmt.Errorf("reading nft's counters: %w", err)
	}
	out := map[string]uint64{}
	for _, o := range doc.Nftables {
		if c := o.Counter; c != nil && c.Table == "brig" && slices.Contains(Reasons, c.Name) {
			out[c.Name] = c.Packets
		}
	}
	return out, nil
}
