// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

package demo

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/dennisklein/brig/internal/bytesize"
	"github.com/dennisklein/brig/internal/ui/run"
	"github.com/dennisklein/brig/internal/ui/store"
	"github.com/dennisklein/brig/internal/vm"
)

const (
	// stepEvery is how long each step of a made-up command takes.
	stepEvery = 700 * time.Millisecond
	// keepOK is how long a made-up command that succeeded stays among the
	// task cards.
	keepOK = 8 * time.Second
)

// step is a step of a made-up command. A step with a skip reason is skipped.
type step struct{ name, skip string }

// plan is what a made-up command does.
type plan struct {
	steps []step
	// output is what the command prints.
	output string
	// fail makes the command fail at its last step with this error.
	fail string
	// apply changes the demo host when the command succeeds.
	apply func(*store.VMState)
	// gw changes the gateway of the VM gwVM when the command succeeds.
	gw   func(*store.Gateway)
	gwVM string
}

// Start implements run.Starter: it plays out the command's steps in the task
// card and the activity log, and changes the demo host as the command would.
func (h *Host) Start(sp run.Spec) (string, error) {
	h.mu.Lock()
	p, err := h.plan(sp.Command.Argv)
	h.mu.Unlock()
	if err != nil {
		return "", err
	}
	id, trace := randomHex(8), randomHex(16)
	h.st.AddTask(store.Task{
		ID: id, Action: sp.Action, Display: sp.Command.Display, VM: sp.VM, Trace: trace,
		Start: time.Now(), Status: store.TaskRunning, ShowOutput: sp.ShowOutput, Next: sp.Next, NextForm: sp.NextForm,
	})
	h.wg.Add(1)
	h.running.Add(1)
	go h.play(id, trace, sp, p)
	return id, nil
}

// Running implements run.Starter.
func (h *Host) Running() int { return int(h.running.Load()) }

// Shutdown implements run.Starter.
func (h *Host) Shutdown(time.Duration) {
	select {
	case <-h.quit:
	default:
		close(h.quit)
	}
	h.wg.Wait()
}

// play plays out the command as the task id: a step after the other, shown in
// the activity log under the task's trace.
func (h *Host) play(id, trace string, sp run.Spec, p plan) {
	defer h.wg.Done()
	defer h.running.Add(-1)
	r := store.Run{ID: id, Trace: trace, Title: title(sp.Command.Argv), Start: time.Now(), Program: sp.Command.Argv[0], Version: "demo"}
	h.setRun(r)
	wait := func() bool {
		select {
		case <-h.quit:
			return false
		case <-time.After(stepEvery):
			return true
		}
	}
	for i, s := range p.steps {
		r.Steps = append(r.Steps, store.Span{Kind: "step", Name: s.name})
		h.setRun(r)
		if !wait() {
			return
		}
		span := &r.Steps[i]
		span.Done, span.Status = true, "ok"
		switch {
		case s.skip != "":
			span.Status, span.Err = "skipped", s.skip
		case p.fail != "" && i == len(p.steps)-1:
			span.Status, span.Err = "failed", p.fail
		}
		h.setRun(r)
	}
	end := time.Now()
	r.End, r.Status = &end, "ok"
	if p.fail != "" {
		r.Status, r.Err = "failed", p.fail
	}
	h.setRun(r)
	if p.fail == "" && p.apply != nil {
		h.mu.Lock()
		p.apply(&h.state)
		h.publish()
		h.mu.Unlock()
	}
	if p.fail == "" && p.gw != nil {
		h.st.UpdateGateway(p.gwVM, func(g *store.Gateway) bool { p.gw(g); return true })
	}
	h.st.UpdateTask(id, store.TopicTasks, func(t *store.Task) {
		t.End, t.Output = &end, p.output
		if p.fail != "" {
			t.Status, t.Err = store.TaskFailed, p.fail
			t.Output += "Error: " + p.fail + "\n"
			return
		}
		t.Status = store.TaskOK
	})
	if p.fail == "" {
		time.AfterFunc(keepOK, func() { h.st.RemoveTask(id) })
	}
}

