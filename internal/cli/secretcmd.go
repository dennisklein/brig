// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"slices"
	"strconv"
	"strings"
	"text/tabwriter"
	"unicode/utf8"

	"github.com/GSI-HPC/go-clikit/termtext"
	"github.com/spf13/cobra"
	"golang.org/x/sys/unix"

	"github.com/dennisklein/brig/internal/ocsync"
	"github.com/dennisklein/brig/internal/openshell"
	"github.com/dennisklein/brig/internal/vm"
)

// maxSecretInput bounds the secret that brig secret set reads, as
// ocsync.SecretTool.Lookup bounds the one it reads back.
const maxSecretInput = 1 << 20

// maxTerminalLine is the most that a read from a Linux terminal in
// canonical mode returns, newline included. The terminal cuts a longer line
// to this without an error.
const maxTerminalLine = 4096

func newSecretCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "secret",
		Short: "Store the secrets that providers in the config directories refer to",
	}
	cmd.AddCommand(newSecretSetCmd(), newSecretListCmd())
	return cmd
}

func newSecretSetCmd() *cobra.Command {
	var (
		workspace  string
		secretTool string
		noSync     bool
	)
	cmd := &cobra.Command{
		Use:   "set [VM] PROVIDER [CREDENTIAL]",
		Short: "Store the secret of a provider's credential in the keyring",
		Long: `Store the secret that a credential of PROVIDER refers to, under the attributes
its provider file gives, so that they match what brig sync looks up by
construction. PROVIDER is looked up in the config directories of VM, in the
workspace -w names (default: $` + workspaceEnv + `, else ` + openshell.DefaultWorkspace + `). CREDENTIAL may be left
out when the provider has only one.

With two arguments, the first is the VM if a VM of that name exists, else
PROVIDER and CREDENTIAL; without VM, the one that brig use selected is used.

The secret is read without echo from the terminal, and brig prints only how
long it was. Otherwise it is read from standard input, so
  claude setup-token | brig secret set dev claude-code
works. One trailing newline is removed. It is never taken from an argument
or an environment variable, so it is not in your shell history or in ps.

The terminal takes one line of less than ` + strconv.Itoa(maxTerminalLine-1) + ` bytes. Give a longer
secret, or one of more lines, on standard input.

brig stores it by running the program set as openshell.secret_tool in
config.yaml (default secret-tool) as
  PROG store --label="brig: PROVIDER CREDENTIAL" application brig ATTRIBUTE VALUE...
with the secret on standard input. brig puts application brig before the
attributes of every secret it stores or looks up, so a provider file can
name only secrets stored for brig. A wrapper that takes only lookup fails
there, and brig prints the attributes to store the secret by hand.

If the VM is running, brig then syncs only that provider, and what its
profile needs, so that a changed secret never also applies an unrelated
pending change; --no-sync leaves that to brig sync. A credential that reads
an environment variable (env:) has nothing to store: set the variable.
Running agents keep the old token until they are started again.`,
		Example: `  claude setup-token | brig secret set dev claude-code
  brig secret set dev github
  brig secret set llm LLM_API_KEY`,
		Args:              cobra.RangeArgs(1, 3),
		ValidArgsFunction: completeSecretArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			ws, err := chosenWorkspace(workspace)
			if err != nil {
				return err
			}
			a, err := newApp()
			if err != nil {
				return err
			}
			v, provider, credential, err := a.secretArgs(cmd, args)
			if err != nil {
				return err
			}
			if secretTool == "" {
				secretTool = a.cfg.OpenShell.SecretTool
			}
			return a.secretSet(cmd, v, ws, provider, credential, secretTool, noSync)
		},
	}
	addWorkspaceFlag(cmd, &workspace)
	f := cmd.Flags()
	f.StringVar(&secretTool, "secret-tool", "", "program to store the secret with (default: openshell.secret_tool from config.yaml)")
	f.BoolVar(&noSync, "no-sync", false, "do not sync the provider to the gateway afterwards")
	// It prompts on the terminal.
	cmd.Annotations = map[string]string{noProgress: "true"}
	return cmd
}

