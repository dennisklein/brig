// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/GSI-HPC/go-clikit/cliout"
	"github.com/GSI-HPC/go-clikit/termtext"
	"github.com/spf13/cobra"

	"github.com/dennisklein/brig/internal/bytesize"
	"github.com/dennisklein/brig/internal/guest"
	"github.com/dennisklein/brig/internal/openshell"
	"github.com/dennisklein/brig/internal/ui/action"
	"github.com/dennisklein/brig/internal/vm"
)

func newSandboxSnapshotCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "snapshot",
		Short: "Keep copies of a sandbox's /sandbox in its VM, and bring them back",
		Long: `A snapshot is a copy of one sandbox's /sandbox, with a record of the
sandbox: its image, providers, policy and services. It lives in the VM, on the
data disk next to the sandboxes, and outlives the sandbox it came from. It is
named SANDBOX@TIME, the time in UTC, and belongs to the sandbox's workspace.

The data disk is btrfs, so a snapshot shares its blocks with the sandbox it
came from: it takes seconds and almost no space when it is taken, and grows
only as the sandbox moves on. brig sandbox snapshot list shows what each one
holds alone, which is what deleting it frees.

Snapshots go with the VM: brig delete removes them with it. Push code to git
as before.`,
	}
	cmd.AddCommand(newSnapshotCreateCmd(), newSnapshotListCmd(), newSnapshotRestoreCmd(), newSnapshotDeleteCmd(), newSnapshotPruneCmd())
	return cmd
}

// vmAndRest splits args into the VM, which the first names if there are
// more than n, and the n or fewer that follow.
func (a *app) vmAndRest(cmd *cobra.Command, args []string, n int) (*vm.VM, []string, error) {
	named := args[:max(0, len(args)-n)]
	v, err := a.resolveVMArg(cmd, named)
	return v, args[len(named):], err
}

func checkSandboxName(name string) error {
	if !action.ValidSandboxName(name) {
		return fmt.Errorf("invalid sandbox name %q", name)
	}
	return nil
}

func newSnapshotCreateCmd() *cobra.Command {
	var (
		workspace string
		yes       bool
	)
	cmd := &cobra.Command{
		Use:   "create [VM] SANDBOX [-w W] [--yes]",
		Short: "Take a snapshot of a sandbox's /sandbox",
		Long: `Copy the /sandbox of SANDBOX into a new snapshot, SANDBOX@TIME. A ready
sandbox is stopped for the copy, so that no file changes during it, and
started again; whatever runs in it ends. If an agent or another program
runs in it, brig names it and asks first; -y/--yes skips the question.
Without a terminal, brig refuses then, unless --yes. A stopped sandbox stays
stopped.

brig warns when the snapshot holds a file with a real credential, as /login
inside a sandbox leaves it.`,
		Example: `  brig sandbox snapshot create dev webapp
  brig sandbox snapshot create webapp -w team --yes`,
		Args:              cobra.RangeArgs(1, 2),
		ValidArgsFunction: completeVMAndSandbox,
		RunE: func(cmd *cobra.Command, args []string) error {
			ws, err := chosenWorkspace(workspace)
			if err != nil {
				return err
			}
			a, err := newApp()
			if err != nil {
				return err
			}
			v, rest, err := a.vmAndRest(cmd, args, 1)
			if err != nil {
				return err
			}
			if err := checkSandboxName(rest[0]); err != nil {
				return err
			}
			ctx := cmd.Context()
			s, err := a.snapshots(ctx, v, cmd.ErrOrStderr())
			if err != nil {
				return err
			}
			defer s.close()
			r, err := s.get(ctx, ws, rest[0])
			if err != nil {
				return err
			}
			if err := stoppable(r); err != nil {
				return err
			}
			if note, refuse := s.stopCheck(ctx, r); note != "" {
				ap := approval{ctx: ctx, in: cmd.InOrStdin(), w: cmd.ErrOrStderr(), yes: yes}
				if err := ap.ask(note+" Stop it and take a snapshot?", refuse); err != nil {
					return err
				}
			}
			snap, err := s.create(ctx, r, createOpts{})
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Took snapshot %s of sandbox %s.\n", termtext.Escape(snap.Name()), termtext.Escape(r.Name))
			return nil
		},
	}
	addWorkspaceFlag(cmd, &workspace)
	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "stop the sandbox without asking, also when a program runs in it")
	return cmd
}

