// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/GSI-HPC/go-clikit/termtext"
	"github.com/spf13/cobra"

	"github.com/dennisklein/brig/internal/ocsync"
	"github.com/dennisklein/brig/internal/paths"
	"github.com/dennisklein/brig/internal/policy"
	"github.com/dennisklein/brig/internal/vm"
)

func newPolicySaveCmd() *cobra.Command {
	var (
		rules     []string
		to        string
		dryRun    bool
		yes       bool
		workspace string
	)
	cmd := &cobra.Command{
		Use:   "save [VM] SANDBOX [-w W] [--rule KEY]... [--to FILE]",
		Short: "Write rules added to a sandbox into its policy file",
		Long: `Write the network rules that SANDBOX has and its policy file lacks, such as
rules added with openshell policy update, into that file, so that sandboxes
created from it get them too. brig policy diff shows which file that is and
which rules it lacks.

Without --rule, every unsaved rule is written. A rule that both have but
with different content is written only when --rule names it, and then
replaces the file's version: the file may be the newer one.

brig edits the file's text: comments, blank lines and the other rules stay
as they are. It neither applies the file to other sandboxes nor commits it.

On a terminal, brig shows the change to the file and the hosts that the rules
allow, and asks before it writes; -y/--yes skips the question. Without a
terminal, such as in a script, it writes at once.

A sandbox created from the default policy, policies/default.yaml, is not
saved into it, since every new sandbox would get its rules: --to NAME
creates policies/NAME.yaml in the same directory, a copy of the default with
the rules added. --to also names an existing file to save into. brig
remembers the file of --to as the sandbox's policy file, as brig policy diff
--remember does.`,
		Example: `  brig policy save dev webapp
  brig policy save dev webapp --rule npm --dry-run
  brig policy save webapp --to webapp
  brig policy save dev webapp --yes`,
		Args:              cobra.RangeArgs(1, 2),
		ValidArgsFunction: completeVMAndSandbox,
		RunE: func(cmd *cobra.Command, args []string) error {
			a, err := newApp()
			if err != nil {
				return err
			}
			v, err := a.resolveVMArg(cmd, args[:len(args)-1])
			if err != nil {
				return err
			}
			ws, err := chosenWorkspace(workspace)
			if err != nil {
				return err
			}
			req := saveRequest{
				workspace: ws, sandbox: args[len(args)-1], keys: rules, to: to, dryRun: dryRun,
				confirm: confirmer(cmd.Context(), cmd.InOrStdin(), cmd.ErrOrStderr(), yes),
			}
			return a.policySave(cmd.Context(), v, req, cmd.OutOrStdout(), cmd.ErrOrStderr())
		},
	}
	cmd.Flags().StringArrayVar(&rules, "rule", nil, "save only this rule, by its key under network_policies (repeatable)")
	cmd.Flags().StringVar(&to, "to", "", "save into this policy file, or create policies/NAME.yaml from the sandbox's policy file")
	addWorkspaceFlag(cmd, &workspace)
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "show the change to the file and write nothing")
	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "write without asking, also on a terminal")
	_ = cmd.RegisterFlagCompletionFunc("rule", completeRules)
	_ = cmd.RegisterFlagCompletionFunc("to", completePolicyFiles)
	return cmd
}

// saveRequest is what brig policy save is asked to do.
type saveRequest struct {
	workspace, sandbox string
	// keys are the rules to save, or else every unsaved rule.
	keys   []string
	to     string
	dryRun bool
	// show prints the change and the hosts that it allows before the write,
	// as a dry run does.
	show bool
	// confirm, if set, is shown what saving writes and decides whether to
	// write it.
	confirm func(summary string) (bool, error)
}

// saveTarget is where brig policy save writes, with the comparison that
// found it.
type saveTarget struct {
	rep *policyReport
	// source is the sandbox's policy file.
	source string
	// path is the file to write, or to create when newName is set.
	path    string
	newName bool
}

