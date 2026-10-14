// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

package audit

import (
	"fmt"
	"net"
	"strconv"
	"strings"

	"github.com/dennisklein/brig/internal/fence2"
)

// Describe says in one line what the record tells, without its time, VM
// and sandbox.
func Describe(r Record) string {
	switch r.Kind {
	case KindRequest:
		return describeRequest(r)
	case KindDrop:
		return describeDrop(r)
	case KindGap:
		return "gap: " + r.Why
	case KindSandbox:
		if r.Event == EventDeleted {
			return "sandbox deleted"
		}
		s := "sandbox created"
		if r.Image != "" {
			s += " from " + r.Image
		}
		if len(r.Providers) > 0 {
			s += " with providers " + strings.Join(r.Providers, ", ")
		}
		return s
	case KindPolicy:
		return describePolicy(r)
	case KindDraft:
		s := "draft " + r.Chunk + " " + r.Status
		if r.Rule != "" {
			s += ": rule " + r.Rule
		}
		if len(r.Endpoints) > 0 {
			s += " for " + strings.Join(r.Endpoints, ", ")
		}
		if r.Binary != "" {
			s += " by " + r.Binary
		}
		if r.Hits > 0 {
			s += fmt.Sprintf(" (%d hits)", r.Hits)
		}
		return s
	case KindAction:
		s := r.Command
		switch {
		case r.Error != "":
			s += " (" + r.Status + ": " + r.Error + ")"
		case r.Status != "":
			s += " (" + r.Status + ")"
		}
		return s
	}
	return r.Kind
}

func describeRequest(r Record) string {
	if r.Raw != "" {
		return "unreadable log line: " + r.Raw
	}
	verdict := r.Verdict
	if verdict == "" {
		verdict = "?"
	}
	parts := []string{verdict}
	if r.Method != "" {
		parts = append(parts, r.Method)
	}
	target := r.Host
	if r.Port != 0 {
		target += ":" + strconv.Itoa(r.Port)
	}
	parts = append(parts, target+r.Path)
	if r.Binary != "" {
		by := "by " + r.Binary
		if r.PID != 0 {
			by += " (pid " + strconv.Itoa(r.PID) + ")"
		}
		parts = append(parts, by)
	}
	s := strings.Join(parts, " ")
	switch {
	case r.Rule != "" && r.Verdict == VerdictAllow:
		s += ", rule " + r.Rule
	case r.Reason != "":
		s += ": " + r.Reason
	}
	return s
}

func describeDrop(r Record) string {
	s := "drop"
	if r.Proto != "" {
		s += " " + r.Proto
	}
	if target := DropTarget(r); target != "" {
		s += " " + target
	}
	if r.Hits > 1 || r.Host == "" {
		s += fmt.Sprintf(" ×%d", r.Hits)
	}
	s += ": " + fence2.Explain(r.Reason)
	if r.Why != "" {
		s += " (" + r.Why + ")"
	}
	return s
}

// DropTarget is where a drop went, as address:port, [address]:port or the
// address alone; empty for drops that were counted only.
func DropTarget(r Record) string {
	if r.Host == "" || r.Port == 0 {
		return r.Host
	}
	return net.JoinHostPort(r.Host, strconv.Itoa(r.Port))
}

func describePolicy(r Record) string {
	s := "policy v" + strconv.FormatUint(uint64(r.Version), 10)
	if r.Event == EventStatus {
		if r.Error != "" {
			return s + " " + r.Status + ": " + r.Error
		}
		return s + " " + r.Status
	}
	s += " (" + r.Status + ")"
	var changes []string
	for _, k := range r.Added {
		changes = append(changes, "+"+k)
	}
	for _, k := range r.Removed {
		changes = append(changes, "-"+k)
	}
	for _, k := range r.Changed {
		changes = append(changes, "~"+k)
	}
	if len(changes) > 0 {
		s += " " + strings.Join(changes, " ")
	}
	if r.Error != "" {
		s += ": " + r.Error
	}
	if r.By != "" {
		s += "; by " + r.By
	}
	return s
}