func newSnapshotListCmd() *cobra.Command {
	var (
		workspace string
		format    outputFormat
	)
	cmd := &cobra.Command{
		Use:     "list [VM] [SANDBOX] [-w W] [-o json]",
		Aliases: []string{"ls"},
		Short:   "List the snapshots in a VM, with the space they take",
		Long: `List the snapshots of every workspace of a VM, newest first for each
sandbox, or with SANDBOX, that sandbox's snapshots in the workspace of -w or
BRIG_WORKSPACE. -w alone lists only that workspace's.

SIZE is what a snapshot holds, EXCLUSIVE what it holds alone: the blocks it
shares with nothing else, which deleting it frees. Since a snapshot shares
its blocks with its sandbox and the other snapshots, the sizes add up to more
than the disk holds. NOTE marks snapshots whose sandbox is gone, and those an
unfinished brig sandbox recreate needs.`,
		Example: `  brig sandbox snapshot list dev
  brig sandbox snapshot list dev webapp -o json`,
		Args:              cobra.MaximumNArgs(2),
		ValidArgsFunction: completeVMAndSandbox,
		RunE: func(cmd *cobra.Command, args []string) error {
			a, err := newApp()
			if err != nil {
				return err
			}
			// A single argument is the VM if there is one of that name.
			n := len(args)
			if n == 1 && a.vms.Exists(args[0]) {
				n = 0
			}
			v, rest, err := a.vmAndRest(cmd, args, min(n, 1))
			if err != nil {
				return err
			}
			ws := workspace
			if ws != "" {
				if err := openshell.CheckWorkspace(ws); err != nil {
					return err
				}
			}
			sandbox := ""
			if len(rest) == 1 {
				sandbox = rest[0]
				if ws, err = chosenWorkspace(workspace); err != nil {
					return err
				}
			}
			ctx := cmd.Context()
			s, err := a.snapshots(ctx, v, cmd.ErrOrStderr())
			if err != nil {
				return err
			}
			defer s.close()
			snaps, err := s.list(ctx, true)
			if err != nil {
				return err
			}
			shown := []snapshot{}
			for _, sn := range snaps {
				if (ws == "" || sn.Workspace == ws) && (sandbox == "" || sn.Sandbox == sandbox) {
					shown = append(shown, sn)
				}
			}
			if err := printSnapshots(cmd.OutOrStdout(), shown, format); err != nil {
				return err
			}
			warnSnapshotSpace(cmd.ErrOrStderr(), v, snaps)
			return nil
		},
	}
	addOutputFlag(cmd, &format)
	cmd.Flags().StringVarP(&workspace, "workspace", "w", "", "list only the snapshots of this workspace")
	_ = cmd.RegisterFlagCompletionFunc("workspace", completeWorkspaces)
	return cmd
}

// size prints a size of a snapshot, "?" if unknown.
func size(n int64) string {
	if n < 0 {
		return "?"
	}
	return bytesize.Size(n).Approx()
}

func printSnapshots(w io.Writer, snaps []snapshot, format outputFormat) error {
	if format == "json" {
		return cliout.WriteJSON(w, snaps)
	}
	t := cliout.Table{Columns: columns("SNAPSHOT", "WORKSPACE", "IMAGE", "SIZE", "EXCLUSIVE", "NOTE")}
	for _, sn := range snaps {
		var note []string
		if sn.Gone {
			note = append(note, "sandbox gone")
		}
		if sn.Recreate {
			note = append(note, "recreate unfinished")
		}
		t.Add(sn.Name(), sn.Workspace, sn.Image, size(sn.Size), size(sn.Exclusive), strings.Join(note, ", "))
	}
	return t.Write(w, false)
}

// warnSnapshotSpace warns when what the snapshots of v hold alone is more
// than a quarter of its data disk.
func warnSnapshotSpace(w io.Writer, v *vm.VM, snaps []snapshot) {
	var excl int64
	for _, sn := range snaps {
		excl += max(sn.Exclusive, 0)
	}
	if v.DataDisk > 0 && bytesize.Size(excl) > v.DataDisk/4 {
		fmt.Fprintf(w, "Warning: snapshots hold %s alone, more than a quarter of VM %s's %s data disk; brig sandbox snapshot prune %[2]s frees space\n",
			bytesize.Size(excl).Approx(), v.Name, v.DataDisk.Approx())
	}
}