// secretArgs resolves the arguments [VM] PROVIDER [CREDENTIAL] of brig secret
// set. Two arguments are VM and PROVIDER if the first names a VM.
func (a *app) secretArgs(cmd *cobra.Command, args []string) (v *vm.VM, provider, credential string, err error) {
	given := ""
	switch len(args) {
	case 2:
		if a.vms.Exists(args[0]) {
			given, args = args[0], args[1:]
		} else if _, err := selectedVM(); err != nil {
			return nil, "", "", fmt.Errorf("no VM called %s; %w", termtext.Escape(args[0]), err)
		}
	case 3:
		given, args = args[0], args[1:]
	}
	if v, err = a.resolveVM(cmd, given); err != nil {
		return nil, "", "", err
	}
	provider = args[0]
	if len(args) > 1 {
		credential = args[1]
	}
	return v, provider, credential, nil
}

// secretProvider returns the provider called name that the config directories
// of v define for workspace ws.
func secretProvider(v *vm.VM, ws, name string) (ocsync.Provider, error) {
	set, err := loadSecretSet(v)
	if err != nil {
		return ocsync.Provider{}, err
	}
	w, ok := set.Workspaces[ws]
	if !ok {
		return ocsync.Provider{}, fmt.Errorf("workspace %s is not defined by the config directories of VM %s; they define %s", ws, v.Name, strings.Join(set.WorkspaceNames(), ", "))
	}
	for _, p := range w.Providers {
		if p.Name == name {
			return p, nil
		}
	}
	var names []string
	for _, p := range w.Providers {
		names = append(names, p.Name)
	}
	if len(names) == 0 {
		return ocsync.Provider{}, fmt.Errorf("the config directories of VM %s define no providers in workspace %s", v.Name, ws)
	}
	return ocsync.Provider{}, fmt.Errorf("the config directories of VM %s define no provider %s in workspace %s; they define %s", v.Name, termtext.Escape(name), ws, strings.Join(names, ", "))
}

// loadSecretSet loads the config directories of v.
func loadSecretSet(v *vm.VM) (*ocsync.Set, error) {
	if len(v.OpenShellConfigs) == 0 {
		return nil, fmt.Errorf("VM %s has no OpenShell config directories; add one with brig update %[1]s --add-openshell-config DIR", v.Name)
	}
	return ocsync.Load(v.OpenShellConfigs)
}

// credentialName returns the key of the credential of p to store: given, or
// the provider's only one.
func credentialName(p ocsync.Provider, given string) (string, error) {
	keys := slices.Sorted(maps.Keys(p.Credentials))
	if given == "" {
		if len(keys) != 1 {
			return "", fmt.Errorf("provider %s has the credentials %s; name one", p.Name, strings.Join(keys, ", "))
		}
		return keys[0], nil
	}
	if _, ok := p.Credentials[given]; !ok {
		return "", fmt.Errorf("provider %s has no credential %s; it has %s", p.Name, termtext.Escape(given), strings.Join(keys, ", "))
	}
	return given, nil
}

func (a *app) secretSet(cmd *cobra.Command, v *vm.VM, ws, providerName, credential, secretTool string, noSync bool) error {
	ctx := cmd.Context()
	p, err := secretProvider(v, ws, providerName)
	if err != nil {
		return err
	}
	key, err := credentialName(p, credential)
	if err != nil {
		return err
	}
	if env := p.Credentials[key].Env; env != "" {
		return fmt.Errorf("credential %s of provider %s reads the environment variable %s, so there is nothing to store; set it where you run brig", key, p.Name, env)
	}
	attrs := p.Credentials[key].SecretTool.Lookup
	secret, err := readSecret(ctx, cmd, fmt.Sprintf("Secret for %s of provider %s: ", key, p.Name))
	if err != nil {
		return err
	}
	tool := ocsync.SecretTool{Path: secretTool}
	if err := tool.Store(ctx, fmt.Sprintf("brig: %s %s", p.Name, key), attrs, secret); err != nil {
		return err
	}
	out := cmd.OutOrStdout()
	fmt.Fprintf(out, "Stored the secret of %s of provider %s (%s).\n", key, p.Name, termtext.Escape(strings.Join(ocsync.KeyringAttributes(attrs), " ")))
	if noSync {
		fmt.Fprintf(cmd.ErrOrStderr(), "Apply it with: brig sync %s --workspace %s\n", v.Name, ws)
	} else if err := a.secretSync(cmd, v, ws, p.Name, secretTool); err != nil {
		return err
	}
	a.noteOtherVMs(ctx, cmd, v, attrs)
	fmt.Fprintln(cmd.ErrOrStderr(), "Running agents keep the old token until they are started again from a new shell.")
	return nil
}

