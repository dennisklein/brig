// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/GSI-HPC/go-clikit/fanout"
	"github.com/GSI-HPC/go-clikit/termtext"
	"github.com/spf13/cobra"

	"github.com/dennisklein/brig/internal/libvirt"
	"github.com/dennisklein/brig/internal/openshell"
	"github.com/dennisklein/brig/internal/osclient"
	"github.com/dennisklein/brig/internal/ui/action"
	"github.com/dennisklein/brig/internal/ui/store"
	"github.com/dennisklein/brig/internal/vm"
)

// sandboxTarget is the sandbox that a brig sandbox command works on.
type sandboxTarget struct {
	vm        *vm.VM
	workspace string
	sandbox   store.Sandbox
}

// openshell returns the openshell command line that runs args on the
// target's gateway, in its workspace.
func (t sandboxTarget) openshell(args ...string) []string {
	argv := append([]string{"openshell"}, openshell.WorkspaceArgs(t.vm.GatewayName(), t.workspace)...)
	return append(argv, args...)
}

// sandboxVerb says what a brig sandbox command that works on one sandbox
// takes and needs.
type sandboxVerb struct {
	use, short string
	// extra counts the arguments after SANDBOX, before --.
	extra int
	// command says that a command follows --.
	command bool
	// ready says that the sandbox must be ready.
	ready bool
}

// sandboxCommand builds a brig sandbox command that works on one sandbox of
// a VM: its arguments before -- are [VM] SANDBOX and then verb.extra more,
// and the VM defaults to the one that brig use selected. Before run, it
// checks that the gateway answers and has the sandbox in the workspace that
// -w or BRIG_WORKSPACE names, and that the sandbox is ready if the verb needs it. run
// gets the arguments after SANDBOX, those after -- included.
func sandboxCommand(verb sandboxVerb, run func(cmd *cobra.Command, t sandboxTarget, rest []string) error) *cobra.Command {
	var workspace string
	cmd := &cobra.Command{
		Use:   verb.use,
		Short: verb.short,
		Args: func(cmd *cobra.Command, args []string) error {
			pos := vmArgs(cmd, args)
			switch n := len(pos); {
			case n < verb.extra+1 || n > verb.extra+2:
				return fmt.Errorf("accepts %d or %d arguments before --, received %d", verb.extra+1, verb.extra+2, n)
			case verb.command && len(args) == n:
				return errors.New("give the command after --")
			case !verb.command && len(args) > n:
				return errors.New("takes no command after --")
			}
			return nil
		},
		ValidArgsFunction: completeVMAndSandbox,
		RunE: func(cmd *cobra.Command, args []string) error {
			ws, err := chosenWorkspace(workspace)
			if err != nil {
				return err
			}
			pos := vmArgs(cmd, args)
			named := pos[:len(pos)-verb.extra-1]
			name, rest := pos[len(named)], args[len(named)+1:]
			if !action.ValidSandboxName(name) {
				return fmt.Errorf("invalid sandbox name %q", name)
			}
			a, err := newApp()
			if err != nil {
				return err
			}
			v, err := a.resolveVMArg(cmd, named)
			if err != nil {
				return err
			}
			s, err := findSandbox(cmd.Context(), v, ws, name)
			if err != nil {
				return err
			}
			if verb.ready && !s.Ready() {
				return notReadyf("sandbox %s is %s; start it with brig sandbox start %s %[1]s", name, strings.ToLower(s.Phase), v.Name)
			}
			return run(cmd, sandboxTarget{vm: v, workspace: ws, sandbox: s}, rest)
		},
	}
	addWorkspaceFlag(cmd, &workspace)
	// These replace brig with openshell, which owns the terminal from then.
	cmd.Annotations = map[string]string{noProgress: "true"}
	return cmd
}

// gatewaySandboxes lists the sandboxes in every workspace of v's gateway;
// tests replace it.
var gatewaySandboxes = func(ctx context.Context, v *vm.VM) ([]store.Sandbox, error) {
	c, err := dialGateway(ctx, v)
	if err != nil {
		return nil, err
	}
	defer func() { _ = c.Close() }()
	return c.Sandboxes(ctx)
}

