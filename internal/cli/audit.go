// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/GSI-HPC/go-clikit/cliout"
	"github.com/GSI-HPC/go-clikit/termtext"
	"github.com/spf13/cobra"

	"github.com/dennisklein/brig/internal/audit"
	"github.com/dennisklein/brig/internal/bytesize"
	"github.com/dennisklein/brig/internal/config"
	"github.com/dennisklein/brig/internal/fence2"
	"github.com/dennisklein/brig/internal/libvirt"
	"github.com/dennisklein/brig/internal/openshell"
	"github.com/dennisklein/brig/internal/vm"
)

func newAuditCmd() *cobra.Command {
	var (
		o      auditOptions
		format outputFormat
	)
	cmd := &cobra.Command{
		Use:   "audit [VM] [SANDBOX]",
		Short: "Show what sandboxes reached, and who allowed it",
		Long: `audit reads the audit trail that brig keeps of each VM: every request that
a sandbox's proxy allowed or denied (fence 1), what the VM's network
profile dropped (fence 2), sandboxes created and deleted, each revision of
a sandbox's policy with the rules it added, removed and changed and who
made it, the rules OpenShell drafted and the decisions on them, the brig
commands that worked on the VM, and gaps where records may be missing.

Fence 2 sees packets from the VM, not from sandboxes: a drop records the
address, port and protocol the VM tried to reach and the profile setting
that dropped it, summed up over a few seconds. Where a sandbox's request
that fence 1 allowed to the same port lies just before, the timeline
names that sandbox as the likely source; this is a guess, never stored.
Drops belong to the VM, so a sandbox's trail has none.

While a VM runs, a collector on the host, the systemd user unit
brig-audit-VM.service, writes the trail to
$XDG_STATE_HOME/brig/audit/VM/, outside the VM. It keeps everything
unless audit.retention in config.yaml is set or brig audit prune deletes
it, and stays after brig delete. It mirrors changes, but not allowed
requests, to the user journal: journalctl --user -u brig-audit-VM.
audit.journal sets that: changes, all or off.

A revision made by brig policy allow (and the web UI's Allow) says so,
with the rule's host; one made by approving a draft names the draft; a
sandbox's first revision is its creation; anything else, such as
openshell policy set, is "unknown".

With a VM, audit reads that VM's trail; with no VM, the one that brig
use selected, else every VM's. A single argument that is no VM names a
sandbox of the selected VM. The output is a timeline, oldest first;
--summary lists per sandbox each endpoint reached, with allowed and
denied counts and the rule that allowed it, and then what each VM's
network profile dropped. -o json prints the records as stored, one per
line. Denials and drops do not change the exit status.

--since and --until take a duration back from now (30m, 24h, 7d, 2w), a
date (2026-10-13) or a time (2026-10-13 08:00 or RFC 3339); --since all
reads the whole trail.`,
		Args:              cobra.MaximumNArgs(2),
		ValidArgsFunction: completeVMAndSandbox,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runAudit(cmd, args, o, format)
		},
	}
	f := cmd.Flags()
	addWorkspaceFlag(cmd, &o.workspace)
	f.StringVar(&o.since, "since", "24h", "show records from this time on: a duration back, a date or time, or all")
	f.StringVar(&o.until, "until", "", "show records before this time")
	f.BoolVar(&o.denied, "denied", false, "show denied requests and fence 2's drops only, or with --kind those and the other kinds")
	f.StringVar(&o.host, "host", "", "show requests and drafts of this host and its subdomains, or of a pattern such as *.github.com, and drops of this address or network")
	f.StringSliceVar(&o.kinds, "kind", nil, "show records of these kinds: "+strings.Join(audit.Kinds, ", "))
	f.BoolVar(&o.summary, "summary", false, "list each endpoint reached per sandbox instead")
	f.BoolVarP(&o.follow, "follow", "f", false, "go on printing records as they are written")
	addOutputFlag(cmd, &format)
	_ = cmd.RegisterFlagCompletionFunc("kind", cobra.FixedCompletions(audit.Kinds, cobra.ShellCompDirectiveNoFileComp))
	cmd.AddCommand(newAuditStatusCmd(), newAuditPruneCmd(), newAuditCollectCmd())
	return cmd
}

