// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

package audit

import (
	"bufio"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

// PruneSpec says what history to delete. Before and Sandbox can be
// combined; DeletedVMs stands alone.
type PruneSpec struct {
	// VMs are the VMs to prune; none prunes every VM.
	VMs []string
	// Before deletes the days written before its date.
	Before time.Time
	// Workspace and Sandbox delete one sandbox's records.
	Workspace, Sandbox string
	// DeletedVMs deletes the whole history of VMs that no longer exist.
	DeletedVMs bool
}

// PruneItem is part of a VM's history that prune deletes: a day, a
// sandbox's records of a day, or the whole history.
type PruneItem struct {
	VM string `json:"vm"`
	// Day is the date of the day, empty for the whole history.
	Day string `json:"day,omitempty"`
	// Sandbox is WORKSPACE/NAME when only its records go.
	Sandbox string `json:"sandbox,omitempty"`
	Rows    int64  `json:"rows"`
	// Disk is how much space deleting it frees, estimated for a sandbox's
	// records in a gzipped day.
	Disk int64 `json:"disk"`
}

// PrunePlan is what a prune deletes.
type PrunePlan struct {
	Items []PruneItem `json:"items"`
	Rows  int64       `json:"rows"`
	Disk  int64       `json:"disk"`
}

// PlanPrune works out what spec deletes; exists reports whether brig still
// has a VM.
func (s Store) PlanPrune(spec PruneSpec, exists func(vm string) bool) (PrunePlan, error) {
	var plan PrunePlan
	vms := spec.VMs
	if len(vms) == 0 {
		var err error
		if vms, err = s.VMs(); err != nil {
			return plan, err
		}
	}
	add := func(it PruneItem) {
		plan.Items = append(plan.Items, it)
		plan.Rows += it.Rows
		plan.Disk += it.Disk
	}
	for _, vm := range vms {
		x, err := s.ReadIndex(vm)
		if err != nil {
			return plan, err
		}
		if spec.DeletedVMs {
			if exists(vm) || s.Collecting(vm) {
				continue
			}
			it := PruneItem{VM: vm, Disk: x.Disk()}
			for _, d := range x.Days {
				it.Rows += d.Rows()
			}
			add(it)
			continue
		}
		before := ""
		if !spec.Before.IsZero() {
			before = spec.Before.UTC().Format(dayLayout)
		}
		key := ""
		if spec.Sandbox != "" {
			key = spec.Workspace + "/" + spec.Sandbox
		}
		for _, name := range x.DayNames() {
			d := x.Days[name]
			if before != "" && name >= before {
				continue
			}
			if key == "" {
				add(PruneItem{VM: vm, Day: name, Rows: d.Rows(), Disk: d.Disk})
				continue
			}
			var c Count
			for _, kc := range d.Counts[key] {
				c.Rows += kc.Rows
				c.Bytes += kc.Bytes
			}
			if c.Rows == 0 {
				continue
			}
			disk := c.Bytes
			if d.Bytes > 0 {
				disk = c.Bytes * d.Disk / d.Bytes
			}
			add(PruneItem{VM: vm, Day: name, Sandbox: key, Rows: c.Rows, Disk: disk})
		}
	}
	return plan, nil
}

// Prune deletes what the plan names.
func (s Store) Prune(plan PrunePlan) error {
	byVM := map[string][]PruneItem{}
	var order []string
	for _, it := range plan.Items {
		if byVM[it.VM] == nil {
			order = append(order, it.VM)
		}
		byVM[it.VM] = append(byVM[it.VM], it)
	}
	var errs []error
	for _, vm := range order {
		errs = append(errs, s.pruneVM(vm, byVM[vm]))
	}
	return errors.Join(errs...)
}

func (s Store) pruneVM(vm string, items []PruneItem) error {
	if slices.ContainsFunc(items, func(it PruneItem) bool { return it.Day == "" }) {
		if s.Collecting(vm) {
			return fmt.Errorf("a collector writes the audit trail of VM %s", vm)
		}
		return os.RemoveAll(s.Dir(vm))
	}
	unlock, err := s.lock(vm, writeLock, true)
	if err != nil {
		return err
	}
	defer unlock()
	x, err := s.ReadIndex(vm)
	if err != nil {
		return err
	}
	var errs []error
	for _, it := range items {
		d := x.Days[it.Day]
		if d == nil {
			continue
		}
		if it.Sandbox == "" {
			for _, ext := range []string{plainExt, gzipExt} {
				if err := os.Remove(filepath.Join(s.Dir(vm), it.Day+ext)); err != nil && !errors.Is(err, os.ErrNotExist) {
					errs = append(errs, err)
				}
			}
			delete(x.Days, it.Day)
			continue
		}
		size, err := s.dropSandbox(vm, it.Day, it.Sandbox)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		for _, c := range d.Counts[it.Sandbox] {
			d.Bytes -= c.Bytes
		}
		delete(d.Counts, it.Sandbox)
		d.Disk = size
	}
	// Forget the sandboxes that have no records left.
	left := map[string]bool{}
	for _, d := range x.Days {
		for k := range d.Counts {
			left[k] = true
		}
	}
	for k := range x.Sandboxes {
		if !left[k] {
			delete(x.Sandboxes, k)
		}
	}
	errs = append(errs, s.writeIndex(x))
	return errors.Join(errs...)
}

// dropSandbox rewrites the VM's file of day without the sandbox's records,
// and returns its new size.
func (s Store) dropSandbox(vm, day, key string) (int64, error) {
	p := s.dayPath(vm, day)
	if p == "" {
		return 0, nil
	}
	gz := strings.HasSuffix(p, gzipExt)
	in, err := os.Open(p)
	if err != nil {
		return 0, err
	}
	defer func() { _ = in.Close() }()
	var r io.Reader = in
	if gz {
		zr, err := gzip.NewReader(in)
		if err != nil {
			return 0, fmt.Errorf("%s: %w", p, err)
		}
		r = zr
	}
	tmp := p + ".tmp"
	out, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return 0, err
	}
	var w io.Writer = out
	var zw *gzip.Writer
	if gz {
		zw = gzip.NewWriter(out)
		w = zw
	}
	bw := bufio.NewWriter(w)
	br := bufio.NewReaderSize(r, 64<<10)
	for err == nil {
		var line []byte
		line, err = br.ReadBytes('\n')
		if len(line) == 0 {
			continue
		}
		if rec, ok := parseLine(line); ok && rec.SandboxKey() == key {
			continue
		}
		_, werr := bw.Write(line)
		if werr != nil {
			err = werr
		}
	}
	if errors.Is(err, io.EOF) {
		err = nil
	}
	err = errors.Join(err, bw.Flush())
	if zw != nil {
		err = errors.Join(err, zw.Close())
	}
	err = errors.Join(err, out.Close())
	if err == nil {
		err = os.Rename(tmp, p)
	}
	if err != nil {
		_ = os.Remove(tmp)
		return 0, err
	}
	fi, err := os.Stat(p)
	if err != nil {
		return 0, err
	}
	return fi.Size(), nil
}
