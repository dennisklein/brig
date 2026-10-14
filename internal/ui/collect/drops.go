// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

package collect

import (
	"context"
	"time"

	"github.com/dennisklein/brig/internal/audit"
	"github.com/dennisklein/brig/internal/ui/store"
)

const (
	// dropsBack is how far back the store's drops reach when brig ui
	// starts, as the live Traffic page shows the last hour.
	dropsBack = time.Hour
	// dropsRetry is how long Drops waits when the trail cannot be read.
	dropsRetry = 10 * time.Second
)

// Drops adds what fence 2 dropped, as the audit trail records it, to the
// store: the drops of the last hour, then each new one, until ctx ends.
func Drops(ctx context.Context, st *store.Store, s audit.Store) {
	q := audit.Query{Kinds: []string{audit.KindDrop}}
	byVM := map[string][]store.Drop{}
	_ = s.Read(audit.Query{Kinds: q.Kinds, Since: time.Now().Add(-dropsBack)}, func(r audit.Record) bool {
		byVM[r.VM] = append(byVM[r.VM], DropOf(r))
		return true
	})
	for vm, ds := range byVM {
		st.AddDrops(vm, ds)
	}
	// Follow reads from when it starts: what came in between, or while
	// the trail could not be read, is missing from the live view only.
	for {
		if err := s.Follow(ctx, q, func(r audit.Record) { st.AddDrops(r.VM, []store.Drop{DropOf(r)}) }); err == nil {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(dropsRetry):
		}
	}
}

// DropOf is a drop record of the audit trail as the store keeps it.
func DropOf(r audit.Record) store.Drop {
	return store.Drop{
		VM: r.VM, Time: r.T, Last: r.Last, Reason: r.Reason, Proto: r.Proto, Addr: r.Host, Port: r.Port, Hits: r.Hits, Why: r.Why,
	}
}
