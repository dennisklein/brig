// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

package action

import (
	"errors"
	"fmt"
	"time"

	"github.com/dennisklein/brig/internal/bytesize"
	"github.com/dennisklein/brig/internal/ui/store"
	"github.com/dennisklein/brig/internal/vm"
)

func init() {
	register(Action{ID: "audit-prune", Subject: subjectVM, Build: buildAuditPrune, Typed: subjectVM})
}

// PruneAges are the ages that the Prune dialog offers, as brig audit prune
// --before takes them.
var PruneAges = []string{"7d", "30d", "90d", "180d", "365d"}

// pruneDays is the number of days of each of PruneAges.
var pruneDays = map[string]int{"7d": 7, "30d": 30, "90d": 90, "180d": 180, "365d": 365}

// now is the present; tests replace it.
var now = time.Now

func buildAuditPrune(f Form, c Context) (Command, error) {
	name := f.Get("vm")
	if err := vm.ValidateName(name); err != nil {
		return Command{}, err
	}
	a, ok := c.Snap.Audit.AuditOf(name)
	if !ok || len(a.Days) == 0 {
		return Command{}, fmt.Errorf("%s has no audit history", name)
	}
	if f.Get("what") == "deleted" {
		if _, exists := c.Snap.VM(name); exists {
			return Command{}, fmt.Errorf("%s still exists; delete its history by age", name)
		}
		return NewCommand("brig", "audit", "prune", name, "--deleted-vms").
			Note("Deletes the whole history of the deleted VM %s: %d records, %s.", name, a.Rows, size(a.Disk)), nil
	}
	age := f.Get("before")
	gone, cutoff, ok := PruneGone(a, age)
	if !ok {
		return Command{}, errors.New("before: pick an age")
	}
	if gone.Rows == 0 {
		return Command{}, fmt.Errorf("%s has no history from before %s", name, cutoff)
	}
	return NewCommand("brig", "audit", "prune", name, "--before", age).
		Note("Deletes the %d records written before %s, freeing %s. Nothing can get them back.", gone.Rows, cutoff, size(gone.Disk)), nil
}

// PruneGone returns what pruning a's history before age, one of PruneAges,
// deletes, and the day it deletes the records before.
func PruneGone(a store.AuditVM, age string) (gone store.AuditPart, cutoff string, ok bool) {
	days, ok := pruneDays[age]
	if !ok {
		return gone, "", false
	}
	cutoff = now().UTC().AddDate(0, 0, -days).Format(time.DateOnly)
	for _, d := range a.Days {
		if d.Name < cutoff {
			gone.Rows += d.Rows
			gone.Disk += d.Disk
		}
	}
	return gone, cutoff, true
}

func size(n int64) string { return bytesize.Size(max(n, 0)).Approx() }