// secretSync syncs provider to the gateway of v, if the VM runs, and says what
// it does instead if it does not.
func (a *app) secretSync(cmd *cobra.Command, v *vm.VM, ws, provider, secretTool string) error {
	ctx := cmd.Context()
	cli, err := openshell.Find()
	if err == nil {
		_, err = cli.GatewayVersion(ctx, v.GatewayName())
	}
	if err != nil {
		fmt.Fprintf(cmd.ErrOrStderr(), "VM %s does not answer, so the provider is not synced; brig start %[1]s applies it.\n", v.Name)
		return nil
	}
	unlock, err := a.lockLoadedVM(v)
	if err != nil {
		return err
	}
	defer unlock()
	opts := ocsync.Options{
		Out: cmd.OutOrStdout(), Verbose: true, HoldNewEndpoints: true,
		Workspaces: []string{ws}, Providers: []string{provider},
	}
	if err := a.syncOpenShell(ctx, cli, v, opts, secretTool); err != nil {
		return syncFailed(fmt.Errorf("the secret is stored, but syncing provider %s failed: %w", provider, err))
	}
	return nil
}

// noteOtherVMs names the other running VMs whose config directories define a
// credential that is looked up by attrs, which brig sync updates there.
func (a *app) noteOtherVMs(ctx context.Context, cmd *cobra.Command, self *vm.VM, attrs []string) {
	cli, err := openshell.Find()
	if err != nil {
		return
	}
	for _, name := range a.vms.Names() {
		if name == self.Name {
			continue
		}
		other, err := a.vms.Load(name)
		if err != nil || !usesSecret(other, attrs) {
			continue
		}
		ctx2, cancel := context.WithTimeout(ctx, completionTimeout)
		_, err = cli.GatewayVersion(ctx2, other.GatewayName())
		cancel()
		if err == nil {
			fmt.Fprintf(cmd.ErrOrStderr(), "VM %s uses the same secret; run brig sync %[1]s.\n", other.Name)
		}
	}
}

// usesSecret reports whether a provider in the config directories of v looks
// a credential up by attrs.
func usesSecret(v *vm.VM, attrs []string) bool {
	if len(v.OpenShellConfigs) == 0 {
		return false
	}
	set, err := ocsync.Load(v.OpenShellConfigs)
	if err != nil {
		return false
	}
	for _, w := range set.Workspaces {
		for _, p := range w.Providers {
			for _, c := range p.Credentials {
				if c.SecretTool != nil && slices.Equal(c.SecretTool.Lookup, attrs) {
					return true
				}
			}
		}
	}
	return false
}

// readSecret reads the secret that brig secret set stores: from the terminal
// without echo if standard input is one, else from standard input.
func readSecret(ctx context.Context, cmd *cobra.Command, prompt string) (string, error) {
	in := cmd.InOrStdin()
	if f, ok := in.(*os.File); ok && isTerminal(f) {
		return readSecretFromTerminal(ctx, f, cmd.ErrOrStderr(), prompt)
	}
	data, err := io.ReadAll(io.LimitReader(in, maxSecretInput+1))
	if err != nil {
		return "", err
	}
	return checkSecret(string(data))
}

// checkSecret removes one trailing newline from s and checks that something
// is left and that it is not too large.
func checkSecret(s string) (string, error) {
	s = strings.TrimSuffix(s, "\n")
	switch {
	case len(s) > maxSecretInput:
		return "", fmt.Errorf("the secret is larger than %d bytes", maxSecretInput)
	case s == "":
		return "", errors.New("no secret on standard input")
	}
	return s, nil
}