// findSaveTarget finds the policy file that saving the rules of the sandbox
// writes, and refuses a save that cannot be made, before anything is done.
func (a *app) findSaveTarget(ctx context.Context, v *vm.VM, workspace, sandbox, to string, warn io.Writer) (*saveTarget, error) {
	var set *ocsync.Set
	if len(v.OpenShellConfigs) > 0 {
		var err error
		if set, err = ocsync.Load(v.OpenShellConfigs); err != nil {
			return nil, err
		}
	}
	// --to names an existing file to compare with and save into, or a new
	// file to create next to the sandbox's.
	var target, newName string
	if to != "" {
		path, err := policyFileNamed(set, to)
		var none noPolicyFileError
		switch {
		case err == nil && exists(path):
			target = path
		case err == nil:
			newName = path
		case errors.As(err, &none):
			newName = to
		default:
			return nil, err
		}
	}
	rep, err := a.policyDiff(ctx, v, workspace, sandbox, target, false, warn)
	if err != nil {
		return nil, err
	}
	if rep.Source == nil {
		return nil, fmt.Errorf("cannot tell which policy file sandbox %s came from (see brig policy diff); name the file with --to", sandbox)
	}
	source := rep.Source.Path
	if target == "" {
		target = source
	}
	if newName != "" {
		target = newName
		if !filepath.IsAbs(target) {
			if filepath.Ext(target) == "" {
				target += ".yaml"
			}
			target = filepath.Join(filepath.Dir(source), target)
		}
		if exists(target) {
			return nil, fmt.Errorf("%s exists; name it with its path to save into it", target)
		}
	} else if to == "" && filepath.Base(target) == "default.yaml" {
		return nil, fmt.Errorf("sandbox %s came from the default policy %s, which every new sandbox gets; "+
			"save into a policy file of its own with --to %s, or into the default with --to %s", sandbox, target, sandbox, target)
	}
	return &saveTarget{rep: rep, source: source, path: target, newName: newName != ""}, nil
}

func (a *app) policySave(ctx context.Context, v *vm.VM, req saveRequest, out, warn io.Writer) error {
	workspace, sandbox, to, dryRun := req.workspace, req.sandbox, req.to, req.dryRun
	t, err := a.findSaveTarget(ctx, v, workspace, sandbox, to, warn)
	if err != nil {
		return err
	}
	rep, source, target, newName := t.rep, t.source, t.path, t.newName

	pick, err := pickRules(rep, req.keys)
	if err != nil {
		return err
	}
	if len(pick) == 0 {
		fmt.Fprintf(out, "Nothing to save: %s has every rule of sandbox %s.\n", termtext.Escape(target), termtext.Escape(sandbox))
		var changed []string
		for _, r := range rep.Rules {
			if r.State == policy.Changed {
				changed = append(changed, r.Key)
			}
		}
		if len(changed) > 0 {
			fmt.Fprintf(out, "These rules differ from the file's: %s; --rule KEY writes the sandbox's version.\n", termtext.Escape(strings.Join(changed, ", ")))
		}
		return nil
	}
	raw, err := policy.RawRules(rep.base)
	if err != nil {
		return fmt.Errorf("sandbox %s: %w", sandbox, err)
	}
	var named []policy.NamedRule
	for _, r := range pick {
		named = append(named, policy.NamedRule{Key: r.Key, Rule: raw[r.Key]})
	}

	// Write through a symbolic link, as an editor would.
	from := source
	if !newName {
		from = target
	}
	if resolved, err := filepath.EvalSymlinks(from); err == nil {
		from = resolved
	}
	if !newName {
		target = from
	}
	old, err := os.ReadFile(from)
	if err != nil {
		return err
	}
	fi, err := os.Stat(from)
	if err != nil {
		return err
	}
	edited, err := policy.Save(old, named)
	if err != nil {
		return fmt.Errorf("%s: %w", from, err)
	}
	keyList := make([]string, len(named))
	for i, r := range named {
		keyList[i] = r.Key
	}

	// Show what is saved before it is written, also to those who are asked.
	if dryRun || req.show || req.confirm != nil {
		verb, create := "Saving", "Creating"
		if dryRun || req.confirm != nil {
			verb, create = "Would save", "Would create"
		}
		if newName {
			fmt.Fprintf(out, "%s %s as a copy of %s with rules %s:\n", create, termtext.Escape(target), termtext.Escape(source), termtext.Escape(strings.Join(keyList, ", ")))
		} else {
			fmt.Fprintf(out, "%s rules %s to %s:\n", verb, termtext.Escape(strings.Join(keyList, ", ")), termtext.Escape(target))
		}
		if _, err := io.WriteString(out, termtext.EscapeLines(lineDiff(target, old, edited))); err != nil {
			return err
		}
		fmt.Fprintln(out, "Sandboxes created from the file can reach:")
		for _, r := range named {
			fmt.Fprintf(out, "  %s: %s\n", termtext.Escape(r.Key), termtext.Escape(strings.Join(policy.Endpoints(r.Rule), ", ")))
		}
	}
	for _, r := range pick {
		if r.Audit {
			fmt.Fprintf(out, "Rule %s only logs (enforcement: audit); set enforcement: enforce in the file to make it block.\n", termtext.Escape(r.Key))
		}
	}
	if dryRun {
		return nil
	}
	if req.confirm != nil {
		prompt := "Save these rules to " + termtext.Escape(target) + "? [y/N] "
		if newName {
			prompt = "Create " + termtext.Escape(target) + " with these rules? [y/N] "
		}
		ok, err := req.confirm(prompt)
		if err != nil {
			return err
		}
		if !ok {
			fmt.Fprintln(out, "Nothing saved.")
			return nil
		}
	}
	if newName {
		if err := writeNewFile(target, edited, fi.Mode().Perm()); err != nil {
			return err
		}
	} else {
		// Fail rather than overwrite an edit made meanwhile.
		now, err := os.ReadFile(target)
		if err != nil {
			return err
		}
		if !bytes.Equal(now, old) {
			return fmt.Errorf("%s changed while brig was saving to it; run brig policy save again", target)
		}
		if err := paths.WriteFile(target, edited, fi.Mode().Perm()); err != nil {
			return err
		}
	}
	if to != "" {
		if err := a.rememberPolicySource(v, workspace, sandbox, target); err != nil {
			return err
		}
	}
	fmt.Fprintf(out, "Saved rules %s to %s.\n", termtext.Escape(strings.Join(keyList, ", ")), termtext.Escape(target))
	fmt.Fprintf(out, "Sandboxes created from the file get them; apply it to a running one with openshell policy set NAME --policy %s --wait.\n", termtext.Escape(target))
	return nil
}

