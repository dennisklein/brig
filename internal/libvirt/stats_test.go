// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

package libvirt

import (
	"errors"
	"fmt"
	"testing"
	"time"

	golibvirt "github.com/digitalocean/go-libvirt"
)

func ull(field string, v uint64) golibvirt.TypedParam {
	return golibvirt.TypedParam{Field: field, Value: *golibvirt.NewTypedParamValueUllong(v)}
}

func TestParseStats(t *testing.T) {
	for _, tc := range []struct {
		name   string
		params []golibvirt.TypedParam
		cpu    uint64
		mem    *MemoryStats
		guest  bool
	}{
		{
			name: "guest view",
			params: []golibvirt.TypedParam{
				ull("cpu.time", 42), ull("balloon.current", 4096), ull("balloon.rss", 3000),
				ull("balloon.available", 3900), ull("balloon.unused", 900),
			},
			cpu:   42,
			mem:   &MemoryStats{UsedKiB: 3000, TotalKiB: 3900, Source: MemoryFromGuest},
			guest: true,
		},
		{
			name:   "no statistics period",
			params: []golibvirt.TypedParam{ull("cpu.time", 7), ull("balloon.current", 4096), ull("balloon.rss", 3000)},
			cpu:    7,
			mem:    &MemoryStats{UsedKiB: 3000, TotalKiB: 4096, Source: MemoryFromHost},
		},
		{
			name: "the driver has not reported yet",
			params: []golibvirt.TypedParam{
				ull("balloon.current", 4096), ull("balloon.rss", 100), ull("balloon.available", 0), ull("balloon.unused", 0),
			},
			mem: &MemoryStats{UsedKiB: 100, TotalKiB: 4096, Source: MemoryFromHost},
		},
		{
			name: "unused exceeds available",
			params: []golibvirt.TypedParam{
				ull("balloon.rss", 100), ull("balloon.available", 10), ull("balloon.unused", 20),
			},
			mem: &MemoryStats{UsedKiB: 100, Source: MemoryFromHost},
		},
		{name: "no balloon", params: []golibvirt.TypedParam{ull("cpu.time", 5)}, cpu: 5},
		{
			name: "other integer types",
			params: []golibvirt.TypedParam{
				{Field: "balloon.rss", Value: *golibvirt.NewTypedParamValueUint(8)},
				{Field: "balloon.current", Value: *golibvirt.NewTypedParamValueLlong(16)},
				{Field: "cpu.time", Value: *golibvirt.NewTypedParamValueLlong(-1)},
				{Field: "balloon.maximum", Value: *golibvirt.NewTypedParamValueString("x")},
			},
			mem: &MemoryStats{UsedKiB: 8, TotalKiB: 16, Source: MemoryFromHost},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, guest := parseStats(tc.params)
			if s.CPUTimeNS != tc.cpu || guest != tc.guest {
				t.Errorf("cpu = %d, guest = %v; want %d, %v", s.CPUTimeNS, guest, tc.cpu, tc.guest)
			}
			switch {
			case tc.mem == nil && s.Memory != nil, tc.mem != nil && (s.Memory == nil || *s.Memory != *tc.mem):
				t.Errorf("memory = %+v, want %+v", s.Memory, tc.mem)
			}
		})
	}
}

func TestStats(t *testing.T) {
	a, b := running(-1), running(-1)
	a.stats = []golibvirt.TypedParam{ull("cpu.time", 1), ull("balloon.rss", 10)}
	b.stats = []golibvirt.TypedParam{
		ull("cpu.time", 2), ull("balloon.available", 100), ull("balloon.unused", 40),
	}
	c, f := newFake(map[string]*fakeDomain{
		"brig-a": a,
		"brig-b": b,
		"brig-c": {state: golibvirt.DomainShutoff},
		"other":  running(-1),
	})
	before := time.Now()
	got, err := c.Stats([]string{"brig-a", "brig-b", "brig-c", "brig-missing"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("Stats = %+v, want brig-a and brig-b only", got)
	}
	if s := got["brig-a"]; s.CPUTimeNS != 1 || s.Memory == nil || s.Memory.Source != MemoryFromHost || s.SampledAt.Before(before) {
		t.Errorf("brig-a = %+v", s)
	}
	if s := got["brig-b"]; s.Memory == nil || *s.Memory != (MemoryStats{UsedKiB: 60, TotalKiB: 100, Source: MemoryFromGuest}) {
		t.Errorf("brig-b = %+v", s.Memory)
	}
	// A second sample asks libvirt again but does not repeat the period.
	if _, err := c.Stats([]string{"brig-a"}); err != nil {
		t.Fatal(err)
	}
	all := fmt.Sprintf("domstats 0 %d %d", golibvirt.DomainStatsBalloon|golibvirt.DomainStatsCPUTotal, golibvirt.ConnectGetAllDomainsStatsRunning)
	live := golibvirt.DomainMemLive | golibvirt.DomainMemConfig
	checkCalls(t, f, all, fmt.Sprintf("statsperiod brig-a 5 %d", live), all)
}

func TestStatsWithoutDomains(t *testing.T) {
	c, f := newFake(nil)
	if got, err := c.Stats(nil); err != nil || got != nil {
		t.Errorf("Stats(nil) = %v, %v", got, err)
	}
	checkCalls(t, f)
	f.statsErr = errors.New("boom")
	if _, err := c.Stats([]string{"brig-a"}); err == nil {
		t.Error("Stats succeeded despite the error")
	}
}
