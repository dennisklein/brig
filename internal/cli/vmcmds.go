// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"slices"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/dennisklein/brig/internal/bytesize"
	"github.com/dennisklein/brig/internal/config"
	"github.com/dennisklein/brig/internal/image"
	"github.com/dennisklein/brig/internal/libvirt"
	"github.com/dennisklein/brig/internal/ocsync"
	"github.com/dennisklein/brig/internal/openshell"
	"github.com/dennisklein/brig/internal/paths"
	"github.com/dennisklein/brig/internal/qemuimg"
	"github.com/dennisklein/brig/internal/sshx"
	"github.com/dennisklein/brig/internal/vm"
)

func newCreateCmd() *cobra.Command {
	var (
		cpus             int
		memory           bytesize.Size
		rootDisk         bytesize.Size
		dataDisk         bytesize.Size
		profile, imageID string
		mounts, configs  []string
		noStart          bool
		noConfigs        bool
	)
	cmd := &cobra.Command{
		Use:   "create NAME",
		Short: "Create and start a VM",
		Long: `Create a VM from the newest base image (building one first if none exists)
and start it. Settings not given as flags come from config.yaml.

Mounts share host directories with the VM via virtiofs:
  --mount SOURCE[:TARGET][:OPTIONS]
TARGET defaults to /mnt/<basename of SOURCE>. OPTIONS is a comma-separated
list of ro (default), rw and sandbox; sandbox also offers the directory to
OpenShell sandboxes, read-only, as a Podman volume named after the mount's
tag (see brig show).

OpenShell config directories hold provider profiles and providers, which
brig applies to the VM's gateway at every start, and a default sandbox
policy, which eval "$(brig env NAME)" exports (see brig sync --help).
Without --openshell-config, the VM gets the directories listed as
openshell.configs in config.yaml.`,
		Example: `  brig create dev
  brig create dev --memory 16GiB --mount ~/src/project:/work/project:rw
  brig create offline --profile isolated`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			a, err := newApp()
			if err != nil {
				return err
			}
			name := args[0]
			if err := vm.ValidateName(name); err != nil {
				return err
			}
			if a.vms.Exists(name) {
				return fmt.Errorf("VM %s already exists", name)
			}
			d := a.cfg.Defaults
			v := &vm.VM{
				Name: name, CreatedAt: time.Now().UTC(),
				CPUs: d.CPUs, Memory: d.Memory, RootDisk: d.RootDisk, DataDisk: d.DataDisk,
				NetworkProfile: d.NetworkProfile, Image: imageID,
			}
			f := cmd.Flags()
			if f.Changed("cpus") {
				v.CPUs = cpus
			}
			if f.Changed("memory") {
				v.Memory = memory
			}
			if f.Changed("root-disk") {
				v.RootDisk = rootDisk
			}
			if f.Changed("data-disk") {
				v.DataDisk = dataDisk
			}
			if f.Changed("profile") {
				v.NetworkProfile = profile
			}
			if _, err := a.cfg.Profile(v.NetworkProfile); err != nil {
				return err
			}
			for _, arg := range mounts {
				m, err := vm.ParseMount(arg)
				if err != nil {
					return err
				}
				v.Mounts = append(v.Mounts, m)
			}
			v.AssignMountTags()
			if err := checkVM(v); err != nil {
				return err
			}
			if !noConfigs {
				dirs := a.cfg.OpenShell.Configs
				if f.Changed("openshell-config") {
					dirs = configs
				}
				if v.OpenShellConfigs, err = configDirs(dirs); err != nil {
					return err
				}
				if _, err := ocsync.Load(v.OpenShellConfigs); err != nil {
					return err
				}
			}
			if v.RootDisk < config.MinRootDisk || v.DataDisk < config.MinDataDisk {
				return fmt.Errorf("a VM needs a root disk of at least %s and a data disk of at least %s", config.MinRootDisk, config.MinDataDisk)
			}
			return a.create(cmd.Context(), cmd, v, !noStart)
		},
	}
	f := cmd.Flags()
	f.IntVar(&cpus, "cpus", 0, "number of virtual CPUs")
	f.Var(&memory, "memory", "memory size, e.g. 8GiB")
	f.Var(&rootDisk, "root-disk", "root disk size, e.g. 20GiB")
	f.Var(&dataDisk, "data-disk", "size of the persistent data disk holding /home/agent, e.g. 40GiB")
	f.StringVar(&profile, "profile", "", "network profile (see config.yaml)")
	f.StringVar(&imageID, "image", "", "base image ID (default: newest)")
	f.StringArrayVar(&mounts, "mount", nil, "share a host directory: SOURCE[:TARGET][:OPTIONS], OPTIONS a comma-separated list of ro (default), rw and sandbox (repeatable)")
	f.BoolVar(&noStart, "no-start", false, "create the VM without starting it")
	f.StringArrayVar(&configs, "openshell-config", nil, "OpenShell config directory to apply to the VM's gateway (repeatable; default: openshell.configs from config.yaml)")
	f.BoolVar(&noConfigs, "no-openshell-config", false, "apply no OpenShell config directories")
	cmd.MarkFlagsMutuallyExclusive("openshell-config", "no-openshell-config")
	hideDefaults(cmd, "memory", "root-disk", "data-disk")
	_ = cmd.RegisterFlagCompletionFunc("profile", completeProfiles)
	_ = cmd.RegisterFlagCompletionFunc("image", completeImages)
	_ = cmd.RegisterFlagCompletionFunc("openshell-config", completeDirs)
	return cmd
}

