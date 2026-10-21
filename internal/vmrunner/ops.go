// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

package vmrunner

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/dennisklein/brig/internal/libvirt"
	"github.com/dennisklein/brig/internal/qga"
)

// agent adapts a Hypervisor's guest agent calls for one domain to qga.
type agent struct {
	hv     Hypervisor
	domain string
}

func (a agent) AgentCommand(cmd string, timeout time.Duration) (string, error) {
	return a.hv.AgentCommand(a.domain, cmd, timeout)
}

func (c *Controller) waitAgent(ctx context.Context, hv Hypervisor, domain string) error {
	return qga.WaitReady(ctx, agent{hv, domain}, c.poll)
}

// do runs f on owner o's VM id with a libvirt connection, holding the VM's
// lock shared, or exclusively for operations that change its state. ctx
// passed to f also ends when the VM is being destroyed.
func (c *Controller) do(ctx context.Context, o Owner, id string, exclusive bool, f func(context.Context, Hypervisor, *entry) error) error {
	e, err := c.get(o, id)
	if err != nil {
		return err
	}
	if exclusive {
		e.op.Lock()
		defer e.op.Unlock()
	} else {
		e.op.RLock()
		defer e.op.RUnlock()
	}
	if e.ctx.Err() != nil {
		return errNoVM
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	stop := context.AfterFunc(e.ctx, cancel)
	defer stop()
	hv, err := c.connect(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = hv.Close() }()
	return f(ctx, hv, e)
}

// VMInfo is a VM as vm_list shows it.
type VMInfo struct {
	ID        string    `json:"id"`
	Image     string    `json:"image"`
	State     string    `json:"state"`
	CPUs      int       `json:"cpus"`
	MemoryMiB uint64    `json:"memory_mib"`
	DiskGiB   float64   `json:"disk_gib"`
	CreatedAt time.Time `json:"created_at"`
	ExpiresAt time.Time `json:"expires_at"`
	Snapshots []string  `json:"snapshots"`
}

// ListOwned returns owner o's VMs with their states.
func (c *Controller) ListOwned(ctx context.Context, o Owner) ([]VMInfo, error) {
	c.mu.Lock()
	var recs []Record
	for _, e := range c.owned(o) {
		if e.ready {
			recs = append(recs, e.rec)
		}
	}
	c.mu.Unlock()
	slices.SortFunc(recs, func(a, b Record) int { return a.CreatedAt.Compare(b.CreatedAt) })
	out := make([]VMInfo, 0, len(recs))
	if len(recs) == 0 {
		return out, nil
	}
	hv, err := c.connect(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = hv.Close() }()
	for _, r := range recs {
		st, err := hv.State(r.DomainName())
		state := st.String()
		if err != nil {
			state = "unknown"
		}
		out = append(out, VMInfo{
			ID: r.ID, Image: r.Image, State: state, CPUs: r.CPUs, MemoryMiB: r.Memory.MiB(),
			DiskGiB: float64(r.Disk) / (1 << 30), CreatedAt: r.CreatedAt, ExpiresAt: r.ExpiresAt,
			Snapshots: append([]string{}, r.Snapshots...),
		})
	}
	return out, nil
}

// Start boots the VM if it is shut off, as after the guest powered off or
// the host restarted.
func (c *Controller) Start(ctx context.Context, o Owner, id string) error {
	return c.do(ctx, o, id, true, func(_ context.Context, hv Hypervisor, e *entry) error {
		st, err := hv.State(e.rec.DomainName())
		if err != nil {
			return err
		}
		if st == libvirt.StateRunning {
			return errors.New("the VM is running already")
		}
		e.setScreen(0, 0)
		return hv.Start(e.rec.DomainName())
	})
}

// Screenshot returns the VM's display as PNG and its size.
func (c *Controller) Screenshot(ctx context.Context, o Owner, id string) ([]byte, int, int, error) {
	var png []byte
	var w, h int
	err := c.do(ctx, o, id, false, func(_ context.Context, hv Hypervisor, e *entry) error {
		raw, mime, err := hv.Screenshot(e.rec.DomainName())
		if err != nil {
			return err
		}
		if png, w, h, err = toPNG(raw, mime); err != nil {
			return err
		}
		e.setScreen(w, h)
		return nil
	})
	return png, w, h, err
}

// Click moves the pointer to pixel x, y and presses button, twice if
// double; button "none" only moves the pointer.
func (c *Controller) Click(ctx context.Context, o Owner, id string, x, y int, btn string, double bool) error {
	if btn != "none" && !pointerButtons[btn] {
		return fmt.Errorf("unknown button %q: use left, right, middle, wheel-up, wheel-down or none", btn)
	}
	return c.do(ctx, o, id, false, func(ctx context.Context, hv Hypervisor, e *entry) error {
		w, h := e.screen()
		if w == 0 {
			// The pointer's axes span the display, so its size is
			// needed; a screenshot tells it.
			raw, mime, err := hv.Screenshot(e.rec.DomainName())
			if err != nil {
				return err
			}
			if _, w, h, err = toPNG(raw, mime); err != nil {
				return err
			}
			e.setScreen(w, h)
		}
		if x < 0 || y < 0 || x >= w || y >= h {
			return fmt.Errorf("point %d,%d is outside the %dx%d display", x, y, w, h)
		}
		send := func(events ...inputEvent) error {
			out, err := hv.MonitorCommand(e.rec.DomainName(), qmpInput(events...))
			if err == nil {
				err = qmpError(out)
			}
			return err
		}
		if err := send(moveTo(x, y, w, h)...); err != nil {
			return err
		}
		if btn == "none" {
			return nil
		}
		presses := 1
		if double {
			presses = 2
		}
		for range presses {
			if err := send(button(btn, true)); err != nil {
				return err
			}
			if err := sleep(ctx, 30*time.Millisecond); err != nil {
				return err
			}
			if err := send(button(btn, false)); err != nil {
				return err
			}
			if err := sleep(ctx, 60*time.Millisecond); err != nil {
				return err
			}
		}
		return nil
	})
}

func sleep(ctx context.Context, d time.Duration) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(d):
		return nil
	}
}

