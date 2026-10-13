// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/dennisklein/brig/internal/libvirt"
	"github.com/dennisklein/brig/internal/openshell"
	"github.com/dennisklein/brig/internal/vm"
)

func newSandboxCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "sandbox",
		Short: "Create, list and work in OpenShell sandboxes",
	}
	cmd.AddCommand(newSandboxCreateCmd(), newSandboxListCmd(), newSandboxConnectCmd(), newSandboxExecCmd(), newSandboxLogsCmd(),
		newSandboxStartCmd(), newSandboxStopCmd(), newSandboxDeleteCmd(), newSandboxAttachCmd(), newSandboxDetachCmd())
	return cmd
}

func newSandboxCreateCmd() *cobra.Command {
	var (
		mounts    []string
		workspace string
		connect   bool
	)
	cmd := &cobra.Command{
		Use:   "create [NAME] [-w W] [--mount DIR:TARGET]... [--connect] [-- OPENSHELL-ARGS...]",
		Short: "Create an OpenShell sandbox in a VM, with host directories mounted read-only",
		Long: `Run openshell sandbox create against VM NAME's gateway, with each
--mount DIR:TARGET showing host directory DIR read-only at TARGET in the
sandbox. Arguments after -- go to openshell sandbox create as they are.
Without NAME, the VM is the one that brig use selected.

The sandbox is created in workspace W of the gateway (-w), by default the one
that $BRIG_WORKSPACE names, else default.

A sandbox sees DIR through a read-only sandbox mount of the VM (see brig
create --help). brig uses the VM's sandbox mount of DIR, or adds one to the
running VM, at /mnt/<tag> in the VM, which stays until brig update
--remove-mount removes it. As brig env does, it points openshell at the VM's
default sandbox policy, if it has one.

The sandbox gets the providers that --provider names after --, and no
others: as brig ui does, brig passes --no-auto-providers, so that a provider
the gateway lacks is an error rather than one that openshell creates from
credentials it finds on the host. Give --auto-providers after -- for
openshell's own behaviour.

With --connect, brig starts the VM first if it is stopped, as brig start
--wait does, creates the sandbox without attaching to it, waits until it is
ready and then connects this terminal to it, as brig sandbox connect does.
It needs the sandbox's --name after --.`,
		Example: `  brig sandbox create dev --mount ~/src/pi-kit:/usr/local/share/pi-kit -- \
    --name kit-dev --from localhost/pi-agent:2026-10-08 --provider llm
  brig use dev && brig sandbox create -m ~/notes:/usr/local/share/notes
  brig sandbox create dev --connect -m ~/src/webapp:/sandbox/src -- --name webapp --template pi`,
		Args: func(cmd *cobra.Command, args []string) error {
			if n := len(vmArgs(cmd, args)); n > 1 {
				return fmt.Errorf("accepts at most one VM name before --, received %d", n)
			}
			return nil
		},
		ValidArgsFunction: completeVMs,
		RunE: func(cmd *cobra.Command, args []string) error {
			pass := args[len(vmArgs(cmd, args)):]
			if err := checkSandboxArgs(pass); err != nil {
				return err
			}
			name := sandboxNameArg(pass)
			if connect && name == "" {
				return errors.New("--connect needs the sandbox's --name after --")
			}
			ws, err := chosenWorkspace(workspace)
			if err != nil {
				return err
			}
			var specs []sandboxMount
			for _, arg := range mounts {
				m, err := parseSandboxMount(arg)
				if err != nil {
					return err
				}
				specs = append(specs, m)
			}
			a, err := newApp()
			if err != nil {
				return err
			}
			v, err := a.resolveVMArg(cmd, vmArgs(cmd, args))
			if err != nil {
				return err
			}
			if connect {
				if err := startIfStopped(cmd.Context(), a, v, cmd.ErrOrStderr()); err != nil {
					return err
				}
			}
			config, err := a.sandboxMounts(cmd.Context(), v, specs, cmd.ErrOrStderr())
			if err != nil {
				return err
			}
			policy, err := defaultPolicy(v)
			if err != nil {
				return err
			}
			if policy == "" {
				err = os.Unsetenv("OPENSHELL_SANDBOX_POLICY")
			} else {
				err = os.Setenv("OPENSHELL_SANDBOX_POLICY", policy)
			}
			if err != nil {
				return err
			}
			argv := append([]string{"openshell"}, openshell.WorkspaceArgs(v.GatewayName(), ws)...)
			argv = append(argv, "sandbox", "create")
			if config != "" {
				argv = append(argv, "--driver-config-json", config)
			}
			if !choosesProviders(pass) {
				argv = append(argv, "--no-auto-providers")
			}
			if !connect {
				return execve("openshell", append(argv, pass...))
			}
			if !slices.Contains(sandboxOptions(pass), "--detach") {
				argv = append(argv, "--detach")
			}
			if err := runCommand(cmd.Context(), append(argv, pass...)); err != nil {
				return err
			}
			if err := waitSandboxReady(cmd.Context(), v, ws, name); err != nil {
				return err
			}
			t := sandboxTarget{vm: v, workspace: ws}
			return execve("openshell", t.openshell("sandbox", "connect", name))
		},
	}
	addWorkspaceFlag(cmd, &workspace)
	cmd.Flags().StringArrayVarP(&mounts, "mount", "m", nil, "show host directory DIR read-only at TARGET in the sandbox: DIR:TARGET (repeatable)")
	_ = cmd.RegisterFlagCompletionFunc("mount", completeDirs)
	cmd.Flags().BoolVar(&connect, "connect", false, "start the VM if needed, wait until the sandbox is ready and connect to it")
	return cmd
}

