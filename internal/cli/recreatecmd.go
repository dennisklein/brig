// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/GSI-HPC/go-clikit/cliout"
	"github.com/GSI-HPC/go-clikit/termtext"
	"github.com/spf13/cobra"

	"github.com/dennisklein/brig/internal/agent"
	"github.com/dennisklein/brig/internal/guest"
	"github.com/dennisklein/brig/internal/ui/store"
	"github.com/dennisklein/brig/internal/vm"
)

// recreateOpts are the flags that brig sandbox recreate and brig agent
// update share.
type recreateOpts struct {
	workspace string
	dryRun    bool
	yes       bool
	resets    []string
	policy    string
	format    outputFormat
}

func (o *recreateOpts) register(cmd *cobra.Command) {
	addWorkspaceFlag(cmd, &o.workspace)
	f := cmd.Flags()
	f.BoolVar(&o.dryRun, "dry-run", false, "print what would happen, and change nothing")
	f.BoolVarP(&o.yes, "yes", "y", false, "stop sandboxes without asking, also when a program runs in them")
	f.StringArrayVar(&o.resets, "reset", nil, "leave PATH, relative to /sandbox, out of the restore, so that the new image sets it up (repeatable)")
	f.StringVar(&o.policy, "policy", "", "give the new sandbox this policy file, or with file, the sandbox's own (default: its current base policy)")
	addOutputFlag(cmd, &o.format)
}

func (o *recreateOpts) check() error {
	for _, r := range o.resets {
		if err := guest.CheckResetPath(r); err != nil {
			return err
		}
	}
	return nil
}

// recreateItem is a sandbox to recreate and where it goes.
type recreateItem struct {
	name   string
	target recreateTarget
	// copyImage says that the host has the target's image, which a
	// recreate copies into a VM that lacks it.
	copyImage bool
}

// recreateAll plans the recreate of each item and, unless o.dryRun, carries
// it out, one sandbox at a time. It stops at the first failure and prints a
// line for each sandbox.
func (a *app) recreateAll(cmd *cobra.Command, s *snapshots, ws string, items []recreateItem, o recreateOpts) error {
	ctx := cmd.Context()
	out := cmd.OutOrStdout()
	if o.dryRun {
		plans := []*recreatePlan{}
		for i, it := range items {
			p, err := s.planRecreate(ctx, ws, it.name, it.target, planOpts{resets: o.resets, dry: true, copyImage: it.copyImage, policy: o.policy})
			if err != nil {
				return err
			}
			plans = append(plans, p)
			if o.format != "json" {
				if i > 0 {
					fmt.Fprintln(out)
				}
				printRecreatePlan(out, p)
			}
		}
		if o.format == "json" {
			return cliout.WriteJSON(out, plans)
		}
		return nil
	}
	var lines []string
	var failed error
	for _, it := range items {
		if failed != nil {
			lines = append(lines, fmt.Sprintf("Left sandbox %s as it was.", termtext.Escape(it.name)))
			continue
		}
		snap, err := a.recreateOne(cmd, s, ws, it, o)
		if err != nil {
			failed = err
			lines = append(lines, fmt.Sprintf("Did not move sandbox %s: %s", termtext.Escape(it.name), termtext.Escape(firstLine(err.Error()))))
			if len(items) == 1 {
				// The error says it.
				lines = nil
			}
			continue
		}
		lines = append(lines, snap)
	}
	for _, l := range lines {
		fmt.Fprintln(out, l)
	}
	return failed
}

func firstLine(s string) string {
	line, _, _ := strings.Cut(s, "\n")
	return line
}

