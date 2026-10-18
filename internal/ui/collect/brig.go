// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

// Package collect gathers what brig ui shows from brig's own state: VM
// records and their domains, base images, the activity log and the host
// checks. It only reads; every change goes through a brig command.
package collect

import (
	"context"
	"errors"
	"fmt"
	"math"
	"path/filepath"
	"reflect"
	"time"

	"github.com/dennisklein/brig/internal/config"
	"github.com/dennisklein/brig/internal/image"
	"github.com/dennisklein/brig/internal/libvirt"
	"github.com/dennisklein/brig/internal/ocsync"
	"github.com/dennisklein/brig/internal/paths"
	"github.com/dennisklein/brig/internal/ui/store"
	"github.com/dennisklein/brig/internal/vm"
)

// brigEvery is how often VM records and domain states are read.
const brigEvery = 2 * time.Second

// Domains is what the collector asks libvirt.
type Domains interface {
	State(domain string) (libvirt.State, error)
	Stats(domains []string) (map[string]libvirt.Stats, error)
	Close() error
}

// Brig reads brig's records and the states and resource use of its domains.
type Brig struct {
	Dirs paths.Dirs
	// Connect connects to libvirt; the collector reconnects with it after
	// an error.
	Connect func(ctx context.Context) (Domains, error)

	// samples are the last resource samples by domain: a CPU share needs
	// two of them.
	samples map[string]libvirt.Stats
	// up is when each running domain was first seen to run after it was
	// seen not to; stopped holds the domains seen not to run since.
	up      map[string]time.Time
	stopped map[string]bool

	wake chan struct{}
}

// Refresh makes Run read everything again now, such as when a command that
// changes a VM has ended, instead of at the next poll.
func (b *Brig) Refresh() {
	select {
	case b.wake <- struct{}{}:
	default:
	}
}

// NewBrig returns a collector for the brig state in dirs.
func NewBrig(dirs paths.Dirs) *Brig {
	return &Brig{Dirs: dirs, Connect: func(ctx context.Context) (Domains, error) {
		return libvirt.Connect(ctx)
	}, wake: make(chan struct{}, 1)}
}

// Run keeps the store's VMs up to date until ctx ends.
func (b *Brig) Run(ctx context.Context, st *store.Store) {
	var conn Domains
	defer func() {
		if conn != nil {
			_ = conn.Close()
		}
	}()
	tick := time.NewTicker(brigEvery)
	defer tick.Stop()
	for {
		conn = b.Collect(ctx, st, conn)
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		case <-b.wake:
		}
	}
}

// Collect reads everything once into st, using conn or a new connection,
// and returns the connection to use next time, nil after an error.
func (b *Brig) Collect(ctx context.Context, st *store.Store, conn Domains) Domains {
	var errs []error
	cfg, err := config.Load(b.Dirs.ConfigFile())
	if err != nil {
		errs = append(errs, err)
		cfg = config.Builtin()
	}
	vms := vm.NewStore(b.Dirs)
	records, err := vms.List()
	if err != nil {
		errs = append(errs, err)
	}
	images := image.NewStore(b.Dirs)
	var bases []store.Image
	if imgs, err := images.List(); err == nil {
		bases = baseImages(imgs, records)
	}
	if conn == nil && len(records) > 0 {
		if conn, err = b.Connect(ctx); err != nil {
			errs = append(errs, fmt.Errorf("connecting to libvirt (try brig doctor): %w", err))
			conn = nil
		}
	}
	now := time.Now()
	out := make([]store.VM, len(records))
	var running []string
	for i, r := range records {
		v := store.VM{VM: *r, State: "unknown"}
		if conn != nil {
			s, err := conn.State(r.DomainName())
			if err != nil {
				errs = append(errs, err)
				_ = conn.Close()
				conn = nil
			} else {
				v.State = s.String()
			}
		}
		v.Up = b.upSince(r.DomainName(), v.State, now)
		if v.Running() {
			running = append(running, r.DomainName())
		}
		if img, err := images.Get(r.Image); err == nil {
			v.OpenShell = img.OpenShellVersion
		}
		if s, err := ocsync.LoadState(filepath.Join(vms.Dir(r.Name), ocsync.StateFile)); err == nil && s.LastSync != nil {
			v.LastSync = &store.SyncRecord{Time: s.LastSync.Time, Error: s.LastSync.Error}
		}
		out[i] = v
	}
	b.addStats(conn, running, out)
	var msg string
	if err := errors.Join(errs...); err != nil {
		msg = err.Error()
	}
	state := store.VMState{
		VMs: out, Err: msg, Images: bases, Profiles: cfg.NetworkProfiles,
		Config: store.Config{Defaults: cfg.Defaults, OpenShellConfigs: cfg.OpenShell.Configs},
	}
	if !sameState(st.Snapshot(), state) {
		st.SetVMs(state)
	}
	return conn
}

