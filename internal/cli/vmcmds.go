// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/GSI-HPC/go-clikit/fanout"
	"github.com/GSI-HPC/go-clikit/progress"
	"github.com/GSI-HPC/go-clikit/termtext"
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
and start it. Settings not given as flags come from config.yaml. Before it
does any work, it runs the checks of brig doctor that need no time and stops
if one fails.

Mounts share host directories with the VM via virtiofs:
  --mount SOURCE[:TARGET][:OPTIONS]
TARGET defaults to /mnt/<basename of SOURCE>. OPTIONS is a comma-separated
list of ro (default), rw and sandbox; sandbox also offers the directory to
OpenShell sandboxes, read-only, as a Podman volume named after the mount's
tag (see brig show and brig sandbox create --help).

OpenShell config directories hold provider profiles and providers, which
brig applies to the VM's gateway at every start, and a default sandbox
policy, which brig use NAME exports (see brig sync --help).
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
				m, err := parseMount(arg)
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
			// Before an image is built, which takes long.
			if err := checkHost(cmd.Context(), cmd.ErrOrStderr()); err != nil {
				return err
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
	_ = cmd.RegisterFlagCompletionFunc("mount", completeDirs)
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
	if err := a.provision(ctx, conn, v); err != nil {
		return err
	}
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

// provision creates the VM's directory with its keys and disks, saves its
// record and defines its domain. It removes the VM's files again if it fails
// before the domain is defined: a domain of that name that already exists is
// none of this command's business, and a VM whose domain is defined is kept,
// even if it does not start.
func (a *app) provision(ctx context.Context, conn domains, v *vm.VM) (err error) {
	ctx, step := progress.Start(ctx, progress.KindStep, "create the VM")
	defer func() { step.End(err) }()
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
	return nil
}

// writeKeys creates the VM's directory with its SSH client key, host key and
// known_hosts.
func (a *app) writeKeys(v *vm.VM) error {
	if err := paths.EnsurePrivate(a.vms.Dir(v.Name)); err != nil {
		return err
	}
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

// vmCommand builds a command that operates on one existing VM: the one NAME
// names, else the one that brig use selected.
func vmCommand(use, short string, run func(ctx context.Context, cmd *cobra.Command, a *app, v *vm.VM) error) *cobra.Command {
	return &cobra.Command{
		Use:               use,
		Short:             short,
		Args:              cobra.MaximumNArgs(1),
		ValidArgsFunction: completeVMs,
		RunE: func(cmd *cobra.Command, args []string) error {
			a, err := newApp()
			if err != nil {
				return err
			}
			v, err := a.resolveVMArg(cmd, args)
			if err != nil {
				return err
			}
			return run(cmd.Context(), cmd, a, v)
		},
	}
}

func newStartCmd() *cobra.Command {
	return vmCommand("start [NAME]", "Start a VM and connect its OpenShell gateway", func(ctx context.Context, cmd *cobra.Command, a *app, v *vm.VM) error {
		unlock, err := a.lockLoadedVM(v)
		if err != nil {
			return err
		}
		defer unlock()
		conn, err := a.connect(ctx)
		if err != nil {
			return err
		}
		defer func() { _ = conn.Close() }()
		if state, err := conn.State(v.DomainName()); err == nil && state != libvirt.StateRunning {
			// A forced power off may have left an upgrade's snapshot behind.
			a.pruneUpgradeSnapshots(ctx, v)
		}
		return a.start(ctx, conn, v, cmd.OutOrStdout())
	})
}

func newStopCmd() *cobra.Command {
	var force bool
	cmd := vmCommand("stop [NAME]", "Shut a VM down", func(ctx context.Context, _ *cobra.Command, a *app, v *vm.VM) error {
		unlock, err := a.lockLoadedVM(v)
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
	var force, yes bool
	cmd := vmCommand("delete [NAME]", "Delete a VM and all its data", func(ctx context.Context, cmd *cobra.Command, a *app, v *vm.VM) error {
		unlock, err := a.lockLoadedVM(v)
		if err != nil {
			return err
		}
		defer unlock()
		conn, err := a.connect(ctx)
		if err != nil {
			return err
		}
		defer func() { _ = conn.Close() }()
		var ask func(libvirt.State) error
		if confirm := confirmer(ctx, cmd.InOrStdin(), cmd.ErrOrStderr(), yes); confirm != nil {
			ask = func(state libvirt.State) error { return confirmDelete(cmd.ErrOrStderr(), confirm, v, state) }
		}
		return a.remove(ctx, conn, v, force, ask, cmd.OutOrStdout(), cmd.ErrOrStderr())
	})
	cmd.Aliases = []string{"rm"}
	cmd.Long = `Delete a VM with its root disk, its data disk and its gateway registration.
The data disk holds the gateway's encrypted secrets, the /sandbox of every
sandbox and every image pushed to the VM, so none of it can be got back.

On a terminal, delete asks first; --yes skips the question. Without a
terminal on standard input, as in a script, it deletes without asking.`
	cmd.Flags().BoolVarP(&force, "force", "f", false, "delete a running VM")
	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "do not ask for confirmation")
	return cmd
}

// confirmDelete says on w what deleting VM v, which is in state, removes,
// asks with confirm and fails if the answer is not yes.
func confirmDelete(w io.Writer, confirm func(prompt string) (bool, error), v *vm.VM, state libvirt.State) error {
	fmt.Fprintf(w, "Deleting VM %s removes its root disk (%s), its data disk (%s) and its gateway registration.\n", v.Name, v.RootDisk, v.DataDisk)
	fmt.Fprintln(w, "The data disk holds the gateway's secrets, every sandbox's /sandbox and every pushed image.")
	if state != libvirt.StateShutoff && state != libvirt.StateMissing {
		fmt.Fprintf(w, "VM %s is %s and will be powered off first.\n", v.Name, state)
	}
	ok, err := confirm("Delete VM " + termtext.Escape(v.Name) + "? [y/N] ")
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("VM %s not deleted", v.Name)
	}
	return nil
}

// remove deletes the VM, which must be shut off or missing unless force is
// set, with its domain, files and gateway. If ask is not nil, it is asked
// before anything is deleted, with the VM's state, and can refuse.
func (a *app) remove(ctx context.Context, conn domains, v *vm.VM, force bool, ask func(libvirt.State) error, stdout, stderr io.Writer) error {
	state, err := conn.State(v.DomainName())
	if err != nil {
		return err
	}
	if state != libvirt.StateShutoff && state != libvirt.StateMissing && !force {
		return fmt.Errorf("VM %s is %s; stop it first or use --force", v.Name, state)
	}
	if ask != nil {
		if err := ask(state); err != nil {
			return err
		}
	}
	if err := a.stop(ctx, conn, v, true); err != nil {
		return err
	}
	err = inSpan(ctx, progress.KindStep, "remove the VM", func(ctx context.Context) error {
		if err := conn.Undefine(v.DomainName()); err != nil {
			return err
		}
		if socket, err := netSocket(v); err == nil {
			_ = os.RemoveAll(filepath.Dir(socket))
		}
		// By now the VM is gone, so Ctrl-C must not leave its gateway behind.
		if err := a.forgetGateway(context.WithoutCancel(ctx), v); err != nil {
			fmt.Fprintf(stderr, "Warning: %s\n", termtext.Escape(err.Error()))
		}
		return a.vms.Remove(v.Name)
	})
	if err != nil {
		return err
	}
	fmt.Fprintf(stdout, "Deleted VM %s.\n", v.Name)
	return nil
}

// forgetGateway removes the VM's gateway from the host's openshell CLI.
func (a *app) forgetGateway(ctx context.Context, v *vm.VM) error {
	cfgHome, err := openshellConfigHome()
	if err != nil {
		return err
	}
	var errs []error
	if cli, err := openshell.Find(); err == nil {
		if err := cli.Unregister(ctx, v.GatewayName()); err != nil {
			errs = append(errs, fmt.Errorf("unregistering gateway %s: %w", v.GatewayName(), err))
		}
	}
	// The client key goes even when unregistering failed: delete drops the VM
	// record next, so nothing could retry.
	errs = append(errs, openshell.RemoveBundle(cfgHome, v.GatewayName()))
	return errors.Join(errs...)
}

// vmStatus is a VM record plus its live state, as printed by list and show.
type vmStatus struct {
	*vm.VM
	State   string `json:"state"`
	Gateway string `json:"gateway"`
	// OpenShellSync is the outcome of the last sync of the VM's OpenShell
	// config directories; only show reports it.
	OpenShellSync *ocsync.Record `json:"openshell_sync,omitempty"`
	// Stats is a sample of the resource use of a running VM; the JSON
	// output has it, and show prints the memory use.
	Stats *libvirt.Stats `json:"stats,omitempty"`
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
	var state func(string) (libvirt.State, error)
	if cerr == nil {
		state = conn.State
	}
	out := vmStatuses(ctx, vms, state)
	var serr error
	if cerr == nil {
		serr = addStats(out, conn.Stats)
	}
	return out, errors.Join(err, cerr, serr)
}

// addStats samples the VMs in sts that are running, with stats, which takes
// domain names. It leaves the others without stats.
func addStats(sts []vmStatus, stats func(domains []string) (map[string]libvirt.Stats, error)) error {
	var domains []string
	for _, st := range sts {
		if st.State == libvirt.StateRunning.String() {
			domains = append(domains, st.DomainName())
		}
	}
	if len(domains) == 0 {
		return nil
	}
	m, err := stats(domains)
	for i, st := range sts {
		if s, ok := m[st.DomainName()]; ok {
			sts[i].Stats = &s
		}
	}
	return err
}

// statusLimit is how many domain states status asks libvirt for at once.
const statusLimit = 8

// vmStatuses pairs the VMs with their states, as state tells them, which it
// does for several VMs at once. A VM whose state cannot be had, or all of
// them if state is nil or ctx ends first, is "unknown".
func vmStatuses(ctx context.Context, vms []*vm.VM, state func(domain string) (libvirt.State, error)) []vmStatus {
	out := make([]vmStatus, len(vms))
	for i, v := range vms {
		out[i] = vmStatus{VM: v, State: "unknown", Gateway: v.GatewayName()}
	}
	if state == nil {
		return out
	}
	// A libvirt connection serves concurrent calls, and each call writes
	// only its own element of out.
	fanout.Each(ctx, len(vms), statusLimit, func(i int) {
		if s, err := state(vms[i].DomainName()); err == nil {
			out[i].State = s.String()
		}
	})
	return out
}

// memoryUsed describes the memory use in s, or returns "" if s has none.
func memoryUsed(s *libvirt.Stats) string {
	if s == nil || s.Memory == nil {
		return ""
	}
	m := s.Memory
	// The host's view is the QEMU process's resident set, which overstates
	// what an idle guest uses.
	return fmt.Sprintf("%s of %s (%s view)", kib(m.UsedKiB), kib(m.TotalKiB), m.Source)
}

// kib formats n kibibytes with one decimal in the largest unit that keeps
// the number at 1 or more.
func kib(n uint64) string {
	switch f := float64(n); {
	case n >= uint64(bytesize.GiB/bytesize.KiB):
		return fmt.Sprintf("%.1f GiB", f/float64(bytesize.GiB/bytesize.KiB))
	case n >= uint64(bytesize.MiB/bytesize.KiB):
		return fmt.Sprintf("%.0f MiB", f/float64(bytesize.MiB/bytesize.KiB))
	}
	return fmt.Sprintf("%d KiB", n)
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
				fmt.Fprintf(cmd.ErrOrStderr(), "Warning: %s\n", termtext.Escape(err.Error()))
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
				fmt.Fprintf(tw, "%s\t%s\t%d\t%s\t%s\t%s\n", termtext.Escape(v.Name), termtext.Escape(v.State), v.CPUs, v.Memory,
					termtext.Escape(v.NetworkProfile), termtext.Escape(v.Image))
			}
			return tw.Flush()
		},
	}
	addOutputFlag(cmd, &format)
	return cmd
}