// recreateOne recreates one sandbox and returns the line that reports it.
func (a *app) recreateOne(cmd *cobra.Command, s *snapshots, ws string, it recreateItem, o recreateOpts) (string, error) {
	ctx := cmd.Context()
	p, err := s.planRecreate(ctx, ws, it.name, it.target, planOpts{resets: o.resets, policy: o.policy})
	if err != nil {
		return "", err
	}
	note := ""
	refuse := ""
	switch {
	case p.Unknown != "":
		note = fmt.Sprintf("Cannot tell what runs in sandbox %s (%s), which stopping it ends.", termtext.Escape(p.Sandbox), termtext.Escape(p.Unknown))
		refuse = fmt.Sprintf("cannot tell what runs in sandbox %s, which stopping it ends", p.Sandbox)
	case len(p.Running) > 0:
		names := make([]string, len(p.Running))
		for i, r := range p.Running {
			names[i] = termtext.Escape(r.String())
		}
		note = fmt.Sprintf("Sandbox %s runs %s.", termtext.Escape(p.Sandbox), strings.Join(names, ", "))
		refuse = fmt.Sprintf("sandbox %s runs %s, which stopping it ends", p.Sandbox, strings.Join(names, ", "))
	}
	if note != "" {
		ap := approval{ctx: ctx, in: cmd.InOrStdin(), w: cmd.ErrOrStderr(), yes: o.yes}
		if err := ap.ask(note+" Stop it and move it to "+termtext.Escape(p.ToImage)+"?", refuse); err != nil {
			return "", err
		}
	}
	snap, err := s.recreate(ctx, p)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("Moved sandbox %s from %s to %s; snapshot %s kept.", termtext.Escape(p.Sandbox), termtext.Escape(p.FromImage),
		termtext.Escape(p.ToImage), termtext.Escape(snap.Name())), nil
}

func newSandboxRecreateCmd() *cobra.Command {
	var (
		o                recreateOpts
		image, template  string
		resume, rollback bool
	)
	cmd := &cobra.Command{
		Use:   "recreate [VM] SANDBOX [--image REF | --template T] [-w W] [--dry-run] [--yes] [--reset PATH]...",
		Short: "Move a sandbox to a new image, keeping its /sandbox",
		Long: `Move SANDBOX to a new image: take a snapshot of it, delete it, create it
again on the new image with the same name, restore the snapshot into the new
sandbox and start it if it was ready. OpenShell cannot change a sandbox's
image, so the sandbox gets a new identity: a new ID, SSH host key and policy
revision history. The snapshot stays; brig sandbox snapshot prune removes it.

Without --image or --template, a sandbox created from a template moves to the
template's current image. --template T moves it to template T, also one
created with --from. --image REF moves it to image REF, with the rest of its
spec as it was; brig copies REF from the host's Podman into the VM if the VM
lacks it.

The new sandbox keeps the providers, labels, command and service exposures,
and gets the sandbox's current base policy: rules added while it ran stay.
--policy file gives it the policy file that the sandbox came from instead,
found as brig policy diff finds it, and --policy FILE that file. A sandbox
from a template gets the template's environment, resources and mounts, as a
new sandbox would.

--reset PATH, relative to /sandbox, leaves PATH out of the restore, so that
the new image's start script sets it up fresh, as for a settings file that
the image seeds only once.

If a program other than a shell runs in the sandbox, such as an agent, brig
names it and asks first; -y/--yes skips the question. Without a terminal it
refuses then, unless --yes. --dry-run checks everything and prints the plan.

If creating, restoring or starting the new sandbox fails, brig creates the old
one again on its old image and restores the snapshot, also after Ctrl-C. If
that fails too, it says so: --resume retries the recreate from where it
stopped, --rollback retries the way back.`,
		Example: `  brig sandbox recreate dev webapp --dry-run
  brig sandbox recreate dev webapp
  brig sandbox recreate dev kit-dev --image localhost/pi-agent:2026-10-14 --reset .pi/agent/settings.json`,
		Args:              cobra.RangeArgs(1, 2),
		ValidArgsFunction: completeVMAndSandbox,
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := o.check(); err != nil {
				return err
			}
			if (resume || rollback) && (image != "" || template != "" || o.dryRun || len(o.resets) > 0 || o.policy != "") {
				return errors.New("--resume and --rollback take only the sandbox")
			}
			ws, err := chosenWorkspace(o.workspace)
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
			name := rest[0]
			if err := checkSandboxName(name); err != nil {
				return err
			}
			ctx := cmd.Context()
			s, err := a.snapshots(ctx, v, cmd.ErrOrStderr())
			if err != nil {
				return err
			}
			defer s.close()
			if resume || rollback {
				return a.finishRecreate(cmd, s, v, ws, name, rollback)
			}
			target := recreateTarget{template: template, image: image}
			if image == "" && template == "" {
				r, err := s.get(ctx, ws, name)
				if err != nil {
					return err
				}
				if r.Template == "" {
					return fmt.Errorf("sandbox %s does not come from a template; give --image or --template", name)
				}
				target.template = r.Template
			}
			item := recreateItem{name: name, target: target}
			if image != "" && !o.dryRun {
				if err := ensureVMImage(ctx, a, v, image, cmd); err != nil {
					return err
				}
			}
			if image != "" && o.dryRun {
				item.copyImage = podmanImageExists(ctx, image)
			}
			return a.recreateAll(cmd, s, ws, []recreateItem{item}, o)
		},
	}
	o.register(cmd)
	f := cmd.Flags()
	f.StringVar(&image, "image", "", "move the sandbox to this image")
	f.StringVar(&template, "template", "", "move the sandbox to this template of the gateway")
	f.BoolVar(&resume, "resume", false, "finish a recreate that stopped half-way")
	f.BoolVar(&rollback, "rollback", false, "undo a recreate that stopped half-way")
	cmd.MarkFlagsMutuallyExclusive("image", "template")
	cmd.MarkFlagsMutuallyExclusive("resume", "rollback")
	_ = cmd.RegisterFlagCompletionFunc("image", func(c *cobra.Command, _ []string, _ string) ([]string, cobra.ShellCompDirective) {
		return completePodmanImages(c.Context())
	})
	return cmd
}

