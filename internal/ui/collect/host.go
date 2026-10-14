// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

package collect

import (
	"context"
	"os"
	"time"

	"github.com/dennisklein/brig/internal/doctor"
	"github.com/dennisklein/brig/internal/openshell"
	"github.com/dennisklein/brig/internal/ui/store"
	"github.com/dennisklein/brig/internal/version"
)

// hostEvery is how often the host is described again, so that an upgraded
// openshell CLI shows without restarting brig ui.
const hostEvery = 30 * time.Second

// HostRun keeps the store's host description up to date until ctx ends.
func HostRun(ctx context.Context, st *store.Store, addr string) {
	hostRun(ctx, st, addr, hostEvery)
}

func hostRun(ctx context.Context, st *store.Store, addr string, every time.Duration) {
	tick := time.NewTicker(every)
	defer tick.Stop()
	for {
		if h := Host(ctx, addr); h != st.Snapshot().Host {
			st.SetHost(h)
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

// Host describes this host: its name, brig's version and the openshell
// CLI's, which it asks the CLI for.
func Host(ctx context.Context, addr string) store.Host {
	h := store.Host{BrigVersion: version.Version(), Addr: addr}
	h.Name, _ = os.Hostname()
	if cli, err := openshell.Find(); err == nil {
		ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		h.OpenShellCLI, _ = cli.Version(ctx)
	}
	return h
}

// Doctor runs brig doctor's checks and puts the outcome in the store.
func Doctor(ctx context.Context, st *store.Store) {
	results := doctor.Run(ctx, doctor.System())
	st.SetDoctor(&store.DoctorReport{Time: time.Now(), Results: results})
}