// setRun adds r to the activity log, or replaces the run with its ID.
func (h *Host) setRun(r store.Run) {
	r.Steps = slices.Clone(r.Steps)
	h.mu.Lock()
	defer h.mu.Unlock()
	h.runs = slices.Clone(h.runs)
	if i := slices.IndexFunc(h.runs, func(x store.Run) bool { return x.ID == r.ID }); i >= 0 {
		h.runs[i] = r
	} else {
		h.runs = slices.Insert(h.runs, 0, r)
	}
	h.st.SetActivity(h.runs)
}

// title names a run as brig does: the command and its first argument.
func title(argv []string) string {
	if argv[0] == "openshell" {
		// The gateway option is no part of the command's name.
		words := []string{"openshell"}
		for _, a := range argv[3:] {
			if strings.HasPrefix(a, "-") || len(words) > 5 {
				break
			}
			words = append(words, a)
		}
		return strings.Join(words, " ")
	}
	words, n := []string{"brig"}, 2
	if argv[1] == "image" {
		n = 3
	}
	for _, a := range argv[1:] {
		if len(words) > n || strings.HasPrefix(a, "-") {
			break
		}
		words = append(words, a)
	}
	return strings.Join(words, " ")
}

// flags returns the values of the options named in argv[2:], and whether
// boolean flags are set.
type flags struct {
	values map[string][]string
	set    map[string]bool
	args   []string
}

func parseFlags(argv []string) flags {
	f := flags{values: map[string][]string{}, set: map[string]bool{}}
	valued := map[string]bool{
		"--cpus": true, "--memory": true, "--root-disk": true, "--data-disk": true, "--profile": true, "--add-mount": true,
		"--remove-mount": true, "--add-openshell-config": true, "--remove-openshell-config": true, "--fedora": true, "--openshell": true,
	}
	for i := 0; i < len(argv); i++ {
		a := argv[i]
		switch {
		case valued[a] && i+1 < len(argv):
			f.values[a] = append(f.values[a], argv[i+1])
			i++
		case strings.HasPrefix(a, "--"):
			f.set[a] = true
		default:
			f.args = append(f.args, a)
		}
	}
	return f
}

// plan makes the made-up command for argv. h.mu is held.
func (h *Host) plan(argv []string) (plan, error) {
	if len(argv) > 0 && argv[0] == "openshell" {
		return h.openshellPlan(argv)
	}
	if len(argv) < 3 || argv[0] != "brig" {
		return plan{}, fmt.Errorf("the demo cannot play %q", argv)
	}
	cmd, rest, sub := argv[1], argv[2:], ""
	if cmd == "image" {
		sub, rest = rest[0], rest[1:]
	}
	f := parseFlags(rest)
	st := h.state
	vmName := ""
	if len(f.args) > 0 {
		vmName = f.args[0]
	}
	idx := slices.IndexFunc(st.VMs, func(v store.VM) bool { return v.Name == vmName })
	switch cmd {
	case "start":
		return startPlan(vmName, idx, st), nil
	case "stop":
		return stopPlan(vmName), nil
	case "delete":
		return plan{
			steps:  []step{{name: "Stop domain brig-" + vmName}, {name: "Unregister gateway brig-" + vmName}, {name: "Remove disks"}, {name: "Remove the VM record"}},
			output: "Deleted VM " + vmName + ".\n",
			apply:  func(s *store.VMState) { removeVM(s, vmName) },
		}, nil
	case "upgrade":
		return upgradePlan(vmName, idx, st), nil
	case "update":
		return updatePlan(vmName, idx, f, st), nil
	case "create":
		return h.createPlan(vmName, f, st), nil
	case "sync":
		return h.syncPlan(vmName, f), nil
	case "image":
		return imagePlan(sub, f, st), nil
	case "sandbox":
		if len(rest) > 1 && rest[0] == "create" {
			return h.sandboxCreatePlan(rest[1:])
		}
	}
	return plan{}, fmt.Errorf("the demo cannot play brig %s", cmd)
}