// sandboxOptions returns the openshell options in args, which end at --.
func sandboxOptions(args []string) []string {
	if i := slices.Index(args, "--"); i >= 0 {
		return args[:i]
	}
	return args
}

// sandboxNameArg returns the --name that the openshell options in args give,
// or "".
func sandboxNameArg(args []string) string {
	opts := sandboxOptions(args)
	for i, arg := range opts {
		if v, ok := strings.CutPrefix(arg, "--name="); ok {
			return v
		}
		if arg == "--name" && i+1 < len(opts) {
			return opts[i+1]
		}
	}
	return ""
}

// startIfStopped starts v, as brig start --wait does, unless it runs; tests
// replace it.
var startIfStopped = func(ctx context.Context, a *app, v *vm.VM, w io.Writer) error {
	conn, err := a.connect(ctx)
	if err != nil {
		return err
	}
	state, err := conn.State(v.DomainName())
	_ = conn.Close()
	if err != nil || state == libvirt.StateRunning {
		return err
	}
	return a.lockAndStart(ctx, v, w, readySynced)
}

// runCommand runs argv, whose first element names the program, on brig's
// terminal; tests replace it.
var runCommand = func(ctx context.Context, argv []string) error {
	c := exec.CommandContext(ctx, argv[0], argv[1:]...)
	c.Stdin, c.Stdout, c.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := c.Run(); err != nil {
		return fmt.Errorf("%s: %w", strings.Join(argv[:min(len(argv), 8)], " "), err)
	}
	return nil
}

// sandboxReadyTimeout bounds the wait for a new sandbox; OpenShell gives up
// provisioning one after 300 s.
var sandboxReadyTimeout = 6 * time.Minute

// waitSandboxReady waits until the sandbox called name in workspace ws of
// v's gateway is ready, failing early if it is in error.
func waitSandboxReady(ctx context.Context, v *vm.VM, ws, name string) error {
	ctx, cancel := context.WithTimeout(ctx, sandboxReadyTimeout)
	defer cancel()
	pause := 500 * time.Millisecond
	for {
		s, err := findSandbox(ctx, v, ws, name)
		switch {
		case err == nil && s.Ready():
			return nil
		case err == nil && s.Phase == "Error":
			return notReadyf("sandbox %s is in error: %s", name, s.Problem)
		case err == nil:
			err = notReadyf("sandbox %s is %s", name, strings.ToLower(s.Phase))
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("waiting for sandbox %s to be ready: %w", name, err)
		case <-time.After(pause):
		}
		pause = min(2*pause, 5*time.Second)
	}
}

// vmArgs returns the arguments before --.
func vmArgs(cmd *cobra.Command, args []string) []string {
	if dash := cmd.ArgsLenAtDash(); dash >= 0 {
		return args[:dash]
	}
	return args
}

// choosesProviders reports whether the openshell options args say whether
// openshell may create missing providers.
func choosesProviders(args []string) bool {
	for _, arg := range args {
		switch arg {
		case "--":
			return false
		case "--auto-providers", "--no-auto-providers":
			return true
		}
	}
	return false
}

// checkSandboxArgs rejects openshell options that brig sets itself. Options
// end at --, after which the sandbox's command follows.
func checkSandboxArgs(args []string) error {
	for _, arg := range args {
		if arg == "--" {
			break
		}
		opt, _, _ := strings.Cut(arg, "=")
		switch opt {
		case "-g", "--gateway":
			return fmt.Errorf("%s: brig picks the gateway from the VM", opt)
		case "--workspace":
			return fmt.Errorf("%s: give it to brig, before --", opt)
		case "--driver-config-json":
			return fmt.Errorf("%s: brig sets it from --mount", opt)
		}
	}
	return nil
}

// sandboxMount is a --mount of brig sandbox create.
type sandboxMount struct {
	// Source is the host directory, absolute and without symbolic links.
	Source string
	// Target is where the sandbox sees it.
	Target string
}