func (a *app) create(ctx context.Context, cmd *cobra.Command, v *vm.VM, start bool) (err error) {
	w := cmd.OutOrStdout()
	if v.Image == "" {
		img, err := a.images.Newest()
		if errors.Is(err, image.ErrNotFound) {
			fmt.Fprintln(w, "No base image yet; building one first (this takes a while).")
			// Another build may add an image while this one waits for it.
			img, err = a.buildImage(ctx, cmd, image.BuildOptions{FedoraRelease: a.cfg.Defaults.FedoraRelease, IfNone: true})
		}
		if err != nil {
			return err
		}
		v.Image = img.ID
	} else if _, err := a.images.Get(v.Image); err != nil {
		return err
	}
	if v.UUID, err = newUUID(); err != nil {
		return err
	}
	if v.Ports, err = allocatePorts(); err != nil {
		return err
	}

	unlock, err := a.lockVM(v.Name)
	if err != nil {
		return err
	}
	defer unlock()
	// Another brig create of the same name may have finished meanwhile, e.g.
	// while this one waited for an image build.
	if a.vms.Exists(v.Name) {
		return fmt.Errorf("VM %s already exists", v.Name)
	}
	conn, err := a.connect(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()

	// Remove the VM's files if creation fails before its domain is defined.
	// A domain of that name that already exists is none of this command's
	// business, and a VM whose domain is defined is kept, even if it does
	// not start.
	defined := false
	defer func() {
		if err != nil && !defined {
			_ = a.vms.Remove(v.Name)
		}
	}()
	if err := a.writeKeys(v); err != nil {
		return err
	}
	if err := qemuimg.CreateOverlay(ctx, a.vmFile(v, rootDiskFile), a.images.Path(v.Image), v.RootDisk); err != nil {
		return err
	}
	if err := qemuimg.Create(ctx, a.vmFile(v, dataDiskFile), v.DataDisk); err != nil {
		return err
	}
	if err := a.vms.Save(v); err != nil {
		return err
	}
	if err := a.define(conn, v); err != nil {
		return err
	}
	defined = true
	fmt.Fprintf(w, "Created VM %s from image %s.\n", v.Name, v.Image)
	if !start {
		return nil
	}
	if err := a.start(ctx, conn, v, w); err != nil {
		// Keep the VM for inspection; it was created successfully.
		return fmt.Errorf("VM %s was created but did not start cleanly: %w", v.Name, err)
	}
	return nil
}

// writeKeys creates the VM's SSH client key, host key and known_hosts.
func (a *app) writeKeys(v *vm.VM) error {
	client, clientPub, err := sshx.GenerateKey("brig@" + v.Name)
	if err != nil {
		return err
	}
	host, hostPub, err := sshx.GenerateKey("root@" + v.Name)
	if err != nil {
		return err
	}
	files := []struct {
		name string
		data []byte
	}{
		{clientKeyFile, client},
		{clientKeyFile + ".pub", []byte(sshx.AuthorizedKey(clientPub, "brig@"+v.Name) + "\n")},
		{hostKeyFile, host},
	}
	for _, f := range files {
		if err := vm.WriteFileAtomic(a.vmFile(v, f.name), f.data, 0o600); err != nil {
			return err
		}
	}
	return sshx.WriteKnownHosts(a.vmFile(v, knownHostsFile), v.DomainName(), hostPub)
}

// vmCommand builds a command that operates on one existing VM.
func vmCommand(use, short string, run func(ctx context.Context, cmd *cobra.Command, a *app, v *vm.VM) error) *cobra.Command {
	return &cobra.Command{
		Use:               use,
		Short:             short,
		Args:              cobra.ExactArgs(1),
		ValidArgsFunction: completeVMs,
		RunE: func(cmd *cobra.Command, args []string) error {
			a, err := newApp()
			if err != nil {
				return err
			}
			v, err := a.vms.Load(args[0])
			if err != nil {
				return err
			}
			return run(cmd.Context(), cmd, a, v)
		},
	}
}

func newStartCmd() *cobra.Command {
	return vmCommand("start NAME", "Start a VM and connect its OpenShell gateway", func(ctx context.Context, cmd *cobra.Command, a *app, v *vm.VM) error {
		unlock, err := a.lockVM(v.Name)
		if err != nil {
			return err
		}
		defer unlock()
		conn, err := a.connect(ctx)
		if err != nil {
			return err
		}
		defer func() { _ = conn.Close() }()
		return a.start(ctx, conn, v, cmd.OutOrStdout())
	})
}

func newStopCmd() *cobra.Command {
	var force bool
	cmd := vmCommand("stop NAME", "Shut a VM down", func(ctx context.Context, _ *cobra.Command, a *app, v *vm.VM) error {
		unlock, err := a.lockVM(v.Name)
		if err != nil {
			return err
		}
		defer unlock()
		conn, err := a.connect(ctx)
		if err != nil {
			return err
		}
		defer func() { _ = conn.Close() }()
		return a.stop(ctx, conn, v, force)
	})
	cmd.Flags().BoolVarP(&force, "force", "f", false, "power off immediately instead of shutting down gracefully")
	return cmd
}

func newDeleteCmd() *cobra.Command {
	var force bool
	cmd := vmCommand("delete NAME", "Delete a VM and all its data", func(ctx context.Context, cmd *cobra.Command, a *app, v *vm.VM) error {
		unlock, err := a.lockVM(v.Name)
		if err != nil {
			return err
		}
		defer unlock()
		conn, err := a.connect(ctx)
		if err != nil {
			return err
		}
		defer func() { _ = conn.Close() }()
		state, err := conn.State(v.DomainName())
		if err != nil {
			return err
		}
		if state != libvirt.StateShutoff && state != libvirt.StateMissing && !force {
			return fmt.Errorf("VM %s is %s; stop it first or use --force", v.Name, state)
		}
		if err := a.stop(ctx, conn, v, true); err != nil {
			return err
		}
		if err := conn.Undefine(v.DomainName()); err != nil {
			return err
		}
		if socket, err := netSocket(v); err == nil {
			_ = os.RemoveAll(filepath.Dir(socket))
		}
		if err := a.forgetGateway(ctx, v); err != nil {
			fmt.Fprintf(cmd.ErrOrStderr(), "Warning: %v\n", err)
		}
		if err := a.vms.Remove(v.Name); err != nil {
			return err
		}
		fmt.Fprintf(cmd.OutOrStdout(), "Deleted VM %s.\n", v.Name)
		return nil
	})
	cmd.Aliases = []string{"rm"}
	cmd.Flags().BoolVarP(&force, "force", "f", false, "delete a running VM")
	return cmd
}

// forgetGateway removes the VM's gateway from the host's openshell CLI.
func (a *app) forgetGateway(ctx context.Context, v *vm.VM) error {
	cfgHome, err := openshellConfigHome()
	if err != nil {
		return err
	}
	if cli, err := openshell.Find(); err == nil {
		if err := cli.Unregister(ctx, v.GatewayName()); err != nil {
			return fmt.Errorf("unregistering gateway %s: %w", v.GatewayName(), err)
		}
	}
	return openshell.RemoveBundle(cfgHome, v.GatewayName())
}

// vmStatus is a VM record plus its live state, as printed by list and show.
type vmStatus struct {
	*vm.VM
	State   string `json:"state"`
	Gateway string `json:"gateway"`
	// OpenShellSync is the outcome of the last sync of the VM's OpenShell
	// config directories; only show reports it.
	OpenShellSync *ocsync.Record `json:"openshell_sync,omitempty"`
}

// status returns the VMs it can read with their states, and an error about
// the rest and about states it cannot get.
func (a *app) status(ctx context.Context) ([]vmStatus, error) {
	vms, err := a.vms.List()
	if len(vms) == 0 {
		return nil, err
	}
	conn, cerr := a.connect(ctx)
	if cerr == nil {
		defer func() { _ = conn.Close() }()
	}
	out := make([]vmStatus, len(vms))
	for i, v := range vms {
		out[i] = vmStatus{VM: v, State: "unknown", Gateway: v.GatewayName()}
		if cerr == nil {
			if s, err := conn.State(v.DomainName()); err == nil {
				out[i].State = s.String()
			}
		}
	}
	return out, errors.Join(err, cerr)
}

func newListCmd() *cobra.Command {
	var format outputFormat
	cmd := &cobra.Command{
		Use:     "list",
		Aliases: []string{"ls"},
		Short:   "List VMs",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			a, err := newApp()
			if err != nil {
				return err
			}
			vms, err := a.status(cmd.Context())
			if err != nil {
				fmt.Fprintf(cmd.ErrOrStderr(), "Warning: %v\n", err)
			}
			if format == "json" {
				if vms == nil {
					vms = []vmStatus{}
				}
				return writeJSON(cmd.OutOrStdout(), vms)
			}
			tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
			fmt.Fprintln(tw, "NAME\tSTATE\tCPUS\tMEMORY\tPROFILE\tIMAGE")
			for _, v := range vms {
				fmt.Fprintf(tw, "%s\t%s\t%d\t%s\t%s\t%s\n", v.Name, v.State, v.CPUs, v.Memory, v.NetworkProfile, v.Image)
			}
			return tw.Flush()
		},
	}
	addOutputFlag(cmd, &format)
	return cmd
}

