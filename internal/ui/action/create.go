// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

package action

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/dennisklein/brig/internal/bytesize"
	"github.com/dennisklein/brig/internal/vm"
)

func init() {
	register(Action{ID: "create", Subject: func(f Form) string { return f.Get("name") }, Build: buildCreate})
}

func buildCreate(f Form, c Context) (Command, error) {
	name := f.Get("name")
	if err := vm.ValidateName(name); err != nil {
		return Command{}, err
	}
	if _, ok := c.Snap.VM(name); ok {
		return Command{}, fmt.Errorf("there is a VM %s already", name)
	}
	d := c.Snap.Config.Defaults
	argv := []string{"brig", "create", name}
	if s := strings.TrimSpace(f.Get("cpus")); s != "" {
		n, err := strconv.Atoi(s)
		if err != nil || n < 1 || n > 256 {
			return Command{}, fmt.Errorf("CPUs: want a number from 1 to 256")
		}
		if n != d.CPUs {
			argv = append(argv, "--cpus", strconv.Itoa(n))
		}
	}
	if s := strings.TrimSpace(f.Get("memory")); s != "" {
		size, err := bytesize.Parse(s)
		if err != nil {
			return Command{}, fmt.Errorf("memory: %w", err)
		}
		if size != d.Memory {
			argv = append(argv, "--memory", s)
		}
	}
	if p := f.Get("profile"); p != "" {
		if _, ok := c.Snap.Profiles[p]; !ok {
			return Command{}, fmt.Errorf("config.yaml has no network profile %q", p)
		}
		if p != d.NetworkProfile {
			argv = append(argv, "--profile", p)
		}
	}
	if f.Get("configs") == "" && len(c.Snap.Config.OpenShellConfigs) > 0 {
		argv = append(argv, "--no-openshell-config")
	}
	if f.Get("start") == "" {
		argv = append(argv, "--no-start")
	}
	cmd := NewCommand(argv...)
	if c.Snap.NewestImage == nil {
		cmd = cmd.Note("There is no base image yet, so brig builds one first. That takes a while.")
	}
	return cmd, nil
}
