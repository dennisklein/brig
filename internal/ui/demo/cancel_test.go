// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

package demo

import (
	"errors"
	"testing"
	"time"

	"github.com/dennisklein/brig/internal/ui/action"
	"github.com/dennisklein/brig/internal/ui/run"
	"github.com/dennisklein/brig/internal/ui/store"
)

func TestCancelEndsTheCommand(t *testing.T) {
	st := store.New()
	h := New(st)
	id, err := h.Start(run.Spec{Command: action.NewCommand("brig", "stop", "dev")})
	if err != nil {
		t.Fatal(err)
	}
	if err := h.Cancel(id); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && h.Running() > 0 {
		time.Sleep(5 * time.Millisecond)
	}
	task, _ := st.Snapshot().Task(id)
	if !task.Done() || task.Status != store.TaskFailed || task.Err != "canceled" || !task.Canceling {
		t.Errorf("task: %+v", task)
	}
	var found bool
	for _, r := range st.Snapshot().Activity {
		if r.Trace == task.Trace {
			found = true
			if r.Status != "canceled" || !r.Done() {
				t.Errorf("run: %+v", r)
			}
		}
	}
	if !found {
		t.Error("the run is not in the activity log")
	}
	if err := h.Cancel(id); !errors.Is(err, run.ErrNotRunning) {
		t.Errorf("Cancel of a command that ended: %v, want ErrNotRunning", err)
	}
	h.Shutdown(time.Second)
}