func newShowCmd() *cobra.Command {
	var format outputFormat
	cmd := vmCommand("show NAME", "Show a VM's settings and state", func(ctx context.Context, cmd *cobra.Command, a *app, v *vm.VM) error {
		st := vmStatus{VM: v, State: "unknown", Gateway: v.GatewayName(), OpenShellSync: a.lastSync(v)}
		if conn, err := a.connect(ctx); err == nil {
			defer func() { _ = conn.Close() }()
			if s, err := conn.State(v.DomainName()); err == nil {
				st.State = s.String()
			}
		}
		if format == "json" {
			return writeJSON(cmd.OutOrStdout(), st)
		}
		tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
		row := func(k, v any) { fmt.Fprintf(tw, "%v:\t%v\n", k, v) }
		row("Name", v.Name)
		row("State", st.State)
		row("Created", v.CreatedAt.Local().Format(time.DateTime))
		row("Image", v.Image)
		row("CPUs", v.CPUs)
		row("Memory", v.Memory)
		row("Root disk", v.RootDisk)
		row("Data disk", v.DataDisk)
		row("Network profile", v.NetworkProfile)
		for _, m := range v.Mounts {
			if m.Sandbox {
				row("Mount", fmt.Sprintf("%s (sandbox volume %s)", m, m.Tag))
			} else {
				row("Mount", m)
			}
		}
		for _, dir := range v.OpenShellConfigs {
			row("OpenShell config", dir)
		}
		if r := st.OpenShellSync; r != nil {
			result := "ok"
			if r.Error != "" {
				result = "failed: " + r.Error
			}
			row("Last sync", fmt.Sprintf("%s, %s", r.Time.Local().Format(time.DateTime), result))
		}
		row("SSH", fmt.Sprintf("brig ssh %s (127.0.0.1:%d)", v.Name, v.Ports.SSH))
		row("Gateway", fmt.Sprintf("%s (https://127.0.0.1:%d)", v.GatewayName(), v.Ports.Gateway))
		row("Directory", a.vms.Dir(v.Name))
		return tw.Flush()
	})
	addOutputFlag(cmd, &format)
	return cmd
}