func newSnapshotRestoreCmd() *cobra.Command {
	var (
		workspace string
		yes       bool
		resets    []string
	)
	cmd := &cobra.Command{
		Use:   "restore [VM] SNAPSHOT SANDBOX [-w W] [--yes] [--reset PATH]...",
		Short: "Replace a sandbox's /sandbox with a snapshot",
		Long: `Replace the /sandbox of SANDBOX, which must exist, with what SNAPSHOT holds.
Everything in SANDBOX's /sandbox now is lost; take a snapshot first to keep
it. A ready sandbox is stopped for the restore and started again. On a
terminal, brig asks first; -y/--yes skips the question. Without a terminal,
it restores without asking, unless a program other than a shell runs in the
sandbox, which needs --yes.

SANDBOX may be another sandbox than the one SNAPSHOT came from, which copies
one sandbox's work into another. Restore leaves SANDBOX's image, providers
and policy as they are; brig sandbox recreate moves a sandbox to a new image.

--reset PATH, relative to /sandbox, leaves PATH out, so that the image's start
script sets it up fresh. If the sandbox's user differs from the snapshot's,
the files of the snapshot's user change to the sandbox's.`,
		Example: `  brig sandbox snapshot restore dev webapp@2026-10-14T15:02:11Z webapp
  brig sandbox snapshot restore dev webapp@2026-10-14T15:02:11Z webapp-try`,
		Args: cobra.RangeArgs(2, 3),
		RunE: func(cmd *cobra.Command, args []string) error {
			ws, err := chosenWorkspace(workspace)
			if err != nil {
				return err
			}
			for _, r := range resets {
				if err := guest.CheckResetPath(r); err != nil {
					return err
				}
			}
			a, err := newApp()
			if err != nil {
				return err
			}
			v, rest, err := a.vmAndRest(cmd, args, 2)
			if err != nil {
				return err
			}
			if err := checkSandboxName(rest[1]); err != nil {
				return err
			}
			ctx := cmd.Context()
			s, err := a.snapshots(ctx, v, cmd.ErrOrStderr())
			if err != nil {
				return err
			}
			defer s.close()
			snap, err := s.find(ctx, ws, rest[0])
			if err != nil {
				return err
			}
			r, err := s.get(ctx, ws, rest[1])
			if err != nil {
				return err
			}
			if err := stoppable(r); err != nil {
				return err
			}
			note, refuse := s.stopCheck(ctx, r)
			ap := approval{ctx: ctx, in: cmd.InOrStdin(), w: cmd.ErrOrStderr(), yes: yes}
			q := fmt.Sprintf("Replace the /sandbox of sandbox %s (workspace %s) with snapshot %s, losing what it holds now?",
				termtext.Escape(r.Name), ws, termtext.Escape(snap.Name()))
			if err := ap.ask(strings.TrimSpace(note+" "+q), refuse); err != nil {
				return err
			}
			if err := s.restore(ctx, snap, r, resets); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Restored snapshot %s into sandbox %s.\n", termtext.Escape(snap.Name()), termtext.Escape(r.Name))
			return nil
		},
	}
	addWorkspaceFlag(cmd, &workspace)
	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "do not ask for confirmation")
	cmd.Flags().StringArrayVar(&resets, "reset", nil, "leave PATH, relative to /sandbox, out of the restore (repeatable)")
	return cmd
}

func newSnapshotDeleteCmd() *cobra.Command {
	var workspace string
	cmd := &cobra.Command{
		Use:     "delete [VM] SNAPSHOT... [-w W]",
		Aliases: []string{"rm"},
		Short:   "Delete snapshots",
		Long: `Delete the snapshots named SANDBOX@TIME in the workspace of -w or
BRIG_WORKSPACE. What a snapshot shares with its sandbox or other snapshots
stays theirs; brig sandbox snapshot list shows what deleting it frees.`,
		Example: `  brig sandbox snapshot delete dev webapp@2026-10-14T15:02:11Z`,
		Args:    cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ws, err := chosenWorkspace(workspace)
			if err != nil {
				return err
			}
			a, err := newApp()
			if err != nil {
				return err
			}
			// Snapshot names have an @, VM names none.
			n := len(args)
			if !strings.Contains(args[0], "@") {
				n--
			}
			if n == 0 {
				return errors.New("name the snapshots to delete")
			}
			v, rest, err := a.vmAndRest(cmd, args, n)
			if err != nil {
				return err
			}
			ctx := cmd.Context()
			s, err := a.snapshots(ctx, v, cmd.ErrOrStderr())
			if err != nil {
				return err
			}
			defer s.close()
			var del []snapshot
			for _, name := range rest {
				sn, err := s.find(ctx, ws, name)
				if err != nil {
					return err
				}
				if sn.Recreate {
					return fmt.Errorf("snapshot %s holds the /sandbox of an unfinished recreate; finish it first", name)
				}
				del = append(del, sn)
			}
			if err := s.remove(ctx, del); err != nil {
				return err
			}
			for _, sn := range del {
				fmt.Fprintf(cmd.OutOrStdout(), "Deleted snapshot %s.\n", termtext.Escape(sn.Name()))
			}
			return nil
		},
	}
	addWorkspaceFlag(cmd, &workspace)
	return cmd
}