func startPlan(name string, idx int, st store.VMState) plan {
	configs := idx >= 0 && len(st.VMs[idx].OpenShellConfigs) > 0
	apply := step{name: "Apply OpenShell config directories"}
	if !configs {
		apply.skip = "no config directories"
	}
	return plan{
		steps: []step{
			{name: "Start the network"},
			{name: "Boot domain brig-" + name},
			{name: "Wait for the gateway"},
			{name: "Register gateway brig-" + name},
			apply,
		},
		output: "Started VM " + name + ".\n",
		apply: func(s *store.VMState) {
			change(s, name, func(v *store.VM) {
				v.State, v.Up = "running", time.Now()
				v.Stats = &store.Stats{CPU: pct(12), Memory: pct(28), Data: pct(34)}
				if configs {
					v.LastSync = &store.SyncRecord{Time: time.Now()}
				}
			})
		},
	}
}

func stopPlan(name string) plan {
	return plan{
		steps:  []step{{name: "Ask the guest to shut down"}, {name: "Wait for the shutdown"}, {name: "Stop the network"}},
		output: "Stopped VM " + name + ".\n",
		apply: func(s *store.VMState) {
			change(s, name, func(v *store.VM) {
				var data *float64
				if v.Stats != nil {
					data = v.Stats.Data
				}
				v.State, v.Up, v.Stats = "shut off", time.Time{}, &store.Stats{Data: data}
			})
		},
	}
}

func upgradePlan(name string, idx int, st store.VMState) plan {
	if idx < 0 || len(st.Images) == 0 {
		return plan{}
	}
	newest, running := st.Images[0], st.VMs[idx].Running()
	p := plan{
		steps:  []step{{name: "Back up the data disk"}, {name: "Swap the root disk onto " + newest.ID}, {name: "Check boot"}},
		output: "Upgraded VM " + name + " to " + newest.ID + ".\n",
		apply: func(s *store.VMState) {
			change(s, name, func(v *store.VM) { v.Image, v.OpenShell = newest.ID, newest.OpenShell })
			s.Images = slices.Clone(s.Images)
			for i := range s.Images {
				s.Images[i].UsedBy = slices.DeleteFunc(slices.Clone(s.Images[i].UsedBy), func(n string) bool { return n == name })
				if s.Images[i].ID == newest.ID {
					s.Images[i].UsedBy = append(s.Images[i].UsedBy, name)
				}
			}
		},
	}
	if running {
		p.steps = slices.Insert(p.steps, 0, step{name: "Stop the VM"})
		p.steps = append(p.steps, step{name: "Start the VM"})
	}
	return p
}

func updatePlan(name string, idx int, f flags, st store.VMState) plan {
	running := idx >= 0 && st.VMs[idx].Running()
	later := func(what string) step {
		if running {
			return step{name: what, skip: "at the next boot"}
		}
		return step{name: what}
	}
	p := plan{steps: []step{{name: "Save the VM record"}}, output: "Updated VM " + name + ".\n"}
	var applies []func(*store.VM)
	for _, arg := range f.values["--add-mount"] {
		if m, err := vm.ParseMount(arg); err == nil {
			p.steps = append(p.steps, step{name: "mount " + m.Target})
			applies = append(applies, func(v *store.VM) {
				m.Tag = fmt.Sprintf("brig%d", v.NextMountTag)
				v.NextMountTag++
				v.Mounts = append(slices.Clone(v.Mounts), m)
			})
		}
	}
	for _, target := range f.values["--remove-mount"] {
		p.steps = append(p.steps, step{name: "unmount " + target})
		applies = append(applies, func(v *store.VM) {
			v.Mounts = slices.DeleteFunc(slices.Clone(v.Mounts), func(m vm.Mount) bool { return m.Target == target })
		})
	}
	for _, prof := range f.values["--profile"] {
		p.steps = append(p.steps, later("Load the network rules"))
		applies = append(applies, func(v *store.VM) { v.NetworkProfile = prof })
	}
	for _, c := range f.values["--cpus"] {
		n, _ := strconv.Atoi(c)
		p.steps = append(p.steps, later("Update the domain definition"))
		applies = append(applies, func(v *store.VM) { v.CPUs = n })
	}
	for flag, set := range map[string]func(*store.VM, bytesize.Size){
		"--memory":    func(v *store.VM, s bytesize.Size) { v.Memory = s },
		"--root-disk": func(v *store.VM, s bytesize.Size) { v.RootDisk = s },
		"--data-disk": func(v *store.VM, s bytesize.Size) { v.DataDisk = s },
	} {
		for _, val := range f.values[flag] {
			if size, err := bytesize.Parse(val); err == nil {
				p.steps = append(p.steps, later("Resize "+strings.TrimPrefix(flag, "--")))
				applies = append(applies, func(v *store.VM) { set(v, size) })
			}
		}
	}
	for _, dir := range f.values["--add-openshell-config"] {
		applies = append(applies, func(v *store.VM) { v.OpenShellConfigs = append(slices.Clone(v.OpenShellConfigs), dir) })
	}
	for _, dir := range f.values["--remove-openshell-config"] {
		applies = append(applies, func(v *store.VM) {
			v.OpenShellConfigs = slices.DeleteFunc(slices.Clone(v.OpenShellConfigs), func(d string) bool { return d == dir })
		})
	}
	p.apply = func(s *store.VMState) {
		change(s, name, func(v *store.VM) {
			for _, a := range applies {
				a(v)
			}
		})
	}
	return p
}