func newShowCmd() *cobra.Command {
	var format outputFormat
	cmd := vmCommand("show [NAME]", "Show a VM's settings and state", func(ctx context.Context, cmd *cobra.Command, a *app, v *vm.VM) error {
		st := vmStatus{VM: v, State: "unknown", Gateway: v.GatewayName(), OpenShellSync: a.lastSync(v)}
		if conn, err := a.connect(ctx); err == nil {
			defer func() { _ = conn.Close() }()
			if s, err := conn.State(v.DomainName()); err == nil {
				st.State = s.String()
			}
			one := []vmStatus{st}
			if err := addStats(one, conn.Stats); err != nil {
				fmt.Fprintf(cmd.ErrOrStderr(), "Warning: %s\n", termtext.Escape(err.Error()))
			}
			st = one[0]
		}
		if format == "json" {
			return writeJSON(cmd.OutOrStdout(), st)
		}
		tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
		row := func(k, v any) { fmt.Fprintf(tw, "%v:\t%s\n", k, termtext.Escape(fmt.Sprint(v))) }
		row("Name", v.Name)
		row("State", st.State)
		row("Created", v.CreatedAt.Local().Format(time.DateTime))
		row("Image", v.Image)
		row("CPUs", v.CPUs)
		row("Memory", v.Memory)
		if m := memoryUsed(st.Stats); m != "" {
			row("Memory used", m)
		}
		row("Root disk", v.RootDisk)
		row("Data disk", v.DataDisk)
		if p, err := a.cfg.Profile(v.NetworkProfile); err == nil {
			row("Network profile", fmt.Sprintf("%s (allows %s)", v.NetworkProfile, p.Allows()))
		} else {
			row("Network profile", v.NetworkProfile)
		}
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
	cmd := vmCommand("update [NAME]", "Change a VM's resources, mounts, network profile or OpenShell config directories", func(ctx context.Context, cmd *cobra.Command, a *app, v *vm.VM) error {
		unlock, err := a.lockLoadedVM(v)
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
			m, err := parseMount(arg)
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
		if state == libvirt.StateOther {
			return fmt.Errorf("VM %s is in an unexpected state (for instance suspended by the guest); stop it first", v.Name)
		}
		running := state == libvirt.StateRunning
		if running {
			// Mounts change at once, so refuse before anything changes.
			removed, _ := mountChanges(old.Mounts, v.Mounts)
			if err := a.checkVolumesUnused(ctx, v, removed); err != nil {
				return err
			}
		}
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
		// These flags take effect at once on a running VM.
		liveFlags := 0
		for _, name := range []string{"add-openshell-config", "remove-openshell-config", "add-mount", "remove-mount"} {
			if f.Changed(name) {
				liveFlags++
			}
		}
		if running && (f.NFlag() == 0 || f.NFlag() > liveFlags) {
			// The guest grows its file systems at boot.
			fmt.Fprintf(w, "The changes, including the space on grown disks, apply when %s next starts: brig stop %[1]s && brig start %[1]s\n", v.Name)
		}
		if running {
			restart, err := a.applyMounts(ctx, conn, v, old.Mounts)
			if err != nil {
				return fmt.Errorf("changing the mounts of running VM %s failed, so they change when it next starts (brig stop %[1]s && brig start %[1]s): %w", v.Name, err)
			}
			if restart {
				fmt.Fprintf(w, "Sandboxes can attach the volumes of sandbox mounts once %s restarts: brig stop %[1]s && brig start %[1]s\n", v.Name)
			}
		}
		if running && !slices.Equal(old.OpenShellConfigs, v.OpenShellConfigs) {
			fmt.Fprintf(w, "Apply the OpenShell config directories now with `brig sync %s`; brig start applies them too.\n", v.Name)
		}
		return nil
	})
	cmd.Long = `Change a VM's settings.

On a running VM, mounts change at once, and --add-openshell-config and
--remove-openshell-config take effect with brig sync NAME. CPUs, memory, the
network profile and the space on grown disks change when the VM next starts.
A sandbox mount cannot be removed from a running VM while a sandbox uses its
volume.`
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
	_ = cmd.RegisterFlagCompletionFunc("add-mount", completeDirs)
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
// first argument, or else selected by brig use.
func completeMountTargets(_ *cobra.Command, args []string, _ string) ([]string, cobra.ShellCompDirective) {
	v := completionVM(args)
	if v == nil {
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
		Use:   "ssh [NAME] [-- COMMAND...]",
		Short: "Open a shell in a VM, or run a command there",
		Long: `Open a shell in a VM as user agent, which runs the OpenShell gateway and
owns the persistent home directory, or as root with --root. Without NAME,
the VM is the one that brig use selected; a command then needs the --.`,
		Example: `  brig ssh dev
  brig ssh dev -- systemctl --user status openshell-gateway
  brig ssh dev --root -- journalctl -b
  brig ssh -- journalctl -b`,
		ValidArgsFunction: completeVMs,
		RunE: func(cmd *cobra.Command, args []string) error {
			a, err := newApp()
			if err != nil {
				return err
			}
			// Without a --, the first argument is the VM, as before.
			named, command := args, []string(nil)
			if dash := cmd.ArgsLenAtDash(); dash >= 0 {
				named, command = args[:dash], args[dash:]
			} else if len(args) > 0 {
				named, command = args[:1], args[1:]
			}
			if len(named) > 1 {
				return fmt.Errorf("accepts at most one VM name before --, received %d", len(named))
			}
			v, err := a.resolveVMArg(cmd, named)
			if err != nil {
				return err
			}
			return execve("ssh", a.sshTarget(v, root).Interactive(command...))
		},
	}
	cmd.Flags().BoolVar(&root, "root", false, "log in as root")
	return cmd
}

func newConsoleCmd() *cobra.Command {
	return vmCommand("console [NAME]", "Attach to a VM's serial console (exit with Ctrl+])", func(_ context.Context, _ *cobra.Command, _ *app, v *vm.VM) error {
		return execve("virsh", []string{"virsh", "--connect", "qemu:///session", "console", v.DomainName()})
	})
}

func newEnvCmd() *cobra.Command {
	var (
		shell string
		unset bool
	)
	cmd := envCommand("env NAME", "Print shell commands that point the openshell CLI at a VM", &unset, func() (string, error) {
		sh := shell
		if sh == "" {
			sh = "sh"
			if filepath.Base(os.Getenv("SHELL")) == "fish" {
				sh = "fish"
			}
		}
		if sh != "sh" && sh != "fish" {
			return "", fmt.Errorf("--shell must be sh or fish, not %q", sh)
		}
		return sh, nil
	})
	cmd.Long = `Print shell commands that point the openshell CLI at a VM's gateway:
OPENSHELL_GATEWAY, and OPENSHELL_SANDBOX_POLICY when one of the VM's
OpenShell config directories has a policies/default.yaml (else it is
unset, so that no other VM's default policy applies). It fails, and
prints nothing, if a config directory does not load. Use it as
eval "$(brig env NAME)"; without the quotes, the shell splits paths
that contain spaces. With --unset instead of NAME, it prints commands
that unset both variables.

The commands are for fish when $SHELL is fish, else for POSIX shells such
as bash and zsh; --shell picks them instead.

brig use NAME does the same without eval, once brig's shell integration
is loaded (see brig shell-init --help).`
	cmd.Flags().StringVar(&shell, "shell", "", "print commands for this shell: sh or fish (default: from $SHELL)")
	_ = cmd.RegisterFlagCompletionFunc("shell", cobra.FixedCompletions([]string{"sh", "fish"}, cobra.ShellCompDirectiveNoFileComp))
	return cmd
}

// shellIntegrationEnv names the variable through which the brig function that
// brig shell-init defines tells brig use the syntax of the shell that
// evaluates its output.
const shellIntegrationEnv = "BRIG_SHELL_INTEGRATION"

func newUseCmd() *cobra.Command {
	var unset bool
	cmd := envCommand("use [NAME]", "Point the openshell CLI in this shell at a VM, or show which one", &unset, func() (string, error) {
		switch sh := os.Getenv(shellIntegrationEnv); sh {
		case "sh", "fish":
			return sh, nil
		case "":
			shell := filepath.Base(os.Getenv("SHELL"))
			if !slices.Contains(shellInitShells, shell) {
				shell = "bash"
			}
			return "", fmt.Errorf("brig use needs brig's shell integration: add eval \"$(brig shell-init %s)\" to your shell's startup file (see brig shell-init --help), or run eval \"$(brig env NAME)\" instead", shell)
		default:
			return "", fmt.Errorf("%s must be sh or fish, not %q", shellIntegrationEnv, sh)
		}
	})
	cmd.Long = `Point the openshell CLI in this shell at VM NAME's gateway, as
eval "$(brig env NAME)" does, or stop pointing it at any VM with --unset.
Without NAME or --unset, it tells which VM this shell points at, on standard
error; it fails if there is none.

Commands that take a VM NAME work on that VM when you leave NAME out.

A program cannot change its shell's environment, so brig use needs brig's
shell integration, which defines a brig shell function that runs brig use
and evaluates what it prints (see brig shell-init --help).`
	// What brig use writes to standard output is evaluated by the shell, so
	// its help goes to standard error.
	help := cmd.HelpFunc()
	cmd.SetHelpFunc(func(c *cobra.Command, args []string) {
		c.SetOut(c.ErrOrStderr())
		help(c, args)
	})
	return cmd
}

// envCommand returns a command that prints shell commands that point the
// openshell CLI at the VM its argument names, or, if *unset is set by the
// --unset flag it defines, that unset what they set. If use ends in "[NAME]",
// it may be run without either, and then tells which VM the shell points at.
// shell returns the shell to write for, "sh" or "fish".
func envCommand(use, short string, unset *bool, shell func() (string, error)) *cobra.Command {
	optional := strings.HasSuffix(use, "[NAME]")
	var workspace string
	cmd := &cobra.Command{
		Use:   use,
		Short: short,
		Args: func(cmd *cobra.Command, args []string) error {
			switch {
			case *unset:
				return cobra.NoArgs(cmd, args)
			case optional:
				return cobra.MaximumNArgs(1)(cmd, args)
			}
			return cobra.ExactArgs(1)(cmd, args)
		},
		ValidArgsFunction: completeVMs,
		RunE: func(cmd *cobra.Command, args []string) error {
			name := ""
			if len(args) > 0 {
				name = args[0]
			}
			if optional && !*unset && name == "" {
				// What is printed to standard output is evaluated by the
				// shell, so the answer goes to standard error.
				if workspace == "" {
					return showSelectedVM(cmd.ErrOrStderr())
				}
				// -w alone moves to a workspace of the VM already selected.
				var err error
				if name, err = selectedVM(); err != nil {
					return err
				}
			}
			if workspace != "" {
				if err := openshell.CheckWorkspace(workspace); err != nil {
					return err
				}
			}
			sh, err := shell()
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			set, unsetVars := func(key, value string) {
				fmt.Fprintf(out, "export %s=%s\n", key, value)
			}, func(keys ...string) {
				fmt.Fprintf(out, "unset %s\n", strings.Join(keys, " "))
			}
			if sh == "fish" {
				set, unsetVars = func(key, value string) {
					fmt.Fprintf(out, "set -gx %s %s\n", key, value)
				}, func(keys ...string) {
					fmt.Fprintf(out, "set -e %s\n", strings.Join(keys, " "))
				}
			}
			if *unset {
				unsetVars(gatewayEnv, "OPENSHELL_SANDBOX_POLICY", workspaceEnv, openshellWorkspaceEnv)
				return nil
			}
			a, err := newApp()
			if err != nil {
				return err
			}
			v, err := a.vms.Load(name)
			if err != nil {
				return err
			}
			// Nothing is printed for a config directory that does not
			// load, so the shell keeps its variables, and the error is
			// shown as create and sync show theirs.
			policy, err := defaultPolicy(v)
			if err != nil {
				return err
			}
			set(gatewayEnv, v.GatewayName())
			// Unset what another VM's environment exported: a default
			// policy, and a workspace, which belongs to one gateway.
			switch {
			case policy == "":
				unsetVars("OPENSHELL_SANDBOX_POLICY")
			case sh == "fish":
				set("OPENSHELL_SANDBOX_POLICY", fishQuote(policy))
			default:
				set("OPENSHELL_SANDBOX_POLICY", shellQuote(policy))
			}
			if workspace == "" {
				unsetVars(workspaceEnv, openshellWorkspaceEnv)
			} else {
				set(workspaceEnv, workspace)
				set(openshellWorkspaceEnv, workspace)
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(unset, "unset", false, "unset the variables instead, without a NAME")
	cmd.Flags().StringVarP(&workspace, "workspace", "w", "", "export "+workspaceEnv+" and "+openshellWorkspaceEnv+", the workspace that brig and openshell work in by default (without NAME, keep the selected VM)")
	cmd.MarkFlagsMutuallyExclusive("unset", "workspace")
	_ = cmd.RegisterFlagCompletionFunc("workspace", completeWorkspaces)
	return cmd
}

// showSelectedVM tells on w which VM the shell points at.
func showSelectedVM(w io.Writer) error {
	name, err := selectedVM()
	if errors.Is(err, errNoVMSelected) {
		return errors.New("this shell points at no VM; select one with brig use NAME")
	}
	if err != nil {
		return err
	}
	ws, err := envWorkspace()
	if err != nil {
		return err
	}
	if ws != "" {
		fmt.Fprintf(w, "This shell points at VM %s, workspace %s.\n", termtext.Escape(name), ws)
		return nil
	}
	fmt.Fprintf(w, "This shell points at VM %s.\n", termtext.Escape(name))
	return nil
}

// defaultPolicy returns the default sandbox policy file of v's OpenShell
// config directories, or "" if they have none. A config directory that does
// not load is an error, not a VM without a default policy.
func defaultPolicy(v *vm.VM) (string, error) {
	if len(v.OpenShellConfigs) == 0 {
		return "", nil
	}
	set, err := ocsync.Load(v.OpenShellConfigs)
	if err != nil {
		return "", err
	}
	return set.DefaultPolicy, nil
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
