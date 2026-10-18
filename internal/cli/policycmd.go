// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"

	"github.com/GSI-HPC/go-clikit/termtext"
	"github.com/spf13/cobra"

	"github.com/dennisklein/brig/internal/ocsync"
	"github.com/dennisklein/brig/internal/openshell"
	"github.com/dennisklein/brig/internal/policy"
	"github.com/dennisklein/brig/internal/vm"
)

// policySourcesFile holds the policy file the user chose for each sandbox of
// a VM, as a JSON object from "WORKSPACE/SANDBOX" to absolute path. Records
// keyed by the sandbox name alone, from before workspaces, are ignored.
const policySourcesFile = "policy-sources.json"

// Exit codes of brig policy diff besides 0 for no difference and 1 for an error.
const (
	exitDrift         = 1
	exitSourceUnknown = 2
)

func newPolicyCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "policy",
		Short: "Compare sandbox policies with the policy files in the config directories, and save rules to them",
	}
	cmd.AddCommand(newPolicyDiffCmd(), newPolicyAllowCmd(), newPolicySaveCmd())
	return cmd
}

// policySource says which file a sandbox's policy was compared with.
type policySource struct {
	Path string `json:"path"`
	// How is "given" for --source, "recorded" for an earlier choice of the
	// user, and "inferred" otherwise.
	How string `json:"how"`
}

// policyReport is the output of brig policy diff -o json.
type policyReport struct {
	VM        string `json:"vm"`
	Workspace string `json:"workspace"`
	Sandbox   string `json:"sandbox"`
	// Source is nil when brig cannot tell which file the sandbox came from.
	Source *policySource `json:"source"`
	// Candidates are the policy files that fit equally well, or all that
	// fit; with no Source, the user picks one.
	Candidates []string `json:"candidates,omitempty"`
	// Drift is true when the sandbox and its source differ.
	Drift bool `json:"drift"`
	policy.Result

	// base is the sandbox's base policy as openshell printed it.
	base []byte
}

func newPolicyDiffCmd() *cobra.Command {
	var (
		format    outputFormat
		source    string
		remember  bool
		workspace string
	)
	cmd := &cobra.Command{
		Use:   "diff [VM] SANDBOX [-w W]",
		Short: "Show how a sandbox's network rules differ from its policy file",
		Long: `Compare the base policy of SANDBOX (openshell policy get --base) with the
policy file it came from, rule by rule. A rule is

  unsaved      in the sandbox only, such as one added with openshell policy update
  not_applied  in the file only
  changed      in both, with different content

Sections other than the network rules, such as filesystem_policy, are fixed
once a sandbox starts; brig lists those that differ and offers nothing for
them. Reformatting a file is not a difference.

The file is looked for among the policies/*.yaml files of the VM's OpenShell
config directories: the one that agrees with the sandbox in everything but
network rules and differs from it in the fewest rules, preferring
policies/SANDBOX.yaml and then policies/default.yaml. --source picks the file
instead, and --remember keeps that choice for the sandbox, ahead of the
inference. Without VM, the one that brig use selected is used.

The exit status is 0 when there is no difference, 1 when there is, and 2 when
brig cannot tell which file to compare with.`,
		Example: `  brig policy diff dev webapp
  brig policy diff webapp -o json
  brig policy diff dev webapp --source policies/webapp.yaml --remember`,
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
			if remember && source == "" {
				return errors.New("--remember needs --source")
			}
			ws, err := chosenWorkspace(workspace)
			if err != nil {
				return err
			}
			rep, err := a.policyDiff(cmd.Context(), v, ws, args[len(args)-1], source, remember, cmd.ErrOrStderr())
			if err != nil {
				return err
			}
			if err := printPolicyReport(cmd.OutOrStdout(), rep, format); err != nil {
				return err
			}
			switch {
			case rep.Source == nil:
				return exitError{exitSourceUnknown}
			case rep.Drift:
				return exitError{exitDrift}
			}
			return nil
		},
	}
	addOutputFlag(cmd, &format)
	cmd.Flags().StringVar(&source, "source", "", "compare with this policy file, or the policy file of the config directories with this name")
	cmd.Flags().BoolVar(&remember, "remember", false, "keep the --source for this sandbox")
	addWorkspaceFlag(cmd, &workspace)
	_ = cmd.RegisterFlagCompletionFunc("source", completePolicyFiles)
	return cmd
}