// dialGateway connects to v's gateway with the client bundle that brig start
// copied, as brig ui does, and checks that it answers.
func dialGateway(ctx context.Context, v *vm.VM) (*osclient.Client, error) {
	c, err := gatewayClient(v)
	if err != nil {
		return nil, notReadyf("cannot reach the gateway of VM %s; start it with brig start %[1]s: %w", v.Name, err)
	}
	if _, err := c.Version(ctx); err != nil {
		_ = c.Close()
		return nil, notReadyf("the gateway of VM %s does not answer; is the VM running (brig start %[1]s)? %w", v.Name, err)
	}
	return c, nil
}

// findSandbox returns the sandbox called name in workspace ws of v's
// gateway.
func findSandbox(ctx context.Context, v *vm.VM, ws, name string) (store.Sandbox, error) {
	all, err := gatewaySandboxes(ctx, v)
	if err != nil {
		return store.Sandbox{}, err
	}
	var elsewhere []string
	for _, s := range all {
		switch {
		case s.Name != name:
		case s.Workspace == ws:
			return s, nil
		default:
			elsewhere = append(elsewhere, s.Workspace)
		}
	}
	err = fmt.Errorf("VM %s has no sandbox %s in workspace %s", v.Name, name, ws)
	if len(elsewhere) > 0 {
		err = fmt.Errorf("%w; it has one in workspace %s (-w %[2]s)", err, strings.Join(elsewhere, ", "))
	}
	return store.Sandbox{}, notFound(err)
}

// notFound marks err as about something that does not exist: a VM, an
// image, a sandbox or a workspace.
func notFound(err error) error { return withCode(exitNotFound, err) }

func newSandboxConnectCmd() *cobra.Command {
	cmd := sandboxCommand(sandboxVerb{use: "connect [VM] SANDBOX", short: "Open a terminal in a sandbox", ready: true}, func(_ *cobra.Command, t sandboxTarget, _ []string) error {
		return execve("openshell", t.openshell("sandbox", "connect", t.sandbox.Name))
	})
	cmd.Long = `Attach this terminal to a sandbox's main process, as openshell sandbox
connect does. Ctrl-P Ctrl-Q, or Ctrl-D, detaches and leaves it running.`
	cmd.Example = `  brig sandbox connect dev webapp
  brig use dev && brig sandbox connect webapp`
	return cmd
}

func newSandboxExecCmd() *cobra.Command {
	var (
		workdir      string
		tty, noTTY   bool
		timeout      int
		env          []string
		noLoginShell bool
	)
	cmd := sandboxCommand(sandboxVerb{use: "exec [VM] SANDBOX -- COMMAND [ARG...]", short: "Run a command in a sandbox", command: true, ready: true}, func(_ *cobra.Command, t sandboxTarget, rest []string) error {
		args := []string{"sandbox", "exec", "--name", t.sandbox.Name}
		if workdir != "" {
			args = append(args, "--workdir", workdir)
		}
		switch {
		case tty:
			args = append(args, "--tty")
		case noTTY:
			args = append(args, "--no-tty")
		}
		if timeout > 0 {
			args = append(args, "--timeout", strconv.Itoa(timeout))
		}
		for _, e := range env {
			args = append(args, "--env", e)
		}
		if noLoginShell {
			args = append(args, "--no-login-shell")
		}
		return execve("openshell", t.openshell(append(append(args, "--"), rest...)...))
	})
	cmd.Long = `Run a command in a ready sandbox, as openshell sandbox exec does, and exit
with its exit status. brig's own failures, before the command starts, exit
with the statuses that brig --help lists.`
	cmd.Example = `  brig sandbox exec dev webapp -- git -C webapp status
  brig sandbox exec webapp --tty -- bash -l`
	f := cmd.Flags()
	f.StringVar(&workdir, "workdir", "", "run the command in this directory of the sandbox")
	f.BoolVar(&tty, "tty", false, "give the command a terminal (default: when standard input and output are terminals)")
	f.BoolVar(&noTTY, "no-tty", false, "give the command no terminal")
	f.IntVar(&timeout, "timeout", 0, "stop the command after this many seconds (default: no limit)")
	f.StringArrayVar(&env, "env", nil, "set a non-secret environment variable: KEY=VALUE (repeatable); attach a provider for secrets")
	f.BoolVar(&noLoginShell, "no-login-shell", false, "run the command without the sandbox user's login startup files")
	cmd.MarkFlagsMutuallyExclusive("tty", "no-tty")
	return cmd
}