func (h *Host) createPlan(name string, f flags, st store.VMState) plan {
	d := st.Config.Defaults
	cpus, memory, profile := d.CPUs, d.Memory, d.NetworkProfile
	if v := f.values["--cpus"]; len(v) > 0 {
		cpus, _ = strconv.Atoi(v[0])
	}
	if v := f.values["--memory"]; len(v) > 0 {
		memory, _ = bytesize.Parse(v[0])
	}
	if v := f.values["--profile"]; len(v) > 0 {
		profile = v[0]
	}
	var configs []string
	if !f.set["--no-openshell-config"] {
		configs = st.Config.OpenShellConfigs
	}
	start := !f.set["--no-start"]
	p := plan{steps: []step{{name: "Use base image " + st.Images[0].ID}, {name: "Create the disks"}, {name: "Define domain brig-" + name}}, output: "Created VM " + name + ".\n"}
	if start {
		p.steps = append(p.steps, step{name: "Start the network"}, step{name: "Boot"}, step{name: "Wait for the gateway"}, step{name: "Register gateway brig-" + name})
	}
	p.apply = func(s *store.VMState) {
		now := time.Now()
		v := store.VM{
			VM: vm.VM{
				Name: name, CreatedAt: now, Image: s.Images[0].ID, CPUs: cpus, Memory: memory,
				RootDisk: d.RootDisk, DataDisk: d.DataDisk, NetworkProfile: profile,
				Ports: vm.Ports{SSH: 42210 + len(s.VMs), Gateway: 42450 + len(s.VMs)}, OpenShellConfigs: configs,
			},
			State: "shut off", OpenShell: s.Images[0].OpenShell, Stats: &store.Stats{Data: pct(3)},
		}
		if start {
			v.State, v.Up = "running", now
			v.Stats = &store.Stats{CPU: pct(10), Memory: pct(22), Data: pct(3)}
		}
		s.VMs = append(slices.Clone(s.VMs), v)
		slices.SortFunc(s.VMs, func(a, b store.VM) int { return strings.Compare(a.Name, b.Name) })
		s.Images = slices.Clone(s.Images)
		s.Images[0].UsedBy = append(slices.Clone(s.Images[0].UsedBy), name)
	}
	return p
}