func newUpdateCmd() *cobra.Command {
	var (
		cpus                     int
		memory, rootDisk, dataDk bytesize.Size
		profile                  string
		addMounts, removeMounts  []string
		addConfigs, rmConfigs    []string
	)
	cmd := vmCommand("update NAME", "Change a VM's resources, mounts, network profile or OpenShell config directories", func(ctx context.Context, cmd *cobra.Command, a *app, v *vm.VM) error {
		unlock, err := a.lockVM(v.Name)
		if err != nil {
			return err
		}
		defer unlock()
		f := cmd.Flags()
		old := *v
		if f.Changed("cpus") {
			v.CPUs = cpus
		}
		if f.Changed("memory") {
			v.Memory = memory
		}
		if f.Changed("profile") {
			if _, err := a.cfg.Profile(profile); err != nil {
				return err
			}
			v.NetworkProfile = profile
		}
		for _, target := range removeMounts {
			i := mountIndex(v.Mounts, target)
			if i < 0 {
				return fmt.Errorf("VM %s has no mount at %s", v.Name, target)
			}
			v.Mounts = append(v.Mounts[:i:i], v.Mounts[i+1:]...)
		}
		for _, arg := range addMounts {
			m, err := vm.ParseMount(arg)
			if err != nil {
				return err
			}
			if mountIndex(v.Mounts, m.Target) >= 0 {
				return fmt.Errorf("VM %s already has a mount at %s", v.Name, m.Target)
			}
			v.Mounts = append(v.Mounts, m)
		}
		v.AssignMountTags()
		if err := checkVM(v); err != nil {
			return err
		}
		rmDirs, err := configDirs(rmConfigs)
		if err != nil {
			return err
		}
		v.OpenShellConfigs = slices.Clone(v.OpenShellConfigs)
		for _, dir := range rmDirs {
			i := slices.Index(v.OpenShellConfigs, dir)
			if i < 0 {
				return fmt.Errorf("VM %s has no OpenShell config directory %s", v.Name, dir)
			}
			v.OpenShellConfigs = slices.Delete(v.OpenShellConfigs, i, i+1)
		}
		addDirs, err := configDirs(addConfigs)
		if err != nil {
			return err
		}
		for _, dir := range addDirs {
			if !slices.Contains(v.OpenShellConfigs, dir) {
				v.OpenShellConfigs = append(v.OpenShellConfigs, dir)
			}
		}
		if len(addConfigs)+len(rmConfigs) > 0 {
			if _, err := ocsync.Load(v.OpenShellConfigs); err != nil {
				return err
			}
		}

		conn, err := a.connect(ctx)
		if err != nil {
			return err
		}
		defer func() { _ = conn.Close() }()
		state, err := conn.State(v.DomainName())
		if err != nil {
			return err
		}
		if state == libvirt.StatePaused {
			return fmt.Errorf("VM %s is paused or was saved at logout; start or stop it first", v.Name)
		}
		running := state == libvirt.StateRunning
		disks := []struct {
			flag, file, dev string
			size            bytesize.Size
			field           *bytesize.Size
		}{
			{"root-disk", rootDiskFile, "vda", rootDisk, &v.RootDisk},
			{"data-disk", dataDiskFile, "vdb", dataDk, &v.DataDisk},
		}
		for _, d := range disks {
			if !f.Changed(d.flag) {
				continue
			}
			if d.size < *d.field {
				return fmt.Errorf("--%s: disks can only grow (currently %s)", d.flag, *d.field)
			}
			if d.size == *d.field {
				continue
			}
			if running {
				err = conn.BlockResize(v.DomainName(), d.dev, d.size)
			} else {
				err = qemuimg.Resize(ctx, a.vmFile(v, d.file), d.size)
			}
			if err != nil {
				return err
			}
			*d.field = d.size
		}
		if err := a.vms.Save(v); err != nil {
			return err
		}
		if err := a.define(conn, v); err != nil {
			_ = a.vms.Save(&old)
			return err
		}
		w := cmd.OutOrStdout()
		fmt.Fprintf(w, "Updated VM %s.\n", v.Name)
		configFlags := 0
		for _, name := range []string{"add-openshell-config", "remove-openshell-config"} {
			if f.Changed(name) {
				configFlags++
			}
		}
		if running && (f.NFlag() == 0 || f.NFlag() > configFlags) {
			// The guest grows its file systems at boot.
			fmt.Fprintf(w, "The changes, including the space on grown disks, apply when %s next starts: brig stop %[1]s && brig start %[1]s\n", v.Name)
		}
		if running && !slices.Equal(old.OpenShellConfigs, v.OpenShellConfigs) {
			fmt.Fprintf(w, "Apply the OpenShell config directories now with `brig sync %s`; brig start applies them too.\n", v.Name)
		}
		return nil
	})
	f := cmd.Flags()
	f.IntVar(&cpus, "cpus", 0, "number of virtual CPUs")
	f.Var(&memory, "memory", "memory size, e.g. 16GiB")
	f.Var(&rootDisk, "root-disk", "grow the root disk to this size")
	f.Var(&dataDk, "data-disk", "grow the data disk to this size")
	f.StringVar(&profile, "profile", "", "network profile")
	f.StringArrayVar(&addMounts, "add-mount", nil, "add a mount: SOURCE[:TARGET][:OPTIONS] as for brig create --mount (repeatable)")
	f.StringArrayVar(&removeMounts, "remove-mount", nil, "remove the mount at this target directory (repeatable)")
	f.StringArrayVar(&addConfigs, "add-openshell-config", nil, "add an OpenShell config directory (repeatable)")
	f.StringArrayVar(&rmConfigs, "remove-openshell-config", nil, "remove an OpenShell config directory (repeatable)")
	hideDefaults(cmd, "memory", "root-disk", "data-disk")
	_ = cmd.RegisterFlagCompletionFunc("profile", completeProfiles)
	_ = cmd.RegisterFlagCompletionFunc("remove-mount", completeMountTargets)
	_ = cmd.RegisterFlagCompletionFunc("add-openshell-config", completeDirs)
	_ = cmd.RegisterFlagCompletionFunc("remove-openshell-config", completeConfigDirs)
	return cmd
}