// readSecretFromTerminal prompts on w and reads a line from the terminal f
// with echo off. It prints how long the line was, and nothing else of it.
// It refuses a line that the terminal may have cut, and more than one line.
func readSecretFromTerminal(ctx context.Context, f *os.File, w io.Writer, prompt string) (string, error) {
	fd := int(f.Fd())
	old, err := unix.IoctlGetTermios(fd, unix.TCGETS)
	if err != nil {
		return "", err
	}
	silent := *old
	silent.Lflag &^= unix.ECHO
	// TCSETSF discards input not yet read: what was typed ahead before
	// the prompt, and after the secret, the rest of a paste of more lines,
	// which would go to the shell, and to its history, after brig ends.
	if err := unix.IoctlSetTermios(fd, unix.TCSETSF, &silent); err != nil {
		return "", err
	}
	defer func() { _ = unix.IoctlSetTermios(fd, unix.TCSETSF, old) }()
	fmt.Fprint(w, prompt)
	type result struct {
		line string
		err  error
	}
	done := make(chan result, 1)
	go func() {
		line, err := bufio.NewReader(f).ReadString('\n')
		done <- result{line, err}
	}()
	var r result
	select {
	case r = <-done:
	case <-ctx.Done():
		fmt.Fprintln(w)
		return "", ctx.Err()
	}
	fmt.Fprintln(w)
	if r.err != nil && !errors.Is(r.err, io.EOF) {
		return "", r.err
	}
	if len(r.line) >= maxTerminalLine {
		return "", fmt.Errorf("the secret is too long for the terminal, which cuts a line at %d bytes; give it on standard input", maxTerminalLine-1)
	}
	if n, err := unix.IoctlGetInt(fd, unix.TIOCINQ); err == nil && n > 0 {
		return "", errors.New("more than one line entered; give a secret of more lines on standard input")
	}
	secret, err := checkSecret(r.line)
	if err != nil {
		return "", errors.New("no secret entered")
	}
	fmt.Fprintf(w, "Read %d characters.\n", utf8.RuneCountInString(secret))
	return secret, nil
}

// secretStatus is one credential in brig secret list.
type secretStatus struct {
	Provider   string `json:"provider"`
	Credential string `json:"credential"`
	// Source is "secret_tool" or "env".
	Source string `json:"source"`
	// Attributes are the attribute/value pairs of a secret_tool source as
	// the keyring has them, brig's namespace included.
	Attributes []string `json:"attributes,omitempty"`
	// Env is the variable of an env source.
	Env string `json:"env,omitempty"`
	// Status is "stored", "missing", "error" or "not checked".
	Status string `json:"status"`
	Error  string `json:"error,omitempty"`
}

// secretListReport is what brig secret list -o json prints.
type secretListReport struct {
	VM          string         `json:"vm"`
	Workspace   string         `json:"workspace"`
	Credentials []secretStatus `json:"credentials"`
}

func newSecretListCmd() *cobra.Command {
	var (
		workspace  string
		secretTool string
		format     outputFormat
	)
	cmd := vmCommand("list [VM]", "Show which providers' secrets are stored, never their values", func(ctx context.Context, cmd *cobra.Command, a *app, v *vm.VM) error {
		ws, err := chosenWorkspace(workspace)
		if err != nil {
			return err
		}
		set, err := loadSecretSet(v)
		if err != nil {
			return err
		}
		w, ok := set.Workspaces[ws]
		if !ok {
			return fmt.Errorf("workspace %s is not defined by the config directories of VM %s; they define %s", ws, v.Name, strings.Join(set.WorkspaceNames(), ", "))
		}
		if secretTool == "" {
			secretTool = a.cfg.OpenShell.SecretTool
		}
		rep := secretListReport{VM: v.Name, Workspace: ws, Credentials: checkSecrets(ctx, w, ocsync.SecretTool{Path: secretTool})}
		failed := 0
		for _, c := range rep.Credentials {
			if c.Status == "error" || c.Status == "not checked" {
				failed++
			}
		}
		var missing []string
		for _, c := range rep.Credentials {
			if c.Status == "missing" && !slices.Contains(missing, c.Provider) {
				missing = append(missing, c.Provider)
			}
		}
		if format == "json" {
			if err := writeJSON(cmd.OutOrStdout(), rep); err != nil {
				return err
			}
		} else {
			printSecretList(cmd.OutOrStdout(), rep)
			for _, p := range missing {
				fmt.Fprintf(cmd.ErrOrStderr(), "Store the missing secrets of %s with: brig secret set %s %s\n", termtext.Escape(p), v.Name, termtext.Escape(p))
			}
		}
		if failed > 0 {
			return fmt.Errorf("could not check %d of %d secrets", failed, len(rep.Credentials))
		}
		return nil
	})
	cmd.Long = `Show, for each credential of each provider that the VM's config directories
define in a workspace, where its secret comes from and whether it is stored.
brig looks each one up, as brig sync does, but never shows a value. A keyring
that is locked may ask you to unlock it.`
	cmd.Example = `  brig secret list dev
  brig secret list dev -o json`
	addWorkspaceFlag(cmd, &workspace)
	cmd.Flags().StringVar(&secretTool, "secret-tool", "", "program to look secrets up with (default: openshell.secret_tool from config.yaml)")
	addOutputFlag(cmd, &format)
	return cmd
}