// keyHold is how long a key stays pressed.
const keyHold = 20 * time.Millisecond

// Type types text on a US keyboard layout.
func (c *Controller) Type(ctx context.Context, o Owner, id, text string) error {
	keys, err := typeKeys(text)
	if err != nil {
		return err
	}
	return c.press(ctx, o, id, keys)
}

// Keys presses key combinations such as "ctrl+alt+t", one after another.
func (c *Controller) Keys(ctx context.Context, o Owner, id string, combos []string) error {
	keys := make([][]uint32, 0, len(combos))
	for _, s := range combos {
		k, err := parseCombo(s)
		if err != nil {
			return err
		}
		keys = append(keys, k)
	}
	return c.press(ctx, o, id, keys)
}

func (c *Controller) press(ctx context.Context, o Owner, id string, keys [][]uint32) error {
	return c.do(ctx, o, id, false, func(ctx context.Context, hv Hypervisor, e *entry) error {
		for _, k := range keys {
			if err := hv.SendKeys(e.rec.DomainName(), k, keyHold); err != nil {
				return err
			}
			if err := sleep(ctx, keyHold); err != nil {
				return err
			}
		}
		return nil
	})
}

// Exec runs a command in the VM through the guest agent.
func (c *Controller) Exec(ctx context.Context, o Owner, id string, r qga.ExecRequest) (qga.ExecResult, error) {
	var res qga.ExecResult
	err := c.do(ctx, o, id, false, func(ctx context.Context, hv Hypervisor, e *entry) error {
		var err error
		res, err = qga.Exec(ctx, agent{hv, e.rec.DomainName()}, r)
		return err
	})
	return res, err
}

// CopyIn writes data to the VM's file path, or appends it.
func (c *Controller) CopyIn(ctx context.Context, o Owner, id, path string, data []byte, appendTo bool) error {
	return c.do(ctx, o, id, false, func(_ context.Context, hv Hypervisor, e *entry) error {
		return qga.WriteFile(agent{hv, e.rec.DomainName()}, path, data, appendTo)
	})
}

// CopyOut reads at most limit bytes of the VM's file path from offset on.
func (c *Controller) CopyOut(ctx context.Context, o Owner, id, path string, offset int64, limit int) ([]byte, bool, error) {
	var data []byte
	var eof bool
	err := c.do(ctx, o, id, false, func(_ context.Context, hv Hypervisor, e *entry) error {
		var err error
		data, eof, err = qga.ReadFile(agent{hv, e.rec.DomainName()}, path, offset, limit)
		return err
	})
	return data, eof, err
}

// Snapshot takes a snapshot of the VM's disk and memory.
func (c *Controller) Snapshot(ctx context.Context, o Owner, id, name string) error {
	return c.do(ctx, o, id, true, func(_ context.Context, hv Hypervisor, e *entry) error {
		if slices.Contains(e.rec.Snapshots, name) {
			return fmt.Errorf("the VM has a snapshot %q already", name)
		}
		if n := c.cfg.Quota.Snapshots; len(e.rec.Snapshots) >= n {
			return fmt.Errorf("quota: a VM may have %d snapshots; vm_snapshot_delete one first", n)
		}
		if err := hv.CreateSnapshot(e.rec.DomainName(), name); err != nil {
			return err
		}
		return c.updateRecord(e, func(r *Record) { r.Snapshots = append(r.Snapshots, name) })
	})
}

// Restore returns the VM to a snapshot.
func (c *Controller) Restore(ctx context.Context, o Owner, id, name string) error {
	return c.do(ctx, o, id, true, func(_ context.Context, hv Hypervisor, e *entry) error {
		if !slices.Contains(e.rec.Snapshots, name) {
			return fmt.Errorf("the VM has no snapshot %q", name)
		}
		if err := hv.RevertSnapshot(e.rec.DomainName(), name); err != nil {
			return err
		}
		e.setScreen(0, 0)
		return nil
	})
}

// DeleteSnapshot deletes a snapshot of the VM.
func (c *Controller) DeleteSnapshot(ctx context.Context, o Owner, id, name string) error {
	return c.do(ctx, o, id, true, func(_ context.Context, hv Hypervisor, e *entry) error {
		if !slices.Contains(e.rec.Snapshots, name) {
			return fmt.Errorf("the VM has no snapshot %q", name)
		}
		if err := hv.DeleteSnapshot(e.rec.DomainName(), name); err != nil {
			return err
		}
		return c.updateRecord(e, func(r *Record) {
			r.Snapshots = slices.DeleteFunc(r.Snapshots, func(s string) bool { return s == name })
		})
	})
}

// updateRecord changes the VM's record and saves it.
func (c *Controller) updateRecord(e *entry, change func(*Record)) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	r := e.rec
	r.Snapshots = slices.Clone(r.Snapshots)
	change(&r)
	if err := c.saveRecord(&r); err != nil {
		return err
	}
	e.rec = r
	return nil
}

var _ Hypervisor = (*libvirt.Conn)(nil)