type auditOptions struct {
	workspace, since, until, host string
	kinds                         []string
	denied, summary, follow       bool
}

// auditStore is the audit store of the user's state directory.
func (a *app) auditStore() audit.Store { return audit.NewStore(a.dirs) }

// hasVM reports whether brig has the VM, or once had it and keeps its
// audit trail.
func (a *app) hasVM(name string) bool {
	if _, err := a.vms.Load(name); err == nil {
		return true
	}
	_, err := os.Stat(a.auditStore().Dir(name))
	return err == nil && vm.ValidateName(name) == nil
}

// auditVMs resolves the VM and sandbox arguments of brig audit: the VMs to
// read, none for every VM, and the sandbox.
func (a *app) auditVMs(cmd *cobra.Command, args []string) ([]string, string, error) {
	if len(args) == 2 {
		if !a.hasVM(args[0]) {
			return nil, "", fmt.Errorf("brig has no VM %s and no audit trail of one", args[0])
		}
		return args[:1], args[1], nil
	}
	if len(args) == 1 && a.hasVM(args[0]) {
		return args, "", nil
	}
	name, err := selectedVM()
	switch {
	case errors.Is(err, errNoVMSelected) && len(args) == 1:
		return nil, "", fmt.Errorf("brig has no VM %s and no audit trail of one; to name a sandbox, name its VM first", args[0])
	case errors.Is(err, errNoVMSelected):
		return nil, "", nil
	case err != nil:
		return nil, "", err
	}
	fmt.Fprintf(cmd.ErrOrStderr(), "Using VM %s (selected by brig use).\n", termtext.Escape(name))
	sandbox := ""
	if len(args) == 1 {
		sandbox = args[0]
	}
	return []string{name}, sandbox, nil
}