// exitError ends brig with an exit status and no message, for an outcome
// that is not a failure.
type exitError struct{ code int }

func (e exitError) Error() string { return fmt.Sprintf("exit status %d", e.code) }

// policyDiff compares the policy of the sandbox on v's gateway with its
// source file. Files that cannot be read are noted on warn and not
// considered.
func (a *app) policyDiff(ctx context.Context, v *vm.VM, workspace, sandbox, source string, remember bool, warn io.Writer) (*policyReport, error) {
	var set *ocsync.Set
	if len(v.OpenShellConfigs) > 0 {
		var err error
		if set, err = ocsync.Load(v.OpenShellConfigs); err != nil {
			return nil, err
		}
	}
	cli, err := openshell.Find()
	if err != nil {
		return nil, err
	}
	raw, err := cli.BasePolicy(ctx, v.GatewayName(), workspace, sandbox)
	if err != nil {
		return nil, fmt.Errorf("the base policy of sandbox %s in workspace %s of VM %s (is the VM running?): %w", sandbox, workspace, v.Name, err)
	}
	have, err := policy.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("sandbox %s: %w", sandbox, err)
	}
	rep := &policyReport{VM: v.Name, Workspace: workspace, Sandbox: sandbox, Result: policy.Result{Rules: []policy.RuleDiff{}, Fixed: []string{}}, base: raw}

	choose := func(path, how string) (*policyReport, error) {
		doc, err := readPolicyFile(path)
		if err != nil {
			return nil, err
		}
		rep.Source, rep.Result = &policySource{Path: path, How: how}, policy.Diff(have, doc)
		rep.Drift = rep.Result.Drift()
		return rep, nil
	}
	if source != "" {
		path, err := policyFileNamed(set, source)
		if err != nil {
			return nil, err
		}
		if remember {
			if err := a.rememberPolicySource(v, workspace, sandbox, path); err != nil {
				return nil, err
			}
		}
		return choose(path, "given")
	}
	if path, err := a.recordedPolicySource(v, workspace, sandbox); err != nil {
		fmt.Fprintf(warn, "Warning: %s\n", termtext.Escape(err.Error()))
	} else if path != "" {
		if rep, err := choose(path, "recorded"); err == nil {
			return rep, nil
		} else if !errors.Is(err, fs.ErrNotExist) {
			return nil, err
		}
		fmt.Fprintf(warn, "Warning: the policy file recorded for sandbox %s, %s, is gone; looking for another\n", sandbox, termtext.Escape(path))
	}
	var cands []policy.Candidate
	if set != nil {
		for _, f := range set.Policies {
			doc, err := readPolicyFile(f.Path)
			if err != nil {
				fmt.Fprintf(warn, "Warning: %s\n", termtext.Escape(err.Error()))
				continue
			}
			cands = append(cands, policy.Candidate{Name: f.Name, Path: f.Path, Doc: doc})
		}
	}
	best, tied := policy.Infer(sandbox, have, cands)
	if best != nil {
		return choose(best.Path, "inferred")
	}
	// Unknown: offer the files that fit as well as any, or else all.
	if len(tied) == 0 {
		tied = cands
	}
	for _, c := range tied {
		rep.Candidates = append(rep.Candidates, c.Path)
	}
	return rep, nil
}

func readPolicyFile(path string) (*policy.Doc, error) {
	data, err := ocsync.ReadConfigFile(path)
	if err != nil {
		return nil, err
	}
	doc, err := policy.Parse(data)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return doc, nil
}

