// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/dennisklein/brig/internal/ocsync"
	"github.com/dennisklein/brig/internal/openshell"
	"github.com/dennisklein/brig/internal/vm"
)

func newSyncCmd() *cobra.Command {
	var (
		opts       ocsync.Options
		secretTool string
		format     outputFormat
	)
	cmd := vmCommand("sync [NAME]", "Apply a VM's OpenShell config directories to its gateway", func(ctx context.Context, cmd *cobra.Command, a *app, v *vm.VM) error {
		unlock, err := a.lockLoadedVM(v)
		if err != nil {
			return err
		}
		defer unlock()
		if len(v.OpenShellConfigs) == 0 {
			// Removing the last directory leaves providers from an earlier
			// sync for --prune to delete, so only a VM never synced is refused.
			st, err := ocsync.LoadState(a.vmFile(v, ocsync.StateFile))
			if err != nil {
				return err
			}
			if st.LastSync == nil {
				return fmt.Errorf("VM %s has no OpenShell config directories; add one with brig update %[1]s --add-openshell-config DIR", v.Name)
			}
		}
		cli, err := openshell.Find()
		if err != nil {
			return err
		}
		if _, err := cli.GatewayVersion(ctx, v.GatewayName()); err != nil {
			return notReadyf("the gateway of VM %s does not answer; is the VM running (brig start %[1]s)? %w", v.Name, err)
		}
		if len(opts.Workspaces) == 0 {
			ws, err := envWorkspace()
			if err != nil {
				return err
			}
			if ws != "" {
				opts.Workspaces = []string{ws}
			}
		}
		if format == "json" {
			rep := syncReport{VM: v.Name, DryRun: opts.DryRun, Changes: []ocsync.Change{}}
			opts.Record = func(c ocsync.Change) { rep.Changes = append(rep.Changes, c) }
			err := a.syncOpenShell(ctx, cli, v, opts, secretTool)
			if err != nil {
				rep.Error = err.Error()
			}
			return errors.Join(writeJSON(cmd.OutOrStdout(), rep), syncFailed(err))
		}
		opts.Out, opts.Verbose = cmd.OutOrStdout(), true
		return syncFailed(a.syncOpenShell(ctx, cli, v, opts, secretTool))
	})
	cmd.Long = `Apply a VM's OpenShell config directories to its gateway. brig start does
the same every time the VM starts.

A config directory may hold:
  profiles/*.yaml        OpenShell provider profiles, imported into the
                         gateway's default workspace or updated there
  providers/*.yaml       providers whose credentials brig reads from your
                         keyring with secret-tool, for example:
                           name: github
                           type: github
                           credentials:
                             GH_TOKEN:
                               secret_tool:
                                 lookup: [service, github.com, user, alice]
                         or, where there is no keyring, a credential
                         read from an environment variable of brig's own:
                             GH_TOKEN:
                               env: BRIG_SECRET_GH_TOKEN
  workspaces/WS/profiles/*.yaml, workspaces/WS/providers/*.yaml
                         the same for the workspace WS, which brig
                         creates on the gateway if it is missing
  policies/default.yaml  the default policy for new sandboxes, which
                         brig use NAME and eval "$(brig env NAME)"
                         export as OPENSHELL_SANDBOX_POLICY

Every workspace is synced on its own: the default workspace first, then
the others by name, with a "workspace WS:" heading each when there is more
than one. The same profile ID or provider name may appear in several
workspaces; they are separate objects. --workspace WS (repeatable) syncs
only the named workspaces, by default the one that $BRIG_WORKSPACE names
if it is set. --prune deletes only what brig created in the
workspace it is synced for, and brig never deletes a workspace itself.
policies/ is not per workspace.

Secrets never appear in config directories, on command lines or on the
host's disk (the gateway keeps them encrypted on the VM's data disk): brig
runs "secret-tool lookup application brig ATTRIBUTE VALUE..." (or the
program set as openshell.secret_tool in config.yaml) or reads the
BRIG_SECRET_* variable that a credential names, and hands the value to the openshell CLI in its
environment. A provider is updated only when one of its
secrets changed. Profiles and providers that brig did not create are
only touched when a config directory defines them, and never deleted.

A provider's credentials may be sent to the endpoints of its profile.
brig start keeps back profile changes that would add endpoints for
credentials the gateway holds already; brig sync applies them and says
so. Check shared profiles with --dry-run first.`
	cmd.Example = `  brig sync dev --dry-run
  brig sync dev --prune
  brig sync dev --workspace team`
	f := cmd.Flags()
	f.BoolVar(&opts.DryRun, "dry-run", false, "show what would change, without changing it")
	f.BoolVar(&opts.Prune, "prune", false, "delete profiles and providers that brig created but no config directory defines any more")
	f.BoolVar(&opts.RefreshSecrets, "refresh-secrets", false, "update every provider's credentials, whether or not they changed")
	f.StringArrayVarP(&opts.Workspaces, "workspace", "w", nil, "sync only this workspace (repeatable; default: $"+workspaceEnv+", else every workspace the config directories define)")
	_ = cmd.RegisterFlagCompletionFunc("workspace", completeWorkspaces)
	f.StringVar(&secretTool, "secret-tool", "", "program to look secrets up with (default: openshell.secret_tool from config.yaml)")
	addOutputFlag(cmd, &format)
	return cmd
}