// parseWhen reads a time as --since and --until take it.
func parseWhen(s string, now time.Time) (time.Time, error) {
	if s == "" || s == "all" {
		return time.Time{}, nil
	}
	if d, err := config.ParseDuration(s); err == nil {
		return now.Add(-d), nil
	}
	for _, layout := range []string{time.RFC3339Nano, "2006-01-02T15:04:05", "2006-01-02T15:04", time.DateTime, "2006-01-02 15:04", time.DateOnly} {
		if t, err := time.ParseInLocation(layout, s, time.Local); err == nil {
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("cannot read %q as a time: give a duration such as 24h or 7d, a date such as 2026-10-13, or a time such as 2026-10-13 08:00", s)
}

func runAudit(cmd *cobra.Command, args []string, o auditOptions, format outputFormat) error {
	a, err := newApp()
	if err != nil {
		return err
	}
	vms, sandbox, err := a.auditVMs(cmd, args)
	if err != nil {
		return err
	}
	q := audit.Query{VMs: vms, Sandbox: sandbox, Denied: o.denied, Host: o.host, Kinds: o.kinds}
	for _, k := range o.kinds {
		if !slices.Contains(audit.Kinds, k) {
			return fmt.Errorf("unknown kind %q (want %s)", k, strings.Join(audit.Kinds, ", "))
		}
	}
	if q.Workspace, err = auditWorkspace(o.workspace); err != nil {
		return err
	}
	if sandbox != "" && q.Workspace == "" {
		q.Workspace = openshell.DefaultWorkspace
	}
	now := time.Now()
	if q.Since, err = parseWhen(o.since, now); err != nil {
		return fmt.Errorf("--since: %w", err)
	}
	if q.Until, err = parseWhen(o.until, now); err != nil {
		return fmt.Errorf("--until: %w", err)
	}
	st := a.auditStore()
	all := vms
	if len(all) == 0 {
		if all, err = st.VMs(); err != nil {
			return err
		}
	}
	if len(all) == 0 {
		fmt.Fprintln(cmd.ErrOrStderr(), "No VM has an audit trail yet: a VM's collector starts with brig start.")
		return nil
	}
	oldest, err := st.Oldest(all)
	if err != nil {
		return err
	}
	if !oldest.IsZero() && q.Since.Before(oldest) && !q.Since.IsZero() {
		fmt.Fprintf(cmd.ErrOrStderr(), "The audit trail starts at %s.\n", oldest.Local().Format(time.DateTime))
	}
	w := cmd.OutOrStdout()
	if o.summary {
		if o.follow {
			return errors.New("--summary and --follow do not go together")
		}
		reach, err := st.Summary(q)
		if err != nil {
			return err
		}
		if format == "json" {
			return cliout.WriteJSON(w, reach)
		}
		return printSummary(w, reach, len(all) > 1)
	}
	p := &timeline{w: w, json: format == "json", vms: len(all) > 1}
	if !p.json && mayHoldDrops(q) {
		if p.hints, err = audit.Hints(st, q); err != nil {
			return err
		}
	}
	n := 0
	if err := st.Read(q, func(r audit.Record) bool { n++; return p.print(r) == nil }); err != nil {
		return err
	}
	if p.err != nil {
		return p.err
	}
	if !o.follow {
		if n == 0 && format != "json" {
			fmt.Fprintln(cmd.ErrOrStderr(), "No records match.")
		}
		return nil
	}
	// Ctrl-C ends the command's context, which follows until then.
	q.Since, q.Until = time.Time{}, time.Time{}
	if p.json || !mayHoldDrops(q) {
		err = st.Follow(cmd.Context(), q, func(r audit.Record) { _ = p.print(r) })
		return errors.Join(err, p.err)
	}
	// Follow the allowed requests too, which the drops' hints come from.
	p.hints, p.hinter = nil, &audit.Hinter{}
	err = st.Follow(cmd.Context(), audit.Query{VMs: q.VMs}, func(r audit.Record) {
		p.hinter.Observe(r)
		if r.Kind == audit.KindDrop {
			p.hinter.Forget(r.T.Add(-time.Minute))
		}
		if q.Match(r) {
			_ = p.print(r)
		}
	})
	return errors.Join(err, p.err)
}

// mayHoldDrops reports whether q may select drops, which a sandbox's trail
// has none of.
func mayHoldDrops(q audit.Query) bool {
	return q.Workspace == "" && q.Sandbox == "" && (len(q.Kinds) == 0 || slices.Contains(q.Kinds, audit.KindDrop))
}

// auditWorkspace is the workspace that brig audit reads: -w, else
// BRIG_WORKSPACE, else every workspace.
func auditWorkspace(given string) (string, error) {
	if given != "" {
		return given, openshell.CheckWorkspace(given)
	}
	return envWorkspace()
}

// timeline prints records as brig audit lists them.
type timeline struct {
	w    io.Writer
	json bool
	// vms says that records of several VMs are printed, so each line
	// names its VM.
	vms bool
	// hints are the drops' hints by audit.DropKey, or hinter works them
	// out as the records come.
	hints  map[string]audit.Hint
	hinter *audit.Hinter
	err    error
}

func (p *timeline) print(r audit.Record) error {
	if p.err != nil {
		return p.err
	}
	if p.json {
		b, err := json.Marshal(r)
		if err == nil {
			_, err = fmt.Fprintf(p.w, "%s\n", b)
		}
		p.err = err
		return err
	}
	where := r.SandboxKey()
	if r.Workspace == openshell.DefaultWorkspace {
		where = r.Sandbox
	}
	if where == "" {
		where = "-"
	}
	if p.vms {
		where = r.VM + " " + where
	}
	what := audit.Describe(r)
	if r.Kind == audit.KindDrop {
		hint, ok := p.hints[audit.DropKey(r)]
		if p.hinter != nil {
			hint, ok = p.hinter.Hint(r)
		}
		if ok {
			what += " (" + hintText(hint) + ")"
		}
	}
	_, p.err = fmt.Fprintf(p.w, "%s  %-20s  %s\n", r.T.Local().Format(time.DateTime), termtext.Escape(where), termtext.Escape(what))
	return p.err
}

// sandboxName names a sandbox by its WORKSPACE/NAME, by its name alone in
// the default workspace.
func sandboxName(key string) string {
	if name, ok := strings.CutPrefix(key, openshell.DefaultWorkspace+"/"); ok {
		return name
	}
	return key
}

// hintText says which sandbox a drop likely came from.
func hintText(h audit.Hint) string {
	switch len(h.Sandboxes) {
	case 0:
		// OpenShell logs no connection that an L4 rule allows, so a
		// sandbox's may match nothing either.
		return "from the VM, or a sandbox's --l4 connection, which fence 1 does not log"
	case 1:
		return fmt.Sprintf("likely from %s: fence 1 allowed %s at %s", sandboxName(h.Sandboxes[0]),
			net.JoinHostPort(h.Host, strconv.Itoa(h.Port)), h.At.Local().Format(time.TimeOnly))
	}
	names := make([]string, len(h.Sandboxes))
	for i, k := range h.Sandboxes {
		names[i] = sandboxName(k)
	}
	return "likely from one of " + strings.Join(names, ", ")
}

// printSummary prints what each sandbox reached, and then what the network
// profiles dropped.
func printSummary(w io.Writer, reach []audit.Reach, vms bool) error {
	var drops []audit.Reach
	reach = slices.DeleteFunc(reach, func(x audit.Reach) bool {
		if x.Dropped > 0 {
			drops = append(drops, x)
		}
		return x.Dropped > 0
	})
	if len(reach) == 0 && len(drops) == 0 {
		_, err := fmt.Fprintln(w, "No requests match.")
		return err
	}
	if len(reach) > 0 {
		if err := printReach(w, reach, vms); err != nil {
			return err
		}
	}
	if len(drops) == 0 {
		return nil
	}
	if len(reach) > 0 {
		fmt.Fprintln(w)
	}
	fmt.Fprintln(w, "Dropped by the network profile (fence 2):")
	t := cliout.Table{Columns: columns("VM", "DESTINATION", "REASON", "DROPPED", "FIRST", "LAST", "LIKELY FROM")}
	for _, x := range drops {
		dest := strings.TrimSpace(x.Proto + " " + x.Endpoint)
		if x.Endpoint == "" {
			dest = "(counted only)"
		}
		var likely []string
		for _, k := range x.Likely {
			likely = append(likely, sandboxName(k))
		}
		if x.FromVM > 0 {
			likely = append(likely, "the VM itself, or an --l4 rule")
		}
		t.Add(x.VM, dest, fence2.Explain(x.Reason), strconv.Itoa(x.Dropped),
			x.First.Local().Format("02 Jan 15:04"), x.Last.Local().Format("02 Jan 15:04"), orDash(strings.Join(likely, ", ")))
	}
	return t.Write(w, false)
}

// printReach prints what each sandbox reached.
func printReach(w io.Writer, reach []audit.Reach, vms bool) error {
	t := cliout.Table{Columns: columns("SANDBOX", "ENDPOINT", "ALLOWED", "DENIED", "FIRST", "LAST", "BINARIES", "RULE")}
	for _, x := range reach {
		where := x.Sandbox
		if x.Workspace != openshell.DefaultWorkspace {
			where = x.Workspace + "/" + x.Sandbox
		}
		if vms {
			where = x.VM + " " + where
		}
		var rules []string
		for _, r := range x.Rules {
			if via := x.Via[r]; via != "" {
				r += " (" + via + ")"
			}
			rules = append(rules, r)
		}
		t.Add(where, x.Endpoint, strconv.Itoa(x.Allowed), strconv.Itoa(x.Denied),
			x.First.Local().Format("02 Jan 15:04"), x.Last.Local().Format("02 Jan 15:04"),
			orDash(strings.Join(x.Binaries, ",")), orDash(strings.Join(rules, ", ")))
	}
	return t.Write(w, false)
}

// diskSize is a size on disk for display.
func diskSize(n int64) string {
	return bytesize.Size(max(n, 0)).Approx()
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func newAuditStatusCmd() *cobra.Command {
	var format outputFormat
	cmd := &cobra.Command{
		Use:   "status [VM]",
		Short: "Show the audit collectors and the space their history takes",
		Long: `status shows, for each VM with an audit trail, whether its collector
runs, since when, the time of its latest record, the gaps of the last day,
how fully fence 2 records drops, and the space its history takes. With a
VM, it adds how that space splits by sandbox, kind and month.

FENCE 2 is itemised when each drop is recorded with its address and port,
counts only when the host's kernel lets brig count drops per reason but
not log them, and off when it records none; the line below the table says
why. brig doctor checks it too.

A collector that failed, or does not run while its VM does, makes the
exit status 1: the VM's traffic is not being recorded.`,
		Args:              cobra.MaximumNArgs(1),
		ValidArgsFunction: completeVMs,
		RunE: func(cmd *cobra.Command, args []string) error {
			a, err := newApp()
			if err != nil {
				return err
			}
			return a.auditStatus(cmd, args, format)
		},
	}
	addOutputFlag(cmd, &format)
	return cmd
}

// auditStatusJSON is what brig audit status -o json prints.
type auditStatusJSON struct {
	VMs       []audit.VMStatus `json:"vms"`
	Dir       string           `json:"dir"`
	Disk      int64            `json:"disk"`
	Free      int64            `json:"free"`
	Oldest    time.Time        `json:"oldest,omitzero"`
	Breakdown *audit.Breakdown `json:"breakdown,omitempty"`
}

// vmInfo returns what audit status needs to know of each VM brig has.
func (a *app) vmInfo(ctx context.Context) func(string) audit.VMInfo {
	conn, err := a.connect(ctx)
	if err == nil {
		defer func() { _ = conn.Close() }()
	}
	known := map[string]audit.VMInfo{}
	for _, name := range a.vms.Names() {
		in := audit.VMInfo{Exists: true}
		if err == nil {
			if v, lerr := a.vms.Load(name); lerr == nil {
				state, serr := conn.State(v.DomainName())
				in.Known, in.Running = serr == nil, state == libvirt.StateRunning
			}
		}
		known[name] = in
	}
	return func(name string) audit.VMInfo { return known[name] }
}

func (a *app) auditStatus(cmd *cobra.Command, args []string, format outputFormat) error {
	st := a.auditStore()
	vms, err := st.VMs()
	if err != nil {
		return err
	}
	for _, name := range a.vms.Names() {
		if !slices.Contains(vms, name) {
			vms = append(vms, name)
		}
	}
	slices.Sort(vms)
	if len(args) == 1 {
		if !a.hasVM(args[0]) {
			return fmt.Errorf("brig has no VM %s and no audit trail of one", args[0])
		}
		vms = args
	}
	ctx := cmd.Context()
	statuses, err := st.Status(ctx, vms, a.vmInfo(ctx))
	if err != nil {
		return err
	}
	out := auditStatusJSON{VMs: statuses, Dir: st.Root}
	for _, s := range statuses {
		out.Disk += s.Disk
	}
	if out.Oldest, err = st.Oldest(vms); err != nil {
		return err
	}
	out.Free, _ = st.Free()
	if len(args) == 1 {
		x, err := st.ReadIndex(args[0])
		if err != nil {
			return err
		}
		b := x.Breakdown()
		out.Breakdown = &b
	}
	w := cmd.OutOrStdout()
	if format == "json" {
		err = cliout.WriteJSON(w, out)
	} else {
		err = printAuditStatus(w, out)
	}
	if err != nil {
		return err
	}
	var problems []string
	for _, s := range statuses {
		if s.Problem() {
			problems = append(problems, s.VM)
		}
	}
	if len(problems) > 0 {
		return fmt.Errorf("nothing records the traffic of %s; see journalctl --user -u %s, and brig start restarts it",
			strings.Join(problems, ", "), audit.UnitName(problems[0]))
	}
	return nil
}

// ago says how long ago t was, in a few characters.
func ago(t, now time.Time) string {
	if t.IsZero() {
		return "–"
	}
	d := now.Sub(t)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%d s ago", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%d min ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%d h ago", int(d.Hours()))
	}
	return t.Local().Format("2 Jan")
}

func printAuditStatus(w io.Writer, s auditStatusJSON) error {
	now := time.Now()
	t := cliout.Table{Columns: columns("VM", "COLLECTOR", "SINCE", "LAST EVENT", "GAPS 24H", "FENCE 2", "ON DISK")}
	for _, v := range s.VMs {
		state, since, gaps, fence := v.Collector, "–", "–", "–"
		if v.Collector == audit.StateVMDeleted && !v.Deleted.IsZero() {
			state += " " + v.Deleted.Local().Format("2 Jan")
		}
		if v.Collector == audit.UnitRunning {
			since, gaps = v.Since.Local().Format("2 Jan 15:04"), fmt.Sprint(v.Gaps24h)
			fence = cmp.Or(string(v.Fence2), "–")
		}
		t.Add(v.VM, state, since, ago(v.LastEvent, now), gaps, fence, diskSize(v.Disk))
	}
	if err := t.Write(w, false); err != nil {
		return err
	}
	for _, v := range s.VMs {
		if v.Error != "" {
			fmt.Fprintf(w, "%s: %s\n", termtext.Escape(v.VM), termtext.Escape(v.Error))
		}
		if v.Collector == audit.UnitRunning && v.Fence2Why != "" {
			fmt.Fprintf(w, "%s: fence 2 %s: %s\n", termtext.Escape(v.VM), v.Fence2, termtext.Escape(v.Fence2Why))
		}
	}
	oldest := ""
	if !s.Oldest.IsZero() {
		oldest = ", oldest " + s.Oldest.Local().Format("2 Jan 2006")
	}
	fmt.Fprintf(w, "\nTotal %s in %s%s; %s free.\n", diskSize(s.Disk), termtext.Escape(tildePath(s.Dir)), oldest, diskSize(s.Free))
	if s.Breakdown == nil {
		return nil
	}
	for _, part := range []struct {
		title string
		parts []audit.Part
	}{{"SANDBOX", s.Breakdown.Sandboxes}, {"KIND", s.Breakdown.Kinds}, {"MONTH", s.Breakdown.Months}} {
		fmt.Fprintln(w)
		t := cliout.Table{Columns: columns(part.title, "ROWS", "ON DISK")}
		for _, p := range part.parts {
			name := p.Name
			switch {
			case part.title == "SANDBOX" && name == "":
				name = "(the VM)"
			case part.title == "SANDBOX" && !p.Deleted.IsZero():
				name += " (deleted " + p.Deleted.Local().Format("2 Jan") + ")"
			}
			t.Add(name, strconv.FormatInt(p.Rows, 10), diskSize(p.Disk))
		}
		if err := t.Write(w, false); err != nil {
			return err
		}
	}
	return nil
}

// tildePath writes a path below the home directory with ~.
func tildePath(p string) string {
	if home, err := os.UserHomeDir(); err == nil {
		if rest, ok := strings.CutPrefix(p, home+"/"); ok {
			return "~/" + rest
		}
	}
	return p
}

func newAuditPruneCmd() *cobra.Command {
	var (
		before, sandbox, workspace string
		deletedVMs, dryRun, yes    bool
	)
	cmd := &cobra.Command{
		Use:   "prune [VM]",
		Short: "Delete audit history",
		Long: `prune deletes audit history, which nothing else does unless audit.retention
in config.yaml is set: the days written before --before, the records of a
sandbox (--sandbox, with -w for another workspace than default), or with
--deleted-vms the whole history of every VM that brig delete deleted.
Without a VM, --before and --deleted-vms work on every VM.

It says what it deletes and how much space that frees. On a terminal it
asks first; --yes skips the question, and --dry-run only says.`,
		Args:              cobra.MaximumNArgs(1),
		ValidArgsFunction: completeVMs,
		RunE: func(cmd *cobra.Command, args []string) error {
			a, err := newApp()
			if err != nil {
				return err
			}
			spec := audit.PruneSpec{Sandbox: sandbox, DeletedVMs: deletedVMs}
			if before != "" {
				if spec.Before, err = parseWhen(before, time.Now()); err != nil {
					return fmt.Errorf("--before: %w", err)
				}
			}
			switch {
			case before == "" && sandbox == "" && !deletedVMs:
				return errors.New("say what to delete with --before, --sandbox or --deleted-vms")
			case deletedVMs && (before != "" || sandbox != ""):
				return errors.New("--deleted-vms deletes whole histories and takes no --before or --sandbox")
			case sandbox != "" && len(args) == 0:
				return errors.New("name the VM of the sandbox")
			}
			if len(args) == 1 {
				if !a.hasVM(args[0]) {
					return fmt.Errorf("brig has no VM %s and no audit trail of one", args[0])
				}
				spec.VMs = args
				recordVM(cmd.Context(), args[0])
			}
			if spec.Workspace, err = chosenWorkspace(workspace); err != nil {
				return err
			}
			st := a.auditStore()
			plan, err := st.PlanPrune(spec, func(name string) bool { _, err := a.vms.Load(name); return err == nil })
			if err != nil {
				return err
			}
			stdout, stderr := cmd.OutOrStdout(), cmd.ErrOrStderr()
			if len(plan.Items) == 0 {
				fmt.Fprintln(stderr, "Nothing to delete.")
				return nil
			}
			describePrune(stderr, plan)
			if dryRun {
				return nil
			}
			if confirm := confirmer(cmd.Context(), cmd.InOrStdin(), stderr, yes); confirm != nil {
				ok, err := confirm("Delete it? [y/N] ")
				if err != nil {
					return err
				}
				if !ok {
					return errors.New("nothing deleted")
				}
			}
			if err := st.Prune(plan); err != nil {
				return err
			}
			fmt.Fprintf(stdout, "Deleted %d records, freeing %s.\n", plan.Rows, diskSize(plan.Disk))
			return nil
		},
	}
	f := cmd.Flags()
	f.StringVar(&before, "before", "", "delete the days before this date, or before this long ago (90d)")
	f.StringVar(&sandbox, "sandbox", "", "delete the records of this sandbox")
	addWorkspaceFlag(cmd, &workspace)
	f.BoolVar(&deletedVMs, "deleted-vms", false, "delete the history of VMs that no longer exist")
	f.BoolVar(&dryRun, "dry-run", false, "only say what would be deleted")
	f.BoolVarP(&yes, "yes", "y", false, "do not ask for confirmation")
	return cmd
}

// describePrune says what the plan deletes.
func describePrune(w io.Writer, plan audit.PrunePlan) {
	type sum struct {
		days       []string
		rows, disk int64
		whole      bool
		sandbox    string
	}
	var order []string
	byVM := map[string]*sum{}
	for _, it := range plan.Items {
		s := byVM[it.VM]
		if s == nil {
			s = &sum{}
			byVM[it.VM] = s
			order = append(order, it.VM)
		}
		s.rows += it.Rows
		s.disk += it.Disk
		s.whole = s.whole || it.Day == ""
		s.sandbox = it.Sandbox
		if it.Day != "" {
			s.days = append(s.days, it.Day)
		}
	}
	for _, vm := range order {
		s := byVM[vm]
		what := "the whole history"
		switch {
		case s.whole:
		case s.sandbox != "":
			what = "the records of sandbox " + s.sandbox
		case len(s.days) == 1:
			what = "the day " + s.days[0]
		default:
			slices.Sort(s.days)
			what = fmt.Sprintf("%d days, %s to %s", len(s.days), s.days[0], s.days[len(s.days)-1])
		}
		fmt.Fprintf(w, "VM %s: %s, %d records, %s.\n", termtext.Escape(vm), termtext.Escape(what), s.rows, diskSize(s.disk))
	}
	if len(order) > 1 {
		fmt.Fprintf(w, "In all %d records, %s.\n", plan.Rows, diskSize(plan.Disk))
	}
}

func newAuditCollectCmd() *cobra.Command {
	return &cobra.Command{
		Use:    "collect VM",
		Short:  "Collect a VM's audit trail (run by brig start)",
		Hidden: true,
		Args:   cobra.ExactArgs(1),
		// It runs until stopped and is no work to show.
		Annotations: map[string]string{noProgress: "true"},
		RunE: func(cmd *cobra.Command, args []string) error {
			a, err := newApp()
			if err != nil {
				return err
			}
			name := args[0]
			if _, err := a.vms.Load(name); err != nil {
				return err
			}
			log := slog.New(slog.NewTextHandler(cmd.ErrOrStderr(), nil))
			var j *audit.Journal
			if mode := a.cfg.Audit.Journal; mode != audit.JournalOff {
				if j, err = audit.OpenJournal(audit.JournalSocket, mode == audit.JournalAll); err != nil {
					log.Warn("not mirroring the audit trail", "err", err)
				}
				defer func() { _ = j.Close() }()
			}
			c := &audit.Collector{
				VM: name, Store: a.auditStore(), ActivityLog: a.dirs.ActivityLog(),
				Journal: j, Retention: time.Duration(a.cfg.Audit.Retention), Log: log,
				Dial: func() (audit.Gateway, error) {
					// brig start may have moved the gateway's port.
					v, err := a.vms.Load(name)
					if err != nil {
						return nil, err
					}
					return gatewayClient(v)
				},
			}
			// Ctrl-C and SIGTERM end the command's context.
			return c.Run(cmd.Context())
		},
	}
}
