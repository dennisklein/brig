// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

package collect

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/dennisklein/brig/internal/paths"
	"github.com/dennisklein/brig/internal/service"
	"github.com/dennisklein/brig/internal/ui/store"
	"github.com/dennisklein/brig/internal/vmrunner"
)

// spikesEvery is how often the vm runners are asked for their VMs.
const spikesEvery = 3 * time.Second

// Spikes keeps the store's VMs of the vm runner services up to date until
// ctx ends. It asks each service's controller through its admin socket in
// runtimeDir.
func Spikes(ctx context.Context, st *store.Store, dirs paths.Dirs, runtimeDir string) {
	tick := time.NewTicker(spikesEvery)
	defer tick.Stop()
	for {
		spikes, err := ReadSpikes(ctx, dirs, runtimeDir)
		msg := ""
		if err != nil {
			msg = err.Error()
		}
		st.SetSpikes(spikes, msg)
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

// ReadSpikes asks every vm runner service for its VMs.
func ReadSpikes(ctx context.Context, dirs paths.Dirs, runtimeDir string) ([]store.Spike, error) {
	svcs, err := service.NewStore(dirs).List()
	var errs []error
	if err != nil {
		errs = append(errs, err)
	}
	var out []store.Spike
	for _, svc := range svcs {
		if svc.Type != vmrunner.ServiceType {
			continue
		}
		cctx, cancel := context.WithTimeout(ctx, 2*time.Second)
		vms, err := vmrunner.NewAdminClient(vmrunner.AdminSocket(runtimeDir, svc.Name)).List(cctx)
		cancel()
		if err != nil {
			errs = append(errs, fmt.Errorf("service %s: %w", svc.Name, err))
			continue
		}
		for _, v := range vms {
			out = append(out, store.Spike{
				Service: svc.Name, ID: v.ID, Sandbox: v.Owner.String(), Image: v.Image, State: v.State,
				CPUs: v.CPUs, Memory: v.Memory, Disk: v.Disk, CreatedAt: v.CreatedAt, ExpiresAt: v.ExpiresAt,
				Snapshots: v.Snapshots,
			})
		}
	}
	slices.SortStableFunc(out, func(a, b store.Spike) int { return a.CreatedAt.Compare(b.CreatedAt) })
	return out, errors.Join(errs...)
}