// finishRecreate takes up an unfinished recreate of sandbox name, forward
// or, with back, back.
func (a *app) finishRecreate(cmd *cobra.Command, s *snapshots, v *vm.VM, ws, name string, back bool) error {
	ctx := cmd.Context()
	j, err := a.readJournal(v, ws, name)
	if err != nil {
		return err
	}
	if j == nil {
		return notFound(fmt.Errorf("no recreate of sandbox %s in workspace %s of VM %s is unfinished", name, ws, v.Name))
	}
	out := cmd.OutOrStdout()
	if back {
		if err := s.back(ctx, j); err != nil {
			return err
		}
		fmt.Fprintf(out, "Rolled sandbox %s back to %s.\n", termtext.Escape(name), termtext.Escape(j.Old.Image))
		return nil
	}
	snap, err := s.forward(ctx, j)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "Moved sandbox %s from %s to %s; snapshot %s kept.\n", termtext.Escape(name), termtext.Escape(j.Old.Image),
		termtext.Escape(j.Image), termtext.Escape(snap.Name()))
	return nil
}

func newAgentUpdateCmd() *cobra.Command {
	var o recreateOpts
	cmd := &cobra.Command{
		Use:   "update [VM] NAME [SANDBOX...] [-w W] [--dry-run] [--yes] [--reset PATH]...",
		Short: "Move an agent's sandboxes to its current image, keeping their /sandbox",
		Long: `Move the sandboxes of an agent to the image that brig agent build last
built: find the template that agents/NAME/agent.yaml names, and recreate
every sandbox of that template whose image differs from the template's, as
brig sandbox recreate does. With SANDBOX arguments, only those move.
Sandboxes that brig sandbox run created are left alone.

It works through the sandboxes one at a time, stops at the first failure and
leaves the rest as they were, and prints a line for each sandbox. Each
sandbox keeps a snapshot from before the move.

Run --dry-run first: it checks each sandbox and prints what would happen,
such as programs that the move stops and policy rules that the sandbox has
and its policy file lacks. The sandboxes keep those rules, unless --policy
file gives each the policy file it came from.

Without VM, the one that brig use selected is used. A first argument that
names a VM is the VM.`,
		Example: `  brig agent build dev claude && brig agent update dev claude --dry-run
  brig agent update dev claude
  brig agent update dev pi kit-dev --reset .pi/agent/settings.json`,
		Args:              cobra.MinimumNArgs(1),
		ValidArgsFunction: completeAgentBuild,
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := o.check(); err != nil {
				return err
			}
			ws, err := chosenWorkspace(o.workspace)
			if err != nil {
				return err
			}
			a, err := newApp()
			if err != nil {
				return err
			}
			n := len(args)
			if a.vms.Exists(args[0]) && len(args) > 1 {
				n--
			}
			v, rest, err := a.vmAndRest(cmd, args, n)
			if err != nil {
				return err
			}
			name, only := rest[0], rest[1:]
			if err := agent.CheckName(name); err != nil {
				return err
			}
			for _, sb := range only {
				if err := checkSandboxName(sb); err != nil {
					return err
				}
			}
			def, _, err := findAgent(v, name)
			if err != nil {
				return err
			}
			if def.Template == "" {
				return fmt.Errorf("agent %s names no template; move its sandboxes one by one with brig sandbox recreate --image", name)
			}
			ctx := cmd.Context()
			s, err := a.snapshots(ctx, v, cmd.ErrOrStderr())
			if err != nil {
				return err
			}
			defer s.close()
			items, err := agentSandboxes(cmd, s, ws, def.Template, only)
			if err != nil || len(items) == 0 {
				return err
			}
			return a.recreateAll(cmd, s, ws, items, o)
		},
	}
	o.register(cmd)
	return cmd
}