// mountIndex returns the index of the mount at target, or -1.
func mountIndex(mounts []vm.Mount, target string) int {
	target = path.Clean(target)
	for i, m := range mounts {
		if m.Target == target {
			return i
		}
	}
	return -1
}

// completeMountTargets completes the mount targets of the VM named by the
// first argument.
func completeMountTargets(_ *cobra.Command, args []string, _ string) ([]string, cobra.ShellCompDirective) {
	if len(args) == 0 {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	dirs, err := paths.Default()
	if err != nil {
		return nil, cobra.ShellCompDirectiveError
	}
	v, err := vm.NewStore(dirs).Load(args[0])
	if err != nil {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	targets := make([]string, len(v.Mounts))
	for i, m := range v.Mounts {
		targets[i] = m.Target
	}
	return targets, cobra.ShellCompDirectiveNoFileComp
}

func newSSHCmd() *cobra.Command {
	var root bool
	cmd := &cobra.Command{
		Use:   "ssh NAME [-- COMMAND...]",
		Short: "Open a shell in a VM, or run a command there",
		Long: `Open a shell in a VM as user agent, which runs the OpenShell gateway and
owns the persistent home directory, or as root with --root.`,
		Example: `  brig ssh dev
  brig ssh dev -- systemctl --user status openshell-gateway
  brig ssh dev --root -- journalctl -b`,
		Args:              cobra.MinimumNArgs(1),
		ValidArgsFunction: completeVMs,
		RunE: func(_ *cobra.Command, args []string) error {
			a, err := newApp()
			if err != nil {
				return err
			}
			v, err := a.vms.Load(args[0])
			if err != nil {
				return err
			}
			return execve("ssh", a.sshTarget(v, root).Interactive(args[1:]...))
		},
	}
	cmd.Flags().BoolVar(&root, "root", false, "log in as root")
	return cmd
}

func newConsoleCmd() *cobra.Command {
	return vmCommand("console NAME", "Attach to a VM's serial console (exit with Ctrl+])", func(_ context.Context, _ *cobra.Command, _ *app, v *vm.VM) error {
		return execve("virsh", []string{"virsh", "--connect", "qemu:///session", "console", v.DomainName()})
	})
}

func newEnvCmd() *cobra.Command {
	cmd := vmCommand("env NAME", "Print shell commands that point the openshell CLI at a VM", func(_ context.Context, cmd *cobra.Command, _ *app, v *vm.VM) error {
		out := cmd.OutOrStdout()
		fmt.Fprintf(out, "export OPENSHELL_GATEWAY=%s\n", v.GatewayName())
		policy := ""
		if len(v.OpenShellConfigs) > 0 {
			set, err := ocsync.Load(v.OpenShellConfigs)
			if err != nil {
				fmt.Fprintf(cmd.ErrOrStderr(), "Warning: %v\n", err)
			} else {
				policy = set.DefaultPolicy
			}
		}
		// Unset a default policy that another VM's environment exported.
		if policy == "" {
			fmt.Fprintln(out, "unset OPENSHELL_SANDBOX_POLICY")
		} else {
			fmt.Fprintf(out, "export OPENSHELL_SANDBOX_POLICY=%s\n", shellQuote(policy))
		}
		return nil
	})
	cmd.Long = `Print shell commands that point the openshell CLI at a VM's gateway:
OPENSHELL_GATEWAY, and OPENSHELL_SANDBOX_POLICY when one of the VM's
OpenShell config directories has a policies/default.yaml (else it is
unset, so that no other VM's default policy applies). Use it as
eval "$(brig env NAME)"; without the quotes, the shell splits paths
that contain spaces.`
	return cmd
}

// execve replaces brig with the named program, which gets the arguments argv
// (starting with its name); tests replace it.
var execve = func(name string, argv []string) error {
	path, err := exec.LookPath(name)
	if err != nil {
		return fmt.Errorf("%s is not installed (see brig doctor): %w", name, err)
	}
	return syscall.Exec(path, argv, os.Environ())
}