// policyFileNamed resolves a --source: a path to a file, or the name of a
// policy file in the config directories, with or without its extension.
func policyFileNamed(set *ocsync.Set, source string) (string, error) {
	if strings.ContainsRune(source, filepath.Separator) {
		return filepath.Abs(source)
	}
	var found []string
	if set != nil {
		for _, f := range set.Policies {
			if f.Name == source || filepath.Base(f.Path) == source {
				found = append(found, f.Path)
			}
		}
	}
	switch len(found) {
	case 1:
		return found[0], nil
	case 0:
		if _, err := os.Stat(source); err == nil {
			return filepath.Abs(source)
		}
		return "", noPolicyFileError{source}
	}
	return "", fmt.Errorf("policy file %q is in several config directories: %s; give its path", source, strings.Join(found, ", "))
}

// noPolicyFileError is the error of policyFileNamed for a name that no
// policy file has.
type noPolicyFileError struct{ name string }

func (e noPolicyFileError) Error() string {
	return fmt.Sprintf("no policy file %q in the VM's OpenShell config directories; give its path", e.name)
}

// policySourceKey is the key of a sandbox in policySourcesFile. Names have no
// slash, so the key is unambiguous.
func policySourceKey(workspace, sandbox string) string { return workspace + "/" + sandbox }

func (a *app) recordedPolicySource(v *vm.VM, workspace, sandbox string) (string, error) {
	data, err := os.ReadFile(a.vmFile(v, policySourcesFile))
	if errors.Is(err, fs.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	var m map[string]string
	if err := json.Unmarshal(data, &m); err != nil {
		return "", fmt.Errorf("%s: %w", policySourcesFile, err)
	}
	return m[policySourceKey(workspace, sandbox)], nil
}

func (a *app) rememberPolicySource(v *vm.VM, workspace, sandbox, path string) error {
	unlock, err := a.lockVM(v.Name)
	if err != nil {
		return err
	}
	defer unlock()
	m := map[string]string{}
	if data, err := os.ReadFile(a.vmFile(v, policySourcesFile)); err == nil {
		// An unreadable file is replaced: it holds nothing but a choice.
		_ = json.Unmarshal(data, &m)
	}
	m[policySourceKey(workspace, sandbox)] = path
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	return vm.WriteFileAtomic(a.vmFile(v, policySourcesFile), append(data, '\n'), 0o600)
}

func printPolicyReport(w io.Writer, rep *policyReport, format outputFormat) error {
	if format == "json" {
		return writeJSON(w, rep)
	}
	if rep.Source == nil {
		fmt.Fprintf(w, "Cannot tell which policy file sandbox %s of VM %s came from.\n", termtext.Escape(rep.Sandbox), termtext.Escape(rep.VM))
		if len(rep.Candidates) == 0 {
			fmt.Fprintln(w, "The VM's OpenShell config directories have no policy files that fit.")
		}
		for _, c := range rep.Candidates {
			fmt.Fprintf(w, "  %s\n", termtext.Escape(c))
		}
		fmt.Fprintln(w, "Choose one with --source FILE; --remember keeps the choice.")
		return nil
	}
	fmt.Fprintf(w, "Sandbox %s of VM %s, compared with %s (%s)\n", termtext.Escape(rep.Sandbox), termtext.Escape(rep.VM), termtext.Escape(rep.Source.Path), rep.Source.How)
	if !rep.Drift {
		fmt.Fprintln(w, "No difference.")
		return nil
	}
	if len(rep.Rules) > 0 {
		tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
		fmt.Fprintln(tw, "STATE\tRULE\tNOTE")
		for _, r := range rep.Rules {
			note := ""
			if r.Audit {
				note = "audit mode: logs, does not block"
			}
			fmt.Fprintf(tw, "%s\t%s\t%s\n", r.State, termtext.Escape(r.Key), note)
		}
		if err := tw.Flush(); err != nil {
			return err
		}
	}
	if len(rep.Fixed) > 0 {
		fmt.Fprintf(w, "Differ and take effect only when the sandbox is recreated: %s\n", termtext.Escape(strings.Join(rep.Fixed, ", ")))
	}
	return nil
}
