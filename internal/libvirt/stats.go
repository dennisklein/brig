// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

package libvirt

import (
	"fmt"
	"slices"
	"time"

	golibvirt "github.com/digitalocean/go-libvirt"
)

// statsPeriod is the interval in seconds at which the guest's balloon driver
// reports its memory statistics to QEMU. Without it, libvirt cannot tell how
// much memory the guest uses.
const statsPeriod = 5

// Memory sources: whose view of the memory use a MemoryStats reports.
const (
	// MemoryFromGuest means that the guest's balloon driver reported it.
	MemoryFromGuest = "guest"
	// MemoryFromHost means that it is the QEMU process's resident set, which
	// includes memory that the guest touched once and no longer uses.
	MemoryFromHost = "host"
)

// Stats is a sample of the resource use of a running domain.
type Stats struct {
	// SampledAt is when the sample was taken.
	SampledAt time.Time `json:"sampled_at"`
	// CPUTimeNS is the CPU time the domain has used since it started, in
	// nanoseconds, summed over its virtual CPUs. A rate needs two samples.
	CPUTimeNS uint64 `json:"cpu_time_ns"`
	// Memory is nil if libvirt reported nothing about the memory.
	Memory *MemoryStats `json:"memory,omitempty"`
}

// MemoryStats is the memory use of a domain.
type MemoryStats struct {
	UsedKiB  uint64 `json:"used_kib"`
	TotalKiB uint64 `json:"total_kib"`
	// Source is MemoryFromGuest or MemoryFromHost.
	Source string `json:"source"`
}

// Stats samples the resource use of the named domains in one call. Domains
// that are not running, or that libvirt does not know, have no entry. Names
// of other domains are ignored.
//
// A domain that does not report the guest's view of its memory, because it
// was defined without a statistics period, gets one set for as long as the
// connection lives; it applies on the next sample.
func (c *Conn) Stats(names []string) (map[string]Stats, error) {
	if len(names) == 0 {
		return nil, nil
	}
	recs, err := c.c.ConnectGetAllDomainStats(nil,
		uint32(golibvirt.DomainStatsBalloon|golibvirt.DomainStatsCPUTotal),
		uint32(golibvirt.ConnectGetAllDomainsStatsRunning))
	if err != nil {
		return nil, fmt.Errorf("get domain stats: %w", err)
	}
	now := time.Now()
	out := make(map[string]Stats)
	for _, r := range recs {
		if !slices.Contains(names, r.Dom.Name) {
			continue
		}
		s, guest := parseStats(r.Params)
		s.SampledAt = now
		out[r.Dom.Name] = s
		if !guest {
			c.enableGuestStats(r.Dom)
		}
	}
	return out, nil
}

// enableGuestStats makes the balloon of dom report the guest's memory
// statistics. Failure is not an error: the host's view remains.
func (c *Conn) enableGuestStats(dom golibvirt.Domain) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.statsSet[dom.Name] {
		return
	}
	if c.statsSet == nil {
		c.statsSet = map[string]bool{}
	}
	c.statsSet[dom.Name] = true
	_ = c.c.DomainSetMemoryStatsPeriod(dom, statsPeriod, golibvirt.DomainMemLive|golibvirt.DomainMemConfig)
}

// parseStats reads the typed parameters of one domain. It reports whether
// they hold the guest's view of the memory.
func parseStats(params []golibvirt.TypedParam) (s Stats, guest bool) {
	v := map[string]uint64{}
	for _, p := range params {
		if n, ok := uintOf(p.Value); ok {
			v[p.Field] = n
		}
	}
	s.CPUTimeNS = v["cpu.time"]
	avail, hasAvail := v["balloon.available"]
	unused, hasUnused := v["balloon.unused"]
	rss, hasRSS := v["balloon.rss"]
	switch {
	// available counts the memory the guest has, unused what it could hand
	// out without swapping. A guest that has not yet reported leaves both at
	// zero or absent.
	case hasAvail && hasUnused && avail > 0 && unused <= avail:
		s.Memory = &MemoryStats{UsedKiB: avail - unused, TotalKiB: avail, Source: MemoryFromGuest}
		return s, true
	case hasRSS:
		s.Memory = &MemoryStats{UsedKiB: rss, TotalKiB: v["balloon.current"], Source: MemoryFromHost}
	}
	return s, false
}

// uintOf returns the value of an unsigned or non-negative signed parameter.
func uintOf(v golibvirt.TypedParamValue) (uint64, bool) {
	switch n := v.I.(type) {
	case uint64:
		return n, true
	case uint32:
		return uint64(n), true
	case int64:
		if n >= 0 {
			return uint64(n), true
		}
	case int32:
		if n >= 0 {
			return uint64(n), true
		}
	}
	return 0, false
}