// parseAge reads --older-than: a number of days, such as 30d, or a Go
// duration, such as 12h.
func parseAge(s string) (time.Duration, error) {
	if d, ok := strings.CutSuffix(s, "d"); ok {
		n, err := strconv.Atoi(d)
		if err == nil && n > 0 {
			return time.Duration(n) * 24 * time.Hour, nil
		}
	} else if d, err := time.ParseDuration(s); err == nil && d > 0 {
		return d, nil
	}
	return 0, fmt.Errorf("invalid age %q: want days, such as 30d, or a duration, such as 12h", s)
}

// pruneSnapshots picks the snapshots that prune removes: those of
// sandboxes that are gone, those past the newest keep of each sandbox, and
// with olderThan, those taken longer ago. snaps are newest first within a
// sandbox. Snapshots that an unfinished recreate needs stay.
func pruneSnapshots(snaps []snapshot, keep int, olderThan time.Duration, now time.Time) []snapshot {
	var out []snapshot
	rank := map[string]int{}
	for _, sn := range snaps {
		key := sn.Workspace + "/" + sn.Sandbox
		n := rank[key]
		rank[key]++
		if sn.Recreate {
			continue
		}
		if sn.Gone || n >= keep || olderThan > 0 && now.Sub(sn.Time) > olderThan {
			out = append(out, sn)
		}
	}
	return out
}

func newSnapshotPruneCmd() *cobra.Command {
	var (
		workspace string
		keep      int
		olderThan string
		dryRun    bool
	)
	cmd := &cobra.Command{
		Use:   "prune [VM] [--keep N] [--older-than AGE] [--dry-run] [-w W]",
		Short: "Delete the snapshots of deleted sandboxes and old snapshots",
		Long: `Delete the snapshots of sandboxes that are gone, and of each other sandbox
all but the newest --keep, one by default. --older-than also deletes the
snapshots taken longer ago than AGE, such as 30d, whatever --keep says. -w
prunes only that workspace's snapshots.

prune prints each snapshot it deletes and the space that frees. --dry-run
prints what it would delete and deletes nothing.`,
		Example: `  brig sandbox snapshot prune dev --dry-run
  brig sandbox snapshot prune dev --keep 3 --older-than 30d`,
		Args:              cobra.MaximumNArgs(1),
		ValidArgsFunction: completeVMs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if keep < 0 {
				return errors.New("--keep must not be negative")
			}
			var age time.Duration
			if olderThan != "" {
				var err error
				if age, err = parseAge(olderThan); err != nil {
					return err
				}
			}
			if workspace != "" {
				if err := openshell.CheckWorkspace(workspace); err != nil {
					return err
				}
			}
			a, err := newApp()
			if err != nil {
				return err
			}
			v, err := a.resolveVMArg(cmd, args)
			if err != nil {
				return err
			}
			ctx := cmd.Context()
			s, err := a.snapshots(ctx, v, cmd.ErrOrStderr())
			if err != nil {
				return err
			}
			defer s.close()
			snaps, err := s.list(ctx, true)
			if err != nil {
				return err
			}
			var scope []snapshot
			for _, sn := range snaps {
				if workspace == "" || sn.Workspace == workspace {
					scope = append(scope, sn)
				}
			}
			del := pruneSnapshots(scope, keep, age, s.now())
			out := cmd.OutOrStdout()
			verb := "Deleted"
			if dryRun {
				verb = "Would delete"
			} else if err := s.remove(ctx, del); err != nil {
				return err
			}
			var freed int64
			for _, sn := range del {
				why := ""
				if sn.Gone {
					why = ", whose sandbox is gone"
				}
				fmt.Fprintf(out, "%s snapshot %s (workspace %s)%s, %s.\n", verb, termtext.Escape(sn.Name()), termtext.Escape(sn.Workspace), why, size(sn.Exclusive))
				freed += max(sn.Exclusive, 0)
			}
			switch {
			case len(del) == 0:
				fmt.Fprintln(out, "No snapshots to delete.")
			case dryRun:
				fmt.Fprintf(out, "That would free about %s.\n", bytesize.Size(freed).Approx())
			default:
				fmt.Fprintf(out, "Freed about %s.\n", bytesize.Size(freed).Approx())
			}
			return nil
		},
	}
	cmd.Flags().StringVarP(&workspace, "workspace", "w", "", "prune only the snapshots of this workspace")
	_ = cmd.RegisterFlagCompletionFunc("workspace", completeWorkspaces)
	cmd.Flags().IntVar(&keep, "keep", 1, "keep this many of the newest snapshots of each sandbox")
	cmd.Flags().StringVar(&olderThan, "older-than", "", "also delete snapshots older than this, e.g. 30d")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "print what would be deleted, and delete nothing")
	return cmd
}