// pickRules returns the rules to save: the named ones, or else every
// unsaved one.
func pickRules(rep *policyReport, keys []string) ([]policy.RuleDiff, error) {
	var pick []policy.RuleDiff
	if len(keys) == 0 {
		for _, r := range rep.Rules {
			if r.State == policy.Unsaved {
				pick = append(pick, r)
			}
		}
		return pick, nil
	}
	for _, k := range slices.Compact(slices.Sorted(slices.Values(keys))) {
		i := slices.IndexFunc(rep.Rules, func(r policy.RuleDiff) bool { return r.Key == k })
		switch {
		case i < 0 && !sandboxHasRule(rep, k):
			return nil, fmt.Errorf("sandbox %s has no rule %s", rep.Sandbox, k)
		case i < 0:
			return nil, ruleUnchangedError{k, rep.Sandbox, rep.Source.Path}
		case rep.Rules[i].State == policy.NotApplied:
			return nil, fmt.Errorf("rule %s is in %s only; openshell policy set applies the file to the sandbox", k, rep.Source.Path)
		}
		pick = append(pick, rep.Rules[i])
	}
	return pick, nil
}

// sandboxHasRule reports whether the sandbox of rep has a rule of key.
func sandboxHasRule(rep *policyReport, key string) bool {
	doc, err := policy.Parse(rep.base)
	if err != nil {
		return false
	}
	_, ok := doc.Network[key]
	return ok
}

// ruleUnchangedError is the error of a rule to save that the file has as the
// sandbox does.
type ruleUnchangedError struct{ key, sandbox, file string }

func (e ruleUnchangedError) Error() string {
	return fmt.Sprintf("rule %s of sandbox %s does not differ from %s", e.key, e.sandbox, e.file)
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return !errors.Is(err, fs.ErrNotExist)
}

// writeNewFile writes a file that must not exist yet.
func writeNewFile(path string, data []byte, perm fs.FileMode) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, perm)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		_ = os.Remove(path)
		return err
	}
	return f.Close()
}

// lineDiff shows how b differs from a as one hunk of a unified diff: the
// lines between their common start and their common end.
func lineDiff(name string, a, b []byte) string {
	x := strings.SplitAfter(string(a), "\n")
	y := strings.SplitAfter(string(b), "\n")
	pre := 0
	for pre < len(x) && pre < len(y) && x[pre] == y[pre] {
		pre++
	}
	suf := 0
	for suf < len(x)-pre && suf < len(y)-pre && x[len(x)-1-suf] == y[len(y)-1-suf] {
		suf++
	}
	var w strings.Builder
	fmt.Fprintf(&w, "--- %s\n+++ %s\n@@ -%d,%d +%d,%d @@\n", name, name, pre+1, len(x)-pre-suf, pre+1, len(y)-pre-suf)
	for _, l := range x[pre : len(x)-suf] {
		w.WriteString("-" + strings.TrimSuffix(l, "\n") + "\n")
	}
	for _, l := range y[pre : len(y)-suf] {
		w.WriteString("+" + strings.TrimSuffix(l, "\n") + "\n")
	}
	return w.String()
}
