// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

package collect

import (
	"context"
	"testing"
	"time"

	"github.com/dennisklein/brig/internal/audit"
	"github.com/dennisklein/brig/internal/ui/store"
)

func TestDrops(t *testing.T) {
	trail := audit.Store{Root: t.TempDir()}
	now := time.Now().UTC()
	add := func(rs ...audit.Record) {
		t.Helper()
		w, err := trail.OpenWriter("dev")
		if err != nil {
			t.Fatal(err)
		}
		w.Add(rs...)
		if err := w.Close(nil); err != nil {
			t.Fatal(err)
		}
	}
	drop := func(at time.Time, port int) audit.Record {
		return audit.Record{V: audit.Version, T: at, VM: "dev", Kind: audit.KindDrop, Reason: "lan", Proto: "tcp", Host: "192.168.1.20", Port: port, Hits: 1}
	}
	add(
		drop(now.Add(-2*time.Hour), 1),
		drop(now.Add(-time.Minute), 445),
		audit.Record{V: audit.Version, T: now.Add(-time.Minute), VM: "dev", Workspace: "default", Sandbox: "webapp", Kind: audit.KindRequest, Verdict: audit.VerdictAllow, Host: "nas.lan", Port: 445},
	)
	defer func(d time.Duration) { audit.FollowEvery = d }(audit.FollowEvery)
	audit.FollowEvery = 10 * time.Millisecond

	st := store.New()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { Drops(ctx, st, trail); close(done) }()
	defer func() { cancel(); <-done }()
	var ds []store.Drop
	for range 500 {
		if ds = st.Snapshot().TrafficOf("dev").Drops; len(ds) > 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if len(ds) != 1 || ds[0].Port != 445 || ds[0].Addr != "192.168.1.20" || ds[0].VM != "dev" {
		t.Errorf("the last hour's drops: %+v", ds)
	}
	// Follow starts reading after the last hour was read: what came in
	// between is missing, so add until a new drop shows.
	for range 100 {
		add(drop(now, 139))
		time.Sleep(20 * time.Millisecond)
		if ds := st.Snapshot().TrafficOf("dev").Drops; ds[len(ds)-1].Port == 139 {
			return
		}
	}
	t.Errorf("no new drop: %+v", st.Snapshot().TrafficOf("dev").Drops)
}