func newSandboxLogsCmd() *cobra.Command {
	var (
		follow  bool
		lines   int
		since   string
		sources []string
		level   string
	)
	cmd := sandboxCommand(sandboxVerb{use: "logs [VM] SANDBOX", short: "Show a sandbox's logs, such as the requests its policy denied"}, func(_ *cobra.Command, t sandboxTarget, _ []string) error {
		args := []string{"logs", t.sandbox.Name, "-n", strconv.Itoa(lines)}
		if follow {
			args = append(args, "--tail")
		}
		if since != "" {
			args = append(args, "--since", since)
		}
		for _, s := range sources {
			args = append(args, "--source", s)
		}
		if level != "" {
			args = append(args, "--level", level)
		}
		return execve("openshell", t.openshell(args...))
	})
	cmd.Long = `Show a sandbox's logs, as openshell logs does: the gateway's records about
the sandbox and the sandbox's own, which name each denied request with its
method, path and the rule that refused it.`
	cmd.Example = `  brig sandbox logs dev webapp --since 10m --source sandbox
  brig sandbox logs webapp -f`
	f := cmd.Flags()
	f.BoolVarP(&follow, "follow", "f", false, "keep printing new lines")
	f.IntVarP(&lines, "lines", "n", 200, "print this many lines")
	f.StringVar(&since, "since", "", "only lines from this long ago, e.g. 10m or 1h")
	f.StringArrayVar(&sources, "source", nil, "only lines from gateway or sandbox (repeatable; default: both)")
	f.StringVar(&level, "level", "", "only lines of this level or above: error, warn, info, debug or trace (default: info)")
	_ = cmd.RegisterFlagCompletionFunc("source", cobra.FixedCompletions([]string{"gateway", "sandbox"}, cobra.ShellCompDirectiveNoFileComp))
	_ = cmd.RegisterFlagCompletionFunc("level", cobra.FixedCompletions([]string{"error", "warn", "info", "debug", "trace"}, cobra.ShellCompDirectiveNoFileComp))
	return cmd
}

func newSandboxStartCmd() *cobra.Command {
	cmd := sandboxCommand(sandboxVerb{use: "start [VM] SANDBOX", short: "Start a stopped sandbox"}, func(_ *cobra.Command, t sandboxTarget, _ []string) error {
		return execve("openshell", t.openshell("sandbox", "start", t.sandbox.Name))
	})
	cmd.Long = `Start a stopped sandbox, or one in error, as openshell sandbox start does.
Its /sandbox is as it was left; programs that ran before do not.`
	return cmd
}

func newSandboxStopCmd() *cobra.Command {
	cmd := sandboxCommand(sandboxVerb{use: "stop [VM] SANDBOX", short: "Stop a sandbox, keeping its /sandbox"}, func(_ *cobra.Command, t sandboxTarget, _ []string) error {
		return execve("openshell", t.openshell("sandbox", "stop", t.sandbox.Name))
	})
	cmd.Long = `Stop a sandbox, as openshell sandbox stop does. Every program in it ends,
the agent included; its /sandbox stays for brig sandbox start.`
	return cmd
}

func newSandboxDeleteCmd() *cobra.Command {
	var yes, snap bool
	cmd := sandboxCommand(sandboxVerb{use: "delete [VM] SANDBOX [--snapshot]", short: "Delete a sandbox and its /sandbox"}, func(cmd *cobra.Command, t sandboxTarget, _ []string) error {
		if confirm := confirmer(cmd.Context(), cmd.InOrStdin(), cmd.ErrOrStderr(), yes); confirm != nil {
			what := "with everything in its /sandbox"
			if snap {
				what = "keeping a snapshot of its /sandbox"
			}
			ok, err := confirm(fmt.Sprintf("Delete sandbox %s of VM %s (workspace %s), %s? [y/N] ",
				termtext.Escape(t.sandbox.Name), t.vm.Name, t.workspace, what))
			if err != nil {
				return err
			}
			if !ok {
				return fmt.Errorf("sandbox %s not deleted", t.sandbox.Name)
			}
		}
		if snap {
			if err := snapshotBeforeDelete(cmd, t); err != nil {
				return err
			}
		}
		return execve("openshell", t.openshell("sandbox", "delete", t.sandbox.Name))
	})
	cmd.Aliases = []string{"rm"}
	cmd.Long = `Delete a sandbox, as openshell sandbox delete does, with what is in its
/sandbox. On a terminal, delete asks first; --yes skips the question. Without
a terminal on standard input, as in a script, it deletes without asking, as
brig delete does.

--snapshot first takes a snapshot of the sandbox's /sandbox, which stays in
the VM: brig sandbox snapshot restore brings it back into another sandbox,
and brig sandbox snapshot prune removes it.`
	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "do not ask for confirmation")
	cmd.Flags().BoolVar(&snap, "snapshot", false, "take a snapshot of the sandbox's /sandbox first")
	return cmd
}

