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