// syncReport is what brig sync -o json prints: each workspace, profile and
// provider with what was done to it, or in a dry run would be, and the
// error, if the sync failed.
type syncReport struct {
	VM      string          `json:"vm"`
	DryRun  bool            `json:"dry_run"`
	Changes []ocsync.Change `json:"changes"`
	Error   string          `json:"error,omitempty"`
}

// syncOpenShell applies the VM's OpenShell config directories to its
// registered gateway and, unless it is a dry run, records the outcome.
func (a *app) syncOpenShell(ctx context.Context, cli *openshell.CLI, v *vm.VM, opts ocsync.Options, secretTool string) error {
	statePath := a.vmFile(v, ocsync.StateFile)
	st, err := ocsync.LoadState(statePath)
	if err != nil {
		return err
	}
	// The report holds names from the config directories and the openshell
	// CLI's error text.
	if opts.Out != nil {
		esc := newEscWriter(opts.Out)
		defer func() { _ = esc.Close() }()
		opts.Out = esc
	}
	err = a.applyConfigs(ctx, cli, v, st, opts, secretTool)
	if opts.DryRun {
		return err
	}
	// A sync of some providers says nothing about the last full sync.
	if len(opts.Providers) == 0 {
		st.LastSync = &ocsync.Record{Time: time.Now().UTC()}
		if err != nil {
			st.LastSync.Error = err.Error()
		}
	}
	return errors.Join(err, st.Save(statePath))
}

func (a *app) applyConfigs(ctx context.Context, cli *openshell.CLI, v *vm.VM, st *ocsync.State, opts ocsync.Options, secretTool string) error {
	set, err := ocsync.Load(v.OpenShellConfigs)
	if err != nil {
		return err
	}
	gw, err := cli.Gateway(v.GatewayName())
	if err != nil {
		return err
	}
	if secretTool == "" {
		secretTool = a.cfg.OpenShell.SecretTool
	}
	return ocsync.SyncAll(ctx, set, workspaceHost{gw}, ocsync.SecretTool{Path: secretTool}, st, opts)
}

// workspaceHost adapts a gateway client to ocsync.Host.
type workspaceHost struct{ *openshell.Gateway }

func (h workspaceHost) In(workspace string) (ocsync.Gateway, error) {
	return h.Gateway.In(workspace)
}

// lastSync returns the outcome of the VM's last sync, if any.
func (a *app) lastSync(v *vm.VM) *ocsync.Record {
	st, err := ocsync.LoadState(a.vmFile(v, ocsync.StateFile))
	if err != nil {
		return nil
	}
	return st.LastSync
}

// configDirs makes the config directory arguments absolute and drops
// duplicates.
func configDirs(args []string) ([]string, error) {
	var dirs []string
	for _, arg := range args {
		dir, err := filepath.Abs(arg)
		if err != nil {
			return nil, err
		}
		if !slices.Contains(dirs, dir) {
			dirs = append(dirs, dir)
		}
	}
	return dirs, nil
}

// shellQuote quotes s for a POSIX shell.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// fishQuote quotes s for fish, which takes \\ and \' in single quotes as
// escapes.
func fishQuote(s string) string {
	return "'" + strings.NewReplacer(`\`, `\\`, `'`, `\'`).Replace(s) + "'"
}

// completeConfigDirs completes the OpenShell config directories of the VM
// named by the first argument.
func completeConfigDirs(_ *cobra.Command, args []string, _ string) ([]string, cobra.ShellCompDirective) {
	v := completionVM(args)
	if v == nil {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	return v.OpenShellConfigs, cobra.ShellCompDirectiveNoFileComp
}

func completeDirs(*cobra.Command, []string, string) ([]string, cobra.ShellCompDirective) {
	return nil, cobra.ShellCompDirectiveFilterDirs
}