// checkSecrets looks up every credential of w's providers, in the order of
// providers and credential keys. After a lookup times out, the rest are not
// tried, as in a sync.
func checkSecrets(ctx context.Context, w ocsync.WorkspaceSet, tool ocsync.Secrets) []secretStatus {
	list := []secretStatus{}
	stopped := false
	for _, p := range w.Providers {
		for _, key := range slices.Sorted(maps.Keys(p.Credentials)) {
			c := p.Credentials[key]
			s := secretStatus{Provider: p.Name, Credential: key}
			var err error
			switch {
			case c.Env != "":
				s.Source, s.Env = "env", c.Env
				_, err = ocsync.LookupEnv(c.Env)
			case stopped:
				s.Source, s.Attributes = "secret_tool", ocsync.KeyringAttributes(c.SecretTool.Lookup)
				s.Status = "not checked"
				list = append(list, s)
				continue
			default:
				s.Source, s.Attributes = "secret_tool", ocsync.KeyringAttributes(c.SecretTool.Lookup)
				_, err = tool.Lookup(ctx, c.SecretTool.Lookup)
			}
			switch {
			case err == nil:
				s.Status = "stored"
			case errors.Is(err, ocsync.ErrSecretNotFound):
				s.Status = "missing"
			default:
				s.Status, s.Error = "error", err.Error()
				stopped = stopped || errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled)
			}
			list = append(list, s)
		}
	}
	return list
}

func printSecretList(w io.Writer, rep secretListReport) {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "PROVIDER\tCREDENTIAL\tSOURCE\tSTATUS")
	for _, c := range rep.Credentials {
		source := "env " + c.Env
		if c.Source == "secret_tool" {
			source = "secret-tool " + strings.Join(c.Attributes, " ")
		}
		status := c.Status
		if c.Error != "" {
			status += ": " + c.Error
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", termtext.Escape(c.Provider), c.Credential, termtext.Escape(source), termtext.Escape(status))
	}
	_ = tw.Flush()
}

// completeSecretArgs completes [VM] PROVIDER [CREDENTIAL] of brig secret set.
func completeSecretArgs(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
	const noFiles = cobra.ShellCompDirectiveNoFileComp
	ws := completionWorkspace(cmd)
	var vmName string
	if len(args) > 0 {
		if dirs, ok := completionVMNames(); ok && slices.Contains(dirs, args[0]) {
			vmName, args = args[0], args[1:]
		}
	}
	var v *vm.VM
	if vmName != "" {
		v = completionVM([]string{vmName})
	} else {
		v = completionVM(nil)
	}
	var words []string
	switch len(args) {
	case 0:
		if vmName == "" {
			words, _ = completeVMs(cmd, nil, toComplete)
		}
		if v != nil {
			words = append(words, completionProviders(v, ws)...)
		}
	case 1:
		if v != nil {
			if p, err := secretProvider(v, ws, args[0]); err == nil {
				words = slices.Sorted(maps.Keys(p.Credentials))
			}
		}
	}
	return words, noFiles
}

// completionVMNames returns the names of the VMs.
func completionVMNames() ([]string, bool) {
	words, dir := completeVMs(nil, nil, "")
	return words, dir != cobra.ShellCompDirectiveError
}

// completionProviders returns the names of the providers that the config
// directories of v define for workspace ws.
func completionProviders(v *vm.VM, ws string) []string {
	set, err := ocsync.Load(v.OpenShellConfigs)
	if err != nil {
		return nil
	}
	var names []string
	for _, p := range set.Workspaces[ws].Providers {
		names = append(names, p.Name)
	}
	return names
}
