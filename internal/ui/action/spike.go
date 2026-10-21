// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

package action

import (
	"fmt"
	"slices"

	"github.com/dennisklein/brig/internal/ui/store"
)

func init() {
	register(Action{ID: "spike-destroy", Build: buildSpikeDestroy})
}

func buildSpikeDestroy(f Form, c Context) (Command, error) {
	svc, id := f.Get("service"), f.Get("id")
	i := slices.IndexFunc(c.Snap.Spikes, func(s store.Spike) bool { return s.Service == svc && s.ID == id })
	if i < 0 {
		return Command{}, fmt.Errorf("there is no VM %s of service %s", id, svc)
	}
	s := c.Snap.Spikes[i]
	return NewCommand("brig", "vm-runner", "destroy", "--service", s.Service, s.ID).
		Note("Powers the VM off and deletes its disk and snapshots. The agent of %s loses what it did there.", s.Sandbox), nil
}