func parseSandboxMount(arg string) (sandboxMount, error) {
	src, target, ok := strings.Cut(arg, ":")
	if !ok || src == "" || strings.Contains(target, ":") {
		return sandboxMount{}, fmt.Errorf("invalid mount %q: want DIR:TARGET", arg)
	}
	if !path.IsAbs(target) || path.Clean(target) != target || target == "/" {
		return sandboxMount{}, fmt.Errorf("invalid mount %q: target %q must be a clean absolute path other than /", arg, target)
	}
	src, err := filepath.Abs(src)
	if err != nil {
		return sandboxMount{}, err
	}
	// The VM's mounts hold resolved sources (see parseMount).
	if resolved, err := filepath.EvalSymlinks(src); err == nil {
		src = resolved
	}
	return sandboxMount{Source: src, Target: target}, nil
}

// planSandboxMounts returns the mounts that v needs for specs: its own, plus
// a read-only sandbox mount at /mnt/<tag> for each source that has none yet,
// and the Podman volume of each spec, in order. added reports new mounts.
func planSandboxMounts(v *vm.VM, specs []sandboxMount) (mounts []vm.Mount, volumes []string, added bool) {
	nv := *v
	nv.Mounts = slices.Clone(v.Mounts)
	find := func(src string) int {
		return slices.IndexFunc(nv.Mounts, func(m vm.Mount) bool { return m.Sandbox && m.Source == src })
	}
	for _, s := range specs {
		if find(s.Source) < 0 {
			nv.Mounts = append(nv.Mounts, vm.Mount{Source: s.Source, ReadOnly: true, Sandbox: true})
			added = true
		}
	}
	nv.AssignMountTags()
	for i, m := range nv.Mounts {
		if m.Target == "" {
			nv.Mounts[i].Target = "/mnt/" + m.Tag
		}
	}
	for _, s := range specs {
		volumes = append(volumes, nv.Mounts[find(s.Source)].Tag)
	}
	return nv.Mounts, volumes, added
}

// driverConfig returns the --driver-config-json of openshell sandbox create
// that attaches volumes at the targets of specs, or "" without specs.
func driverConfig(specs []sandboxMount, volumes []string) (string, error) {
	if len(specs) == 0 {
		return "", nil
	}
	type mount struct {
		Type   string `json:"type"`
		Source string `json:"source"`
		Target string `json:"target"`
	}
	var c struct {
		Podman struct {
			Mounts []mount `json:"mounts"`
		} `json:"podman"`
	}
	for i, s := range specs {
		c.Podman.Mounts = append(c.Podman.Mounts, mount{Type: "volume", Source: volumes[i], Target: s.Target})
	}
	b, err := json.Marshal(c)
	return string(b), err
}

// sandboxMounts makes sure that VM v has a sandbox mount for each of specs,
// adding the missing ones to the running VM, and returns the driver
// configuration that attaches their volumes. It reports added mounts to w.
func (a *app) sandboxMounts(ctx context.Context, v *vm.VM, specs []sandboxMount, w io.Writer) (string, error) {
	if _, volumes, added := planSandboxMounts(v, specs); !added {
		return driverConfig(specs, volumes)
	}
	unlock, err := a.lockLoadedVM(v)
	if err != nil {
		return "", err
	}
	defer unlock()
	// Plan again: another command may have changed the VM meanwhile.
	mounts, volumes, _ := planSandboxMounts(v, specs)
	old := *v
	v.Mounts = mounts
	v.AssignMountTags() // advances NextMountTag past the new tags
	if err := checkVM(v); err != nil {
		return "", err
	}
	conn, err := a.connect(ctx)
	if err != nil {
		return "", err
	}
	defer func() { _ = conn.Close() }()
	state, err := conn.State(v.DomainName())
	if err != nil {
		return "", err
	}
	if state != libvirt.StateRunning {
		return "", notReadyf("VM %s is %s; start it with brig start %[1]s", v.Name, state)
	}
	if err := a.vms.Save(v); err != nil {
		return "", err
	}
	if err := a.define(conn, v); err != nil {
		_ = a.vms.Save(&old)
		return "", err
	}
	restart, err := a.applyMounts(ctx, conn, v, old.Mounts)
	if err != nil {
		return "", fmt.Errorf("mounting in running VM %s failed, so the mounts appear when it next starts (brig stop %[1]s && brig start %[1]s): %w", v.Name, err)
	}
	_, added := mountChanges(old.Mounts, v.Mounts)
	for _, m := range added {
		fmt.Fprintf(w, "Mounted %s in VM %s at %s, as sandbox volume %s.\n", m.Source, v.Name, m.Target, m.Tag)
	}
	if restart {
		return "", fmt.Errorf("sandboxes can attach the volumes of sandbox mounts once VM %s restarts: brig stop %[1]s && brig start %[1]s", v.Name)
	}
	return driverConfig(specs, volumes)
}