// agentSandboxes picks the sandboxes of template tmpl in workspace ws to
// move: those named in only, or else those whose image differs from the
// template's, leaving out those of brig sandbox run. It says so when there
// are none.
func agentSandboxes(cmd *cobra.Command, s *snapshots, ws, tmpl string, only []string) ([]recreateItem, error) {
	ctx := cmd.Context()
	tmpls, err := s.gw.Templates(ctx)
	if err != nil {
		return nil, err
	}
	i := slices.IndexFunc(tmpls, func(t store.Template) bool { return t.Workspace == ws && t.Name == tmpl })
	if i < 0 {
		return nil, notFound(fmt.Errorf("VM %s has no template %s in workspace %s; create it with brig agent build", s.v.Name, tmpl, ws))
	}
	image := tmpls[i].Image
	all, err := s.gw.Sandboxes(ctx)
	if err != nil {
		return nil, err
	}
	target := recreateTarget{template: tmpl}
	var items []recreateItem
	if len(only) > 0 {
		for _, name := range only {
			j := slices.IndexFunc(all, func(x store.Sandbox) bool { return x.Workspace == ws && x.Name == name })
			switch {
			case j < 0:
				return nil, notFound(fmt.Errorf("VM %s has no sandbox %s in workspace %s", s.v.Name, name, ws))
			case all[j].Template != tmpl:
				return nil, fmt.Errorf("sandbox %s does not come from template %s; move it with brig sandbox recreate --template %[2]s", name, tmpl)
			}
			items = append(items, recreateItem{name: name, target: target})
		}
		return items, nil
	}
	for _, x := range all {
		if x.Workspace == ws && x.Template == tmpl && x.Image != image && x.Labels[runLabel] == "" {
			items = append(items, recreateItem{name: x.Name, target: target})
		}
	}
	if len(items) == 0 {
		fmt.Fprintf(cmd.OutOrStdout(), "No sandbox of template %s in workspace %s needs moving to %s.\n", termtext.Escape(tmpl), ws, termtext.Escape(image))
	}
	return items, nil
}
