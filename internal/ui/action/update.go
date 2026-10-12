// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

package action

import (
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/dennisklein/brig/internal/bytesize"
	"github.com/dennisklein/brig/internal/ui/store"
	"github.com/dennisklein/brig/internal/vm"
)

func init() {
	register(
		Action{ID: "mount-add", Subject: subjectVM, Build: buildMountAdd},
		Action{ID: "mount-rm", Subject: subjectVM, Build: buildMountRemove},
		Action{ID: "profile", Subject: subjectVM, Build: buildProfile},
		Action{ID: "resources", Subject: subjectVM, Build: buildResources},
		Action{ID: "config-add", Subject: subjectVM, Build: buildConfigAdd},
		Action{ID: "config-rm", Subject: subjectVM, Build: buildConfigRemove},
	)
}

// mountSpec writes a mount in brig's --mount syntax: SOURCE[:TARGET][:OPTIONS].
func mountSpec(src, target string, opts []string) string {
	spec := src
	if target != "" || len(opts) > 0 {
		spec += ":" + target
	}
	if len(opts) > 0 {
		spec += ":" + strings.Join(opts, ",")
	}
	return spec
}

func buildMountAdd(f Form, c Context) (Command, error) {
	v, err := findVM(f, c)
	if err != nil {
		return Command{}, err
	}
	src, err := dirPath(f, "src", "Host directory", c)
	if err != nil {
		return Command{}, err
	}
	if strings.Contains(src, ":") {
		return Command{}, errors.New("host directory: brig's mount syntax cannot hold a colon in the path")
	}
	var opts []string
	sandbox := f.Get("sandbox") != ""
	switch {
	case sandbox:
		opts = []string{"sandbox"}
	case f.Get("access") == "rw":
		opts = []string{"rw"}
	}
	target := strings.TrimSpace(f.Get("target"))
	if target != "" && !strings.HasPrefix(target, "/") {
		return Command{}, errors.New("in the VM: write an absolute path, such as /mnt/notes")
	}
	spec := mountSpec(src, target, opts)
	// brig parses the argument again; this is its own rule.
	m, err := vm.ParseMount(spec)
	if err != nil {
		return Command{}, err
	}
	for _, have := range v.Mounts {
		if have.Target == m.Target {
			return Command{}, fmt.Errorf("%s already shares %s at %s", v.Name, have.Source, have.Target)
		}
	}
	cmd := NewCommand("brig", "update", v.Name, "--add-mount", spec)
	if v.Running() {
		cmd = cmd.Note("%s is running, so the directory is plugged in at once.", v.Name)
	} else {
		cmd = cmd.Note("%s is not running; the directory is mounted when it starts.", v.Name)
	}
	if !m.ReadOnly {
		cmd = cmd.Note("Read-write: what the agent user writes in the VM is yours on the host.")
	}
	return cmd, nil
}

func buildMountRemove(f Form, c Context) (Command, error) {
	v, err := findVM(f, c)
	if err != nil {
		return Command{}, err
	}
	target := f.Get("target")
	i := slices.IndexFunc(v.Mounts, func(m vm.Mount) bool { return m.Target == target })
	if i < 0 {
		return Command{}, fmt.Errorf("%s shares nothing at %q", v.Name, target)
	}
	cmd := NewCommand("brig", "update", v.Name, "--remove-mount", target)
	if v.Mounts[i].Sandbox && v.Running() {
		cmd = cmd.Note("A sandbox that uses this directory as a volume blocks the removal: delete it first, or stop %s.", v.Name)
	}
	return cmd, nil
}

func buildProfile(f Form, c Context) (Command, error) {
	v, err := findVM(f, c)
	if err != nil {
		return Command{}, err
	}
	name := f.Get("profile")
	if _, ok := c.Snap.Profiles[name]; !ok {
		return Command{}, fmt.Errorf("config.yaml has no network profile %q", name)
	}
	if name == v.NetworkProfile {
		return Command{}, errNothing
	}
	cmd := NewCommand("brig", "update", v.Name, "--profile", name)
	if v.Running() {
		cmd = cmd.Note("Applies when %s next starts: brig stop %[1]s && brig start %[1]s", v.Name)
	}
	return cmd, nil
}

func buildResources(f Form, c Context) (Command, error) {
	v, err := findVM(f, c)
	if err != nil {
		return Command{}, err
	}
	argv := []string{"brig", "update", v.Name}
	if s := strings.TrimSpace(f.Get("cpus")); s != "" {
		n, err := strconv.Atoi(s)
		if err != nil || n < 1 || n > 256 {
			return Command{}, fmt.Errorf("CPUs: want a number from 1 to 256")
		}
		if n != v.CPUs {
			argv = append(argv, "--cpus", strconv.Itoa(n))
		}
	}
	sizes := []struct {
		field, label, flag string
		have               bytesize.Size
		grow               bool
	}{
		{"memory", "Memory", "--memory", v.Memory, false},
		{"root-disk", "Root disk", "--root-disk", v.RootDisk, true},
		{"data-disk", "Data disk", "--data-disk", v.DataDisk, true},
	}
	for _, s := range sizes {
		text := strings.TrimSpace(f.Get(s.field))
		if text == "" {
			continue
		}
		size, err := bytesize.Parse(text)
		if err != nil {
			return Command{}, fmt.Errorf("%s: %w", s.label, err)
		}
		switch {
		case size == s.have:
		case s.grow && size < s.have:
			return Command{}, fmt.Errorf("%s: disks can only grow (now %s)", s.label, s.have)
		default:
			argv = append(argv, s.flag, text)
		}
	}
	if len(argv) == 3 {
		return Command{}, errNothing
	}
	cmd := NewCommand(argv...)
	if v.Running() {
		cmd = cmd.Note("Applies when %s next starts: brig stop %[1]s && brig start %[1]s", v.Name)
	}
	return cmd, nil
}

func buildConfigAdd(f Form, c Context) (Command, error) {
	v, err := findVM(f, c)
	if err != nil {
		return Command{}, err
	}
	dir, err := dirPath(f, "dir", "Directory", c)
	if err != nil {
		return Command{}, err
	}
	if slices.Contains(v.OpenShellConfigs, dir) {
		return Command{}, fmt.Errorf("%s applies %s already", v.Name, dir)
	}
	cmd := NewCommand("brig", "update", v.Name, "--add-openshell-config", dir).
		Note("brig reads profiles/*.yaml, providers/*.yaml and policies/default.yaml in it, and refuses a directory it cannot load.")
	return syncAfter(cmd, v), nil
}

func buildConfigRemove(f Form, c Context) (Command, error) {
	v, err := findVM(f, c)
	if err != nil {
		return Command{}, err
	}
	dir := f.Get("dir")
	if !slices.Contains(v.OpenShellConfigs, dir) {
		return Command{}, fmt.Errorf("%s does not apply %q", v.Name, dir)
	}
	cmd := NewCommand("brig", "update", v.Name, "--remove-openshell-config", dir).
		Note("What the directory created in the gateway stays until a sync with --prune.")
	return syncAfter(cmd, v), nil
}

// syncAfter notes that a running VM takes a changed config directory list
// with its next sync.
func syncAfter(cmd Command, v store.VM) Command {
	if v.Running() {
		return cmd.Note("Run a sync to apply it to the running gateway; brig start applies it too.")
	}
	return cmd
}