// syncPlan plays a sync, or its dry run. The first real sync of review fails
// as a locked keyring would make it; the next one works.
func (h *Host) syncPlan(name string, f flags) plan {
	if f.set["--dry-run"] {
		return plan{
			steps: []step{{name: "Read the config directories"}, {name: "Compare with the gateway"}},
			output: `profile  llm           unchanged
profile  github        unchanged
profile  gitlab-work   update  endpoints: + gitlab-ci.example.org:443
profile  claude-code   unchanged
provider gitlab-work   unchanged
provider claude-code   update  secret changed in keyring
policy   default       unchanged
secrets of gitlab-work may be sent to: gitlab.example.org:443, gitlab-ci.example.org:443
(made-up report; nothing was compared)
`,
		}
	}
	p := plan{
		steps:  []step{{name: "Read the config directories"}, {name: "Apply 4 provider profiles"}, {name: "Apply 3 providers"}, {name: "Set the default policy"}},
		output: "Synced VM " + name + ".\n",
		apply: func(s *store.VMState) {
			change(s, name, func(v *store.VM) { v.LastSync = &store.SyncRecord{Time: time.Now()} })
		},
	}
	if name == "review" && h.keyringLocked {
		h.keyringLocked = false
		p.fail = "provider gitlab-work: secret-tool found no secret for service=brig provider=gitlab-work. Unlock the keyring or store the token again, then sync."
		p.apply = func(s *store.VMState) {
			change(s, name, func(v *store.VM) { v.LastSync = &store.SyncRecord{Time: time.Now(), Error: "keyring locked"} })
		}
	}
	return p
}

func imagePlan(sub string, f flags, st store.VMState) plan {
	switch sub {
	case "build":
		fedora := st.Config.Defaults.FedoraRelease
		if v := f.values["--fedora"]; len(v) > 0 {
			fedora, _ = strconv.Atoi(v[0])
		}
		osv := "0.1.3"
		if v := f.values["--openshell"]; len(v) > 0 {
			osv = v[0]
		}
		id := fmt.Sprintf("f%d-openshell%s-%s", fedora, osv, time.Now().UTC().Format("20060102T150405Z"))
		return plan{
			steps:  []step{{name: "Resolve OpenShell " + osv}, {name: fmt.Sprintf("mkosi build (Fedora %d)", fedora)}, {name: "Convert to qcow2"}},
			output: fmt.Sprintf("Built image %s (Fedora %d, OpenShell %s).\n", id, fedora, osv),
			apply: func(s *store.VMState) {
				s.Images = slices.Insert(slices.Clone(s.Images), 0, store.Image{ID: id, Fedora: fedora, OpenShell: osv, CreatedAt: time.Now()})
			},
		}
	case "prune":
		var gone []string
		for i, img := range st.Images {
			if i > 0 && len(img.UsedBy) == 0 {
				gone = append(gone, img.ID)
			}
		}
		out := ""
		for _, id := range gone {
			out += "Removed image " + id + ".\n"
		}
		return plan{
			steps:  []step{{name: "Remove unused images"}, {name: "Clear the package cache"}},
			output: out + "Removed 412.0 MiB of cached packages.\n",
			apply: func(s *store.VMState) {
				s.Images = slices.DeleteFunc(slices.Clone(s.Images), func(i store.Image) bool { return slices.Contains(gone, i.ID) })
			},
		}
	case "push":
		return plan{steps: []step{{name: "podman save"}, {name: "Copy into the VM"}, {name: "podman load as agent"}}, output: "Loaded image.\n"}
	}
	return plan{}
}

// change applies fn to a copy of the named VM and puts the copy into the state.
func change(s *store.VMState, name string, fn func(*store.VM)) {
	i := slices.IndexFunc(s.VMs, func(v store.VM) bool { return v.Name == name })
	if i < 0 {
		return
	}
	s.VMs = slices.Clone(s.VMs)
	v := s.VMs[i]
	if v.Stats != nil {
		st := *v.Stats
		v.Stats = &st
	}
	fn(&v)
	s.VMs[i] = v
}

// removeVM takes the named VM out of the state and out of its image's users.
func removeVM(s *store.VMState, name string) {
	s.VMs = slices.DeleteFunc(slices.Clone(s.VMs), func(v store.VM) bool { return v.Name == name })
	s.Images = slices.Clone(s.Images)
	for i := range s.Images {
		s.Images[i].UsedBy = slices.DeleteFunc(slices.Clone(s.Images[i].UsedBy), func(n string) bool { return n == name })
	}
}

func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