// snapshotBeforeDelete takes the snapshot of brig sandbox delete
// --snapshot, leaving the sandbox stopped for the delete.
func snapshotBeforeDelete(cmd *cobra.Command, t sandboxTarget) error {
	ctx := cmd.Context()
	a, err := newApp()
	if err != nil {
		return err
	}
	s, err := a.snapshots(ctx, t.vm, cmd.ErrOrStderr())
	if err != nil {
		return err
	}
	defer s.close()
	r, err := s.get(ctx, t.workspace, t.sandbox.Name)
	if err != nil {
		return err
	}
	sn, err := s.create(ctx, r, createOpts{leaveStopped: true})
	if err != nil {
		return fmt.Errorf("sandbox %s not deleted: %w", t.sandbox.Name, err)
	}
	fmt.Fprintf(cmd.ErrOrStderr(), "Took snapshot %s of sandbox %s.\n", termtext.Escape(sn.Name()), termtext.Escape(t.sandbox.Name))
	return nil
}

func newSandboxAttachCmd() *cobra.Command {
	cmd := sandboxCommand(sandboxVerb{use: "attach [VM] SANDBOX PROVIDER", short: "Attach a provider to a sandbox", extra: 1, ready: true}, func(_ *cobra.Command, t sandboxTarget, rest []string) error {
		return execve("openshell", t.openshell("sandbox", "provider", "attach", t.sandbox.Name, rest[0], "--wait"))
	})
	cmd.Long = `Attach a provider to a ready sandbox and wait until the sandbox applies it,
as openshell sandbox provider attach --wait does. Only programs that the
sandbox starts afterwards see the provider's credentials: open a new shell
with brig sandbox exec, or stop and start the sandbox.`
	cmd.Example = `  brig sandbox attach dev webapp github`
	return cmd
}

func newSandboxDetachCmd() *cobra.Command {
	cmd := sandboxCommand(sandboxVerb{use: "detach [VM] SANDBOX PROVIDER", short: "Detach a provider from a sandbox", extra: 1, ready: true}, func(_ *cobra.Command, t sandboxTarget, rest []string) error {
		if !slices.Contains(t.sandbox.Providers, rest[0]) {
			return notFound(fmt.Errorf("sandbox %s has no provider %s attached", t.sandbox.Name, rest[0]))
		}
		return execve("openshell", t.openshell("sandbox", "provider", "detach", t.sandbox.Name, rest[0], "--wait"))
	})
	cmd.Long = `Detach a provider from a ready sandbox and wait until the sandbox applies
it, as openshell sandbox provider detach --wait does. The rules that the
provider added to the sandbox's policy go with it; programs that run already
keep its placeholders until they restart.`
	return cmd
}

// sandboxRow is a sandbox as brig sandbox list prints it.
type sandboxRow struct {
	VM        string    `json:"vm"`
	Workspace string    `json:"workspace"`
	Name      string    `json:"name"`
	Phase     string    `json:"phase"`
	Image     string    `json:"image,omitempty"`
	Template  string    `json:"template,omitempty"`
	Providers []string  `json:"providers"`
	Created   time.Time `json:"created,omitzero"`
	Problem   string    `json:"problem,omitempty"`
}

func newSandboxRow(vmName string, s store.Sandbox) sandboxRow {
	providers := s.Providers
	if providers == nil {
		providers = []string{}
	}
	return sandboxRow{
		VM: vmName, Workspace: s.Workspace, Name: s.Name, Phase: s.Phase, Image: s.Image, Template: s.Template,
		Providers: providers, Created: s.Created, Problem: s.Problem,
	}
}

