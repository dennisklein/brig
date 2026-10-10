// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/dennisklein/brig/internal/config"
	"github.com/dennisklein/brig/internal/image"
	"github.com/dennisklein/brig/internal/libvirt"
	"github.com/dennisklein/brig/internal/paths"
	"github.com/dennisklein/brig/internal/vm"
)

// app bundles what the VM and image commands share.
type app struct {
	dirs   paths.Dirs
	cfg    *config.Config
	vms    vm.Store
	images image.Store
}

func newApp() (*app, error) {
	dirs, err := paths.Default()
	if err != nil {
		return nil, err
	}
	cfg, err := config.Load(dirs.ConfigFile())
	if err != nil {
		return nil, err
	}
	return &app{dirs: dirs, cfg: cfg, vms: vm.NewStore(dirs), images: image.NewStore(dirs)}, nil
}

// connect connects to the libvirt session daemon.
func (a *app) connect(ctx context.Context) (*libvirt.Conn, error) {
	conn, err := libvirt.Connect(ctx)
	if err != nil {
		return nil, fmt.Errorf("connecting to libvirt (try brig doctor): %w", err)
	}
	return conn, nil
}

// lockVM serialises brig commands that change the named VM. The returned
// function releases the lock. The lock file stays after brig delete, so a
// command that starts later cannot lock a new file while delete holds the old.
func (a *app) lockVM(name string) (func(), error) {
	if err := vm.ValidateName(name); err != nil {
		return nil, err
	}
	if err := paths.EnsurePrivate(a.dirs.VMsDir()); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(a.vms.LockPath(name), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, fmt.Errorf("another brig command is working on VM %s", name)
		}
		return nil, err
	}
	return func() { _ = f.Close() }, nil
}

// lockAllVMs takes the lock of every VM, so that no brig command is changing
// a VM, for example upgrading it onto an image while its old root disk still
// depends on another. The returned function releases the locks.
func (a *app) lockAllVMs() (func(), error) {
	names, err := a.vms.DirNames()
	if err != nil {
		return nil, err
	}
	var unlocks []func()
	unlock := func() {
		for _, u := range unlocks {
			u()
		}
	}
	for _, name := range names {
		u, err := a.lockVM(name)
		if err != nil {
			unlock()
			return nil, err
		}
		unlocks = append(unlocks, u)
	}
	return unlock, nil
}

// lockLoadedVM locks the VM and reloads its record into v. The command loaded
// v before it had the lock, so a brig delete may have removed the VM since.
func (a *app) lockLoadedVM(v *vm.VM) (func(), error) {
	unlock, err := a.lockVM(v.Name)
	if err != nil {
		return nil, err
	}
	fresh, err := a.vms.Load(v.Name)
	if err != nil {
		unlock()
		return nil, err
	}
	*v = *fresh
	return unlock, nil
}

// newUUID returns a random (version 4) UUID.
func newUUID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16]), nil
}

// outputFormat is the value of an --output flag.
type outputFormat string

func (f *outputFormat) String() string { return string(*f) }

func (f *outputFormat) Set(v string) error {
	switch v {
	case "table", "json":
		*f = outputFormat(v)
		return nil
	}
	return fmt.Errorf("unknown output format %q (want table or json)", v)
}

func (f *outputFormat) Type() string { return "format" }

func addOutputFlag(cmd *cobra.Command, f *outputFormat) {
	*f = "table"
	cmd.Flags().VarP(f, "output", "o", "output format: table or json")
	_ = cmd.RegisterFlagCompletionFunc("output", cobra.FixedCompletions([]string{"table", "json"}, cobra.ShellCompDirectiveNoFileComp))
}

// hideDefaults keeps the help from showing the zero values of the named
// flags as their defaults: for these flags, zero means "not given", and the
// default comes from config.yaml or the VM.
func hideDefaults(cmd *cobra.Command, names ...string) {
	for _, name := range names {
		cmd.Flags().Lookup(name).DefValue = ""
	}
}

func writeJSON(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

// completeVMs completes the first argument with VM names.
func completeVMs(_ *cobra.Command, args []string, _ string) ([]string, cobra.ShellCompDirective) {
	if len(args) > 0 {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	dirs, err := paths.Default()
	if err != nil {
		return nil, cobra.ShellCompDirectiveError
	}
	return vm.NewStore(dirs).Names(), cobra.ShellCompDirectiveNoFileComp
}

// completeImages completes image IDs.
func completeImages(*cobra.Command, []string, string) ([]string, cobra.ShellCompDirective) {
	dirs, err := paths.Default()
	if err != nil {
		return nil, cobra.ShellCompDirectiveError
	}
	imgs, err := image.NewStore(dirs).List()
	if err != nil {
		return nil, cobra.ShellCompDirectiveError
	}
	ids := make([]string, len(imgs))
	for i, img := range imgs {
		ids[i] = img.ID
	}
	return ids, cobra.ShellCompDirectiveNoFileComp
}

// completeProfiles completes network profile names.
func completeProfiles(*cobra.Command, []string, string) ([]string, cobra.ShellCompDirective) {
	dirs, err := paths.Default()
	if err != nil {
		return nil, cobra.ShellCompDirectiveError
	}
	cfg, err := config.Load(dirs.ConfigFile())
	if err != nil {
		return nil, cobra.ShellCompDirectiveError
	}
	return cfg.ProfileNames(), cobra.ShellCompDirectiveNoFileComp
}
