// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

package ui

import (
	"cmp"
	"fmt"
	"slices"
	"strconv"
	"time"

	"github.com/dennisklein/brig/internal/audit"
	"github.com/dennisklein/brig/internal/config"
	"github.com/dennisklein/brig/internal/fence2"
	"github.com/dennisklein/brig/internal/ui/collect"
	"github.com/dennisklein/brig/internal/ui/store"
)

// dropWhat says what fence 2 dropped, such as "tcp 192.168.1.20:445 ×3".
func dropWhat(d store.Drop) string {
	if d.Target() == "" {
		return fmt.Sprintf("×%d packets", d.Hits)
	}
	s := d.Proto + " " + d.Target()
	if d.Hits > 1 {
		s += " ×" + strconv.Itoa(d.Hits)
	}
	return s
}

// dropAct is the button that opens the VM's network profile for a drop, or
// nil when no profile setting lets it through or the VM is gone.
func dropAct(v *view, d store.Drop) *actButton {
	if fence2.Setting(d.Reason) == "" {
		return nil
	}
	if _, ok := v.Snap.VM(d.VM); !ok {
		return nil
	}
	b := act(v, "profile", "Profile…", "sm", "vm", d.VM, "reason", d.Reason, "dest", d.Target(), "port", strconv.Itoa(d.Port))
	return &b
}

// profileLets reports whether the profile p lets through what fence 2
// dropped for reason, to port.
func profileLets(p config.NetworkProfile, reason string, port int) bool {
	switch reason {
	case fence2.ReasonInternet:
		return p.Internet
	case fence2.ReasonLAN:
		return p.LAN
	case fence2.ReasonHost:
		return p.Host
	case fence2.ReasonHostPort:
		return p.Host || slices.Contains(p.HostPorts, uint16(min(max(port, 0), 65535)))
	case fence2.ReasonIPv6:
		return p.IPv6
	}
	return false
}

// dropReason is the reason of the drop that the profile dialog was opened
// for, if it was, and the port it went to.
func (d *dialog) dropReason() (string, int) {
	reason := d.Form.Get("reason")
	if fence2.Setting(reason) == "" {
		return "", 0
	}
	port, _ := strconv.Atoi(d.Form.Get("port"))
	return reason, port
}

// profileDefaults picks the VM's profile, or, for a drop that it does not
// let through, the first profile that does.
func profileDefaults(d *dialog) {
	if reason, port := d.dropReason(); reason != "" && !profileLets(d.Snap.Profiles[d.VM.NetworkProfile], reason, port) {
		for _, name := range profileNames(d.Snap) {
			if profileLets(d.Snap.Profiles[name], reason, port) {
				d.def("profile", name)
				break
			}
		}
	}
	d.def("profile", d.VM.NetworkProfile)
}

// profileSub is the line below the profile dialog's title: the drop it was
// opened for, and the setting that lets it through.
func profileSub(d *dialog) string {
	reason, port := d.dropReason()
	if reason == "" {
		return ""
	}
	s := "Fence 2 dropped " + cmp.Or(d.Form.Get("dest"), "packets") + " from " + d.VM.Name + ": " + fence2.Explain(reason) + ". "
	setting := fence2.Setting(reason) + ": true"
	if reason == fence2.ReasonHostPort {
		setting = "host_ports: [" + strconv.Itoa(port) + "]"
	}
	lets := slices.ContainsFunc(profileNames(d.Snap), func(name string) bool { return profileLets(d.Snap.Profiles[name], reason, port) })
	if !lets {
		return s + "No profile in config.yaml has " + setting + " yet; add one under network_profiles first."
	}
	return s + "A profile with " + setting + " lets it through, from the next start of " + d.VM.Name + "."
}

// markDrops marks the allowed requests among rows that fence 2 likely
// dropped, from drops, by VM.
func markDrops(rows []trafficRow, drops func(vm string) store.Traffic) {
	for i, r := range rows {
		if r.Drop != nil || r.Verdict != store.VerdictAllow {
			continue
		}
		if d, ok := drops(r.VM).DropFor(r.Request, audit.HintBefore); ok {
			rows[i].Dropped = &d
		}
	}
}

// historyDrops reads the drops of the audit trail that may match the
// allowed requests among rows, by VM.
func historyDrops(h audit.Reader, rows []trafficRow) (func(vm string) store.Traffic, error) {
	var vms []string
	var first, last time.Time
	for _, r := range rows {
		if r.Verdict != store.VerdictAllow {
			continue
		}
		if !slices.Contains(vms, r.VM) {
			vms = append(vms, r.VM)
		}
		if first.IsZero() || r.Time.Before(first) {
			first = r.Time
		}
		last = maxTime(last, r.Time)
	}
	byVM := map[string]store.Traffic{}
	get := func(vm string) store.Traffic { return byVM[vm] }
	if len(vms) == 0 {
		return get, nil
	}
	// A drop's packets span a window of fence 2's listener at most.
	q := audit.Query{VMs: vms, Kinds: []string{audit.KindDrop}, Since: first.Add(-time.Minute), Until: last.Add(audit.HintBefore + time.Second)}
	err := h.Read(q, func(r audit.Record) bool {
		t := byVM[r.VM]
		t.Drops = append(t.Drops, collect.DropOf(r))
		byVM[r.VM] = t
		return true
	})
	return get, err
}

func maxTime(a, b time.Time) time.Time {
	if b.After(a) {
		return b
	}
	return a
}