// upSince returns when the domain booted, as far as the collector saw it:
// the poll that found it running after it had found it stopped. It is zero
// for a domain that already ran when the collector started, because libvirt
// does not tell when a domain booted. A state the collector could not read
// changes nothing.
func (b *Brig) upSince(domain, state string, now time.Time) time.Time {
	if b.up == nil {
		b.up, b.stopped = map[string]time.Time{}, map[string]bool{}
	}
	switch state {
	case libvirt.StateRunning.String():
		if t, ok := b.up[domain]; ok {
			return t
		}
		if b.stopped[domain] {
			b.up[domain] = now
			return now
		}
		return time.Time{}
	case "unknown":
		return b.up[domain]
	}
	delete(b.up, domain)
	b.stopped[domain] = true
	return time.Time{}
}

// baseImages returns imgs, newest first, with the VMs that use each.
func baseImages(imgs []image.Image, vms []*vm.VM) []store.Image {
	var out []store.Image
	for _, img := range imgs {
		out = append(out, store.Image{})
		i := len(out) - 1
		out[i] = store.Image{ID: img.ID, Fedora: img.FedoraRelease, OpenShell: img.OpenShellVersion, CreatedAt: img.CreatedAt}
		for _, v := range vms {
			if v.Image == img.ID {
				out[i].UsedBy = append(out[i].UsedBy, v.Name)
			}
		}
	}
	return out
}

// addStats samples the resource use of the running domains and sets the
// usage numbers of their VMs in vms. Failing to sample leaves the numbers
// unknown; it is no error worth a message, and the next poll of the
// domains' states finds a lost connection.
func (b *Brig) addStats(conn Domains, running []string, vms []store.VM) {
	if conn == nil || len(running) == 0 {
		b.samples = nil
		return
	}
	samples, err := conn.Stats(running)
	if err != nil {
		b.samples = nil
		return
	}
	for i := range vms {
		s, ok := samples[vms[i].DomainName()]
		if !ok {
			continue
		}
		vms[i].Stats = usage(b.samples[vms[i].DomainName()], s, vms[i].CPUs)
	}
	b.samples = samples
}

// usage turns a domain's samples into the usage numbers of a VM with cpus
// vCPUs: the CPU share since the previous sample, if there is one from the
// same run, and the memory share. Shares are whole percent, so that a
// browser hears of a change only when the numbers it shows change.
func usage(prev, cur libvirt.Stats, cpus int) *store.Stats {
	var st store.Stats
	elapsed := cur.SampledAt.Sub(prev.SampledAt)
	if !prev.SampledAt.IsZero() && elapsed > 0 && cur.CPUTimeNS >= prev.CPUTimeNS {
		st.CPU = share(float64(cur.CPUTimeNS-prev.CPUTimeNS), float64(elapsed.Nanoseconds())*float64(max(cpus, 1)))
	}
	if m := cur.Memory; m != nil && m.TotalKiB > 0 {
		st.Memory = share(float64(m.UsedKiB), float64(m.TotalKiB))
		st.MemoryFromHost = m.Source == libvirt.MemoryFromHost
	}
	return &st
}

// share returns part of whole in whole percent, at most 100.
func share(part, whole float64) *float64 {
	p := math.Round(min(part/whole, 1) * 100)
	return &p
}

// sameState reports whether the snapshot already holds what was read, so
// that an unchanged poll sends no update to the browsers.
func sameState(sn store.Snapshot, v store.VMState) bool {
	return sn.VMsErr == v.Err && reflect.DeepEqual(sn.VMs, v.VMs) && reflect.DeepEqual(sn.Images, v.Images) &&
		reflect.DeepEqual(sn.Profiles, v.Profiles) && reflect.DeepEqual(sn.Config, v.Config)
}