func newSandboxListCmd() *cobra.Command {
	var (
		format    outputFormat
		allVMs    bool
		workspace string
	)
	cmd := &cobra.Command{
		Use:     "list [VM]",
		Aliases: []string{"ls"},
		Short:   "List the sandboxes of a VM, or of every running VM",
		Long: `List the sandboxes in every workspace of a VM's gateway, or with --all-vms,
of every running VM's gateway. -w lists only that workspace's; BRIG_WORKSPACE
does not narrow the list.`,
		Example: `  brig sandbox list dev
  brig sandbox list --all-vms -o json`,
		Args:              cobra.MaximumNArgs(1),
		ValidArgsFunction: completeVMs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if allVMs && len(args) > 0 {
				return errors.New("name a VM or give --all-vms, not both")
			}
			filter := ""
			if workspace != "" {
				if err := openshell.CheckWorkspace(workspace); err != nil {
					return err
				}
				filter = workspace
			}
			a, err := newApp()
			if err != nil {
				return err
			}
			var rows []sandboxRow
			if allVMs {
				sts, serr := a.status(cmd.Context())
				if serr != nil {
					fmt.Fprintf(cmd.ErrOrStderr(), "Warning: %s\n", termtext.Escape(serr.Error()))
				}
				rows, err = allSandboxes(cmd.Context(), sts)
			} else {
				var v *vm.VM
				if v, err = a.resolveVMArg(cmd, args); err != nil {
					return err
				}
				rows, err = vmSandboxes(cmd.Context(), v)
			}
			rows = slices.DeleteFunc(rows, func(r sandboxRow) bool { return filter != "" && r.Workspace != filter })
			if perr := printSandboxes(cmd.OutOrStdout(), rows, format, allVMs); perr != nil {
				return perr
			}
			return err
		},
	}
	addOutputFlag(cmd, &format)
	cmd.Flags().StringVarP(&workspace, "workspace", "w", "", "list only the sandboxes of this workspace")
	_ = cmd.RegisterFlagCompletionFunc("workspace", completeWorkspaces)
	cmd.Flags().BoolVar(&allVMs, "all-vms", false, "list the sandboxes of every running VM")
	return cmd
}

// vmSandboxes lists the sandboxes of v's gateway.
func vmSandboxes(ctx context.Context, v *vm.VM) ([]sandboxRow, error) {
	all, err := gatewaySandboxes(ctx, v)
	if err != nil {
		return nil, err
	}
	rows := make([]sandboxRow, len(all))
	for i, s := range all {
		rows[i] = newSandboxRow(v.Name, s)
	}
	return rows, nil
}

// allSandboxes lists the sandboxes of the VMs in sts that run, by VM. A
// gateway that does not answer fails the list, which still has the others.
func allSandboxes(ctx context.Context, sts []vmStatus) ([]sandboxRow, error) {
	sts = slices.DeleteFunc(slices.Clone(sts), func(st vmStatus) bool { return st.State != libvirt.StateRunning.String() })
	rows := make([][]sandboxRow, len(sts))
	errs := make([]error, len(sts))
	fanout.Each(ctx, len(sts), statusLimit, func(i int) {
		rows[i], errs[i] = vmSandboxes(ctx, sts[i].VM)
	})
	return slices.Concat(rows...), errors.Join(errs...)
}

// printSandboxes prints rows as a table, with a VM column if withVM, or as
// JSON.
func printSandboxes(w io.Writer, rows []sandboxRow, format outputFormat, withVM bool) error {
	if format == "json" {
		if rows == nil {
			rows = []sandboxRow{}
		}
		return writeJSON(w, rows)
	}
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	if withVM {
		fmt.Fprint(tw, "VM\t")
	}
	fmt.Fprintln(tw, "WORKSPACE\tNAME\tPHASE\tIMAGE\tPROVIDERS\tCREATED")
	for _, r := range rows {
		if withVM {
			fmt.Fprintf(tw, "%s\t", termtext.Escape(r.VM))
		}
		image := r.Image
		if r.Template != "" {
			image = "template " + r.Template
		}
		created := ""
		if !r.Created.IsZero() {
			created = r.Created.Local().Format(time.DateTime)
		}
		phase := r.Phase
		if r.Problem != "" {
			phase += " (" + r.Problem + ")"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n", termtext.Escape(r.Workspace), termtext.Escape(r.Name), termtext.Escape(phase),
			termtext.Escape(image), termtext.Escape(strings.Join(r.Providers, ",")), created)
	}
	return tw.Flush()
}
