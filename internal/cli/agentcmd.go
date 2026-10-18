// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"slices"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/GSI-HPC/go-clikit/termtext"
	"github.com/spf13/cobra"

	"github.com/dennisklein/brig/internal/agent"
	"github.com/dennisklein/brig/internal/ocsync"
	"github.com/dennisklein/brig/internal/openshell"
	"github.com/dennisklein/brig/internal/vm"
)

func newAgentCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "agent",
		Short: "Set up a coding agent's image, provider and template, rebuild its image and move its sandboxes to it",
	}
	cmd.AddCommand(newAgentInitCmd(), newAgentBuildCmd(), newAgentUpdateCmd())
	return cmd
}

// agentInitOpts are the flags of brig agent init.
type agentInitOpts struct {
	spec       agent.Spec
	dir        string
	secretsDir string
	vm         string
	force      bool
}

func newAgentInitCmd() *cobra.Command {
	var o agentInitOpts
	cmd := &cobra.Command{
		Use:   "init claude|pi|opencode [NAME] [--dir DIR] [--vm VM]",
		Short: "Write the files for a first agent sandbox, and with --vm set it up",
		Long: `Write what an agent sandbox of the given kind needs, as brig's agents guide
(docs/how-to/agents.md) describes it, without touching any VM:

  DIR/agents/NAME/       agent.yaml, Containerfile, start script, settings
  DIR/profiles/          the agent's provider profile, unless a directory
                         defines it
  DIR/policies/default.yaml
                         a default policy, unless a directory defines one
  SECRETS-DIR/providers/ the agent's provider, which only points into your
                         keyring (default ~/.config/brig/openshell)

NAME defaults to the kind. DIR is the shared directory, often in git; with
--vm it may be left out if the VM has exactly one config directory without
providers/. brig never overwrites: a file that exists with the same content
is skipped, and different content stops the command with the list of
conflicts, unless --force.

claude uses a Claude subscription token (claude setup-token). pi and opencode
use an OpenAI-compatible --endpoint with a --model, and share one llm profile
and provider; opencode can instead use --anthropic, Anthropic's API with an API
key. --github LOGIN adds a github provider. The image versions are pinned
and move with brig releases.

Without --vm it prints the commands that finish the setup. With --vm it runs
them, naming each: it attaches the directories to the VM, asks for the
secrets that are missing (on a terminal only; otherwise it prints the
commands), syncs, builds the image with brig agent build, and prints the
command that starts a sandbox.`,
		Example: `  brig agent init claude --dir ~/src/agent-openshell --vm dev
  brig agent init pi --dir ~/src/agent-openshell --endpoint https://llm.example.com/v1 --model my-model
  brig agent init opencode --anthropic --github alice --vm dev`,
		Args:              cobra.RangeArgs(1, 2),
		ValidArgsFunction: completeAgentKinds,
		RunE: func(cmd *cobra.Command, args []string) error {
			var err error
			if o.spec.Kind, err = agent.ParseKind(args[0]); err != nil {
				return err
			}
			if len(args) > 1 {
				o.spec.Name = args[1]
			}
			a, err := newApp()
			if err != nil {
				return err
			}
			return a.agentInit(cmd, o)
		},
	}
	f := cmd.Flags()
	f.StringVar(&o.dir, "dir", "", "the shared config directory to write into")
	f.StringVar(&o.secretsDir, "secrets-dir", "", "the personal config directory for provider files (default: openshell in brig's config directory)")
	f.StringVar(&o.spec.Endpoint, "endpoint", "", "pi, opencode: the base URL of an OpenAI-compatible API")
	f.StringVar(&o.spec.Model, "model", "", "pi, opencode: the model to use at the endpoint; with --anthropic, an Anthropic model ID (optional)")
	f.BoolVar(&o.spec.Anthropic, "anthropic", false, "opencode: use Anthropic's API with an API key")
	f.StringVar(&o.spec.GitHub, "github", "", "also write a github provider whose token is in the keyring for this login")
	f.StringVar(&o.vm, "vm", "", "also attach the directories to this VM, store missing secrets, sync and build the image")
	f.BoolVar(&o.force, "force", false, "overwrite files that exist with other content")
	_ = cmd.RegisterFlagCompletionFunc("vm", func(c *cobra.Command, _ []string, t string) ([]string, cobra.ShellCompDirective) {
		return completeVMs(c, nil, t)
	})
	_ = cmd.MarkFlagDirname("dir")
	_ = cmd.MarkFlagDirname("secrets-dir")
	// It prompts for secrets and runs brig commands that draw on the terminal.
	cmd.Annotations = map[string]string{noProgress: "true"}
	return cmd
}

func completeAgentKinds(_ *cobra.Command, args []string, _ string) ([]string, cobra.ShellCompDirective) {
	if len(args) > 0 {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	var words []string
	for _, k := range agent.Kinds {
		words = append(words, string(k))
	}
	return words, cobra.ShellCompDirectiveNoFileComp
}

// sharedConfigDir returns the one config directory of v without providers/,
// which is where the shared files go.
func sharedConfigDir(v *vm.VM) (string, error) {
	var shared []string
	for _, dir := range v.OpenShellConfigs {
		if _, err := os.Stat(filepath.Join(dir, "providers")); errors.Is(err, fs.ErrNotExist) {
			shared = append(shared, dir)
		}
	}
	if len(shared) != 1 {
		return "", fmt.Errorf("give --dir: VM %s has %d config directories without providers/, not exactly one", v.Name, len(shared))
	}
	return shared[0], nil
}

func (a *app) agentInit(cmd *cobra.Command, o agentInitOpts) error {
	var v *vm.VM
	if o.vm != "" {
		var err error
		if v, err = a.vms.Load(o.vm); err != nil {
			return err
		}
	}
	if o.dir == "" {
		if v == nil {
			return errors.New("give --dir, the config directory to write into; --vm takes the VM's only directory without providers/")
		}
		var err error
		if o.dir, err = sharedConfigDir(v); err != nil {
			return err
		}
	}
	if o.secretsDir == "" {
		o.secretsDir = filepath.Join(filepath.Dir(a.dirs.ConfigFile()), "openshell")
	}
	var err error
	roots := agent.Roots{}
	if roots.Shared, err = filepath.Abs(o.dir); err != nil {
		return err
	}
	if roots.Secrets, err = filepath.Abs(o.secretsDir); err != nil {
		return err
	}
	if o.spec.User, err = currentUser(); err != nil {
		return err
	}
	plan, err := agent.New(o.spec)
	if err != nil {
		return err
	}
	// What the config directories define already is not written again.
	var known []string
	for _, dir := range append([]string{roots.Shared, roots.Secrets}, vmConfigs(v)...) {
		if fi, err := os.Stat(dir); err == nil && fi.IsDir() && !slices.Contains(known, dir) {
			known = append(known, dir)
		}
	}
	var set *ocsync.Set
	if len(known) > 0 {
		if set, err = ocsync.Load(known); err != nil {
			return err
		}
	}
	results, err := agent.Write(plan.Files, roots, o.force, func(f agent.File) string {
		return definedElsewhere(set, f, filepath.Join(roots.Shared, filepath.FromSlash(f.Path)), filepath.Join(roots.Secrets, filepath.FromSlash(f.Path)))
	})
	if err != nil {
		return err
	}
	out := cmd.OutOrStdout()
	fmt.Fprintf(out, "Agent %s (%s):\n", plan.Name, o.spec.Kind)
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	for _, r := range results {
		note := ""
		if r.Note != "" {
			note = " (" + termtext.Escape(r.Note) + ")"
		}
		fmt.Fprintf(tw, "  %s\t%s%s\n", r.State, termtext.Escape(r.Path), note)
	}
	_ = tw.Flush()
	st := agentSetup{app: a, cmd: cmd, plan: plan, roots: roots, kind: o.spec.Kind, vm: v}
	if v == nil {
		return st.printNext()
	}
	return st.run(cmd.Context())
}

// vmConfigs returns the config directories of v, which may be nil.
func vmConfigs(v *vm.VM) []string {
	if v == nil {
		return nil
	}
	return v.OpenShellConfigs
}

// currentUser returns the login that names the keyring entries of the Claude
// and Anthropic providers.
func currentUser() (string, error) {
	if name := os.Getenv("USER"); name != "" {
		return name, nil
	}
	u, err := user.Current()
	if err != nil {
		return "", fmt.Errorf("cannot tell your login: %w", err)
	}
	return u.Username, nil
}

// definedElsewhere says why f, a profile, provider or default policy, is not
// written: a config directory defines it in another file already. The
// candidate targets are the paths f would have in the shared and the personal
// directory.
func definedElsewhere(set *ocsync.Set, f agent.File, shared, secrets string) string {
	if set == nil || f.Defines == "" {
		return ""
	}
	target := shared
	if f.Root == agent.Secrets {
		target = secrets
	}
	kind, name, _ := strings.Cut(f.Defines, ":")
	var at string
	switch kind {
	case "profile":
		if p, ok := set.Workspaces[openshell.DefaultWorkspace].Profile(name); ok {
			at = p.Path
		}
	case "provider":
		for _, p := range set.Workspaces[openshell.DefaultWorkspace].Providers {
			if p.Name == name {
				at = p.Path
			}
		}
	case "policy":
		at = set.DefaultPolicy
	}
	if at == "" || sameFile(at, target) {
		return ""
	}
	return fmt.Sprintf("%s %s is defined in %s already", kind, name, at)
}

// sameFile reports whether a and b are the same path, or the same file.
func sameFile(a, b string) bool {
	if filepath.Clean(a) == filepath.Clean(b) {
		return true
	}
	fa, errA := os.Stat(a)
	fb, errB := os.Stat(b)
	return errA == nil && errB == nil && os.SameFile(fa, fb)
}

// agentSetup is what brig agent init does after the files are written.
type agentSetup struct {
	app   *app
	cmd   *cobra.Command
	plan  *agent.Plan
	roots agent.Roots
	kind  agent.Kind
	vm    *vm.VM
}

// brigStep runs a brig command for brig agent init; tests replace it.
var brigStep = func(ctx context.Context, cmd *cobra.Command, args ...string) error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	c := exec.CommandContext(ctx, exe, args...)
	c.Stdin, c.Stdout, c.Stderr = cmd.InOrStdin(), cmd.OutOrStdout(), cmd.ErrOrStderr()
	return c.Run()
}

// cmdLine formats a brig command line, quoting what a shell would split.
func cmdLine(args ...string) string {
	words := make([]string, len(args))
	for i, a := range args {
		words[i] = a
		if a == "" || strings.ContainsFunc(a, func(r rune) bool {
			return !strings.ContainsRune("abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789-._/:@%+=,~", r)
		}) {
			words[i] = shellQuote(a)
		}
	}
	return "brig " + strings.Join(words, " ")
}

// attachArgs returns the arguments of brig update that attach the agent's
// directories to a VM that lacks them, or nil.
func (s agentSetup) attachArgs(vmName string, attached []string) []string {
	args := []string{"update", vmName}
	for _, dir := range []string{s.roots.Shared, s.roots.Secrets} {
		if !slices.Contains(attached, dir) {
			args = append(args, "--add-openshell-config", dir)
		}
	}
	if len(args) == 2 {
		return nil
	}
	return args
}

// sandboxArgs returns the arguments of brig sandbox create that start a
// sandbox of the agent.
func (s agentSetup) sandboxArgs(vmName string) []string {
	args := []string{"sandbox", "create", vmName, "--connect", "--", "--name", "webapp", "--template", s.plan.Definition.Template}
	for _, p := range s.plan.Definition.Providers {
		args = append(args, "--provider", p)
	}
	return args
}

// secretNote says what to do before a secret can be stored.
func (s agentSetup) secretNote(provider string) string {
	if provider == "claude-code" {
		return "Run claude setup-token first; it prints the token to store."
	}
	return ""
}

// printNext prints the commands that finish the setup, for a VM that is not
// given.
func (s agentSetup) printNext() error {
	w := cmdWriter{s.cmd.OutOrStdout()}
	fmt.Fprintln(w, "\nNext, with your VM's name for VM:")
	if args := s.attachArgs("VM", nil); args != nil {
		w.line(args...)
	}
	for _, p := range s.plan.Definition.Providers {
		if n := s.secretNote(p); n != "" {
			fmt.Fprintf(w, "  # %s\n", n)
		}
		w.line("secret", "set", "VM", p)
	}
	w.line("sync", "VM")
	w.line("agent", "build", "VM", s.plan.Name)
	w.line(s.sandboxArgs("VM")...)
	return nil
}

// cmdWriter prints indented brig command lines.
type cmdWriter struct{ io.Writer }

func (w cmdWriter) line(args ...string) { fmt.Fprintf(w, "  %s\n", cmdLine(args...)) }

// run does the steps for a VM, naming each as the brig command it runs. It
// stops at the first that fails and says what is left to do by hand.
func (s agentSetup) run(ctx context.Context) error {
	v, out := s.vm, s.cmd.OutOrStdout()
	var steps []step
	if args := s.attachArgs(v.Name, v.OpenShellConfigs); args != nil {
		steps = append(steps, step{args: args})
	}
	pending := s.missingSecrets(ctx)
	interactive := false
	if f, ok := s.cmd.InOrStdin().(*os.File); ok {
		interactive = isTerminal(f)
	}
	var later []string
	for _, p := range pending {
		if interactive {
			steps = append(steps, step{args: []string{"secret", "set", v.Name, p}, note: s.secretNote(p)})
		} else {
			later = append(later, cmdLine("secret", "set", v.Name, p))
		}
	}
	// Providers whose secrets are not stored yet fail the sync, which is no
	// reason not to build the image.
	steps = append(steps,
		step{args: []string{"sync", v.Name}, tolerate: len(pending) > 0 && !interactive, needsVM: true},
		step{args: []string{"agent", "build", v.Name, s.plan.Name}, needsVM: true})
	for i, st := range steps {
		if st.needsVM && !s.app.gatewayUp(ctx, v) {
			fmt.Fprintf(out, "\nVM %s does not answer. Start it with brig start %[1]s, then run:\n", v.Name)
			w := cmdWriter{out}
			for _, rest := range steps[i:] {
				w.line(rest.args...)
			}
			for _, l := range later {
				fmt.Fprintf(out, "  %s\n", l)
			}
			return notReadyf("VM %s does not answer; the files are written and the directories attached", v.Name)
		}
		if st.note != "" {
			fmt.Fprintf(out, "\n%s\n", st.note)
		}
		fmt.Fprintf(out, "\n$ %s\n", cmdLine(st.args...))
		if err := brigStep(ctx, s.cmd, st.args...); err != nil {
			if st.tolerate {
				fmt.Fprintf(s.cmd.ErrOrStderr(), "Not all providers synced, as expected until their secrets are stored: %v\n", err)
				continue
			}
			w := cmdWriter{s.cmd.ErrOrStderr()}
			fmt.Fprintf(w, "\n%s failed. To go on after fixing that, run:\n", cmdLine(st.args...))
			for _, rest := range steps[i:] {
				w.line(rest.args...)
			}
			return fmt.Errorf("%s: %w", cmdLine(st.args...), err)
		}
	}
	if len(later) > 0 {
		fmt.Fprintln(out, "\nStore the missing secrets (there is no terminal to ask on):")
		for _, l := range later {
			fmt.Fprintf(out, "  %s\n", l)
		}
	}
	fmt.Fprintln(out, "\nStart a sandbox:")
	cmdWriter{out}.line(s.sandboxArgs(v.Name)...)
	return nil
}

// step is one brig command of brig agent init --vm.
type step struct {
	args []string
	note string
	// tolerate lets the setup go on when the step fails.
	tolerate bool
	// needsVM says that the gateway must answer.
	needsVM bool
}

// missingSecrets returns the agent's providers whose secrets are not stored
// yet.
func (s agentSetup) missingSecrets(ctx context.Context) []string {
	dirs := slices.Clone(s.vm.OpenShellConfigs)
	for _, d := range []string{s.roots.Shared, s.roots.Secrets} {
		if !slices.Contains(dirs, d) {
			dirs = append(dirs, d)
		}
	}
	set, err := ocsync.Load(dirs)
	if err != nil {
		return slices.Clone(s.plan.Definition.Providers)
	}
	var mine ocsync.WorkspaceSet
	for _, p := range set.Workspaces[openshell.DefaultWorkspace].Providers {
		if slices.Contains(s.plan.Definition.Providers, p.Name) {
			mine.Providers = append(mine.Providers, p)
		}
	}
	var missing []string
	for _, c := range checkSecrets(ctx, mine, ocsync.SecretTool{Path: s.app.cfg.OpenShell.SecretTool}) {
		if c.Status != "stored" && !slices.Contains(missing, c.Provider) {
			missing = append(missing, c.Provider)
		}
	}
	return missing
}

// gatewayUp reports whether the gateway of v answers.
func (a *app) gatewayUp(ctx context.Context, v *vm.VM) bool {
	cli, err := openshell.Find()
	if err != nil {
		return false
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	_, err = cli.GatewayVersion(ctx, v.GatewayName())
	return err == nil
}

func newAgentBuildCmd() *cobra.Command {
	var (
		workspace string
		tag       string
		noPull    bool
	)
	cmd := &cobra.Command{
		Use:   "build [VM] NAME [--tag TAG] [--no-pull]",
		Short: "Rebuild an agent's image, copy it into a VM and point its template at it",
		Long: `Build the image of the agent that agents/NAME/agent.yaml in one of the VM's
config directories describes, with Podman on the host: podman build --pull
-t IMAGE:TAG. TAG is today's date, with -2, -3 and so on if that tag exists
already; --tag sets it. --no-pull keeps the base image as it is.

It then copies the image into the VM, unless the VM has this exact image
already, and makes the template that agent.yaml names use it, in the
workspace of -w. OpenShell can only create and delete templates, so brig
deletes and creates it again. That is safe: a sandbox copies its template
when it is created, so existing sandboxes keep their image. Move them, with
their /sandbox, when it suits you: brig agent update.

Without VM, the one that brig use selected is used.`,
		Example: `  brig agent build dev claude
  brig agent build dev pi --tag 2026-10-13-test --no-pull`,
		Args:              cobra.RangeArgs(1, 2),
		ValidArgsFunction: completeAgentBuild,
		RunE: func(cmd *cobra.Command, args []string) error {
			ws, err := chosenWorkspace(workspace)
			if err != nil {
				return err
			}
			a, err := newApp()
			if err != nil {
				return err
			}
			name := args[len(args)-1]
			if err := agent.CheckName(name); err != nil {
				return err
			}
			v, err := a.resolveVMArg(cmd, args[:len(args)-1])
			if err != nil {
				return err
			}
			return a.agentBuild(cmd, v, ws, name, tag, noPull)
		},
	}
	addWorkspaceFlag(cmd, &workspace)
	f := cmd.Flags()
	f.StringVar(&tag, "tag", "", "the image tag (default: today's date)")
	f.BoolVar(&noPull, "no-pull", false, "do not pull the base image first")
	// podman build draws on the terminal.
	cmd.Annotations = map[string]string{noProgress: "true"}
	return cmd
}

// completeAgentBuild completes [VM] NAME with the agents of the VM.
func completeAgentBuild(_ *cobra.Command, args []string, _ string) ([]string, cobra.ShellCompDirective) {
	const noFiles = cobra.ShellCompDirectiveNoFileComp
	if len(args) > 1 {
		return nil, noFiles
	}
	var words []string
	if len(args) == 0 {
		words, _ = completeVMs(nil, nil, "")
	}
	if v := completionVM(args); v != nil {
		for _, dir := range v.OpenShellConfigs {
			entries, _ := os.ReadDir(filepath.Join(dir, "agents"))
			for _, e := range entries {
				if e.IsDir() {
					words = append(words, e.Name())
				}
			}
		}
	}
	return words, noFiles
}

// findAgent returns the definition of the agent called name in the config
// directories of v.
func findAgent(v *vm.VM, name string) (agent.Definition, string, error) {
	var found []string
	for _, dir := range v.OpenShellConfigs {
		path := filepath.Join(dir, "agents", name)
		if _, err := os.Stat(filepath.Join(path, "agent.yaml")); err == nil {
			found = append(found, path)
		}
	}
	switch len(found) {
	case 0:
		return agent.Definition{}, "", fmt.Errorf("no agents/%s/agent.yaml in the config directories of VM %s; create it with brig agent init", name, v.Name)
	case 1:
	default:
		return agent.Definition{}, "", fmt.Errorf("agent %s is defined in %s", name, strings.Join(found, " and "))
	}
	data, err := os.ReadFile(filepath.Join(found[0], "agent.yaml"))
	if err != nil {
		return agent.Definition{}, "", err
	}
	def, err := agent.ParseDefinition(data)
	if err != nil {
		return agent.Definition{}, "", fmt.Errorf("%s: %w", filepath.Join(found[0], "agent.yaml"), err)
	}
	if _, err := os.Stat(filepath.Join(found[0], "Containerfile")); err != nil {
		return agent.Definition{}, "", fmt.Errorf("agent %s has no Containerfile: %w", name, err)
	}
	return def, found[0], nil
}

// podmanImageExists reports whether the host's Podman has the image ref.
func podmanImageExists(ctx context.Context, ref string) bool {
	return exec.CommandContext(ctx, "podman", "image", "exists", ref).Run() == nil
}

// nextTag returns the tag for a new image of repository: today's date, and
// -2, -3 and so on while the host has an image of that tag.
func nextTag(ctx context.Context, repository string, now time.Time) string {
	base := now.Format("2006-01-02")
	tag := base
	for n := 2; podmanImageExists(ctx, repository+":"+tag); n++ {
		tag = fmt.Sprintf("%s-%d", base, n)
	}
	return tag
}

func (a *app) agentBuild(cmd *cobra.Command, v *vm.VM, ws, name, tag string, noPull bool) error {
	ctx := cmd.Context()
	def, dir, err := findAgent(v, name)
	if err != nil {
		return err
	}
	// Do not build for a VM that cannot take the image.
	if !a.gatewayUp(ctx, v) {
		return notReadyf("the gateway of VM %s does not answer; is the VM running (brig start %[1]s)?", v.Name)
	}
	if tag == "" {
		tag = nextTag(ctx, def.Image, time.Now())
	}
	ref := def.Image + ":" + tag
	out, errw := cmd.OutOrStdout(), cmd.ErrOrStderr()

	args := []string{"build"}
	if !noPull {
		args = append(args, "--pull")
	}
	args = append(args, "-t", ref, dir)
	fmt.Fprintf(errw, "$ podman %s\n", strings.Join(args, " "))
	build := exec.CommandContext(ctx, "podman", args...)
	build.Stdin, build.Stdout, build.Stderr = cmd.InOrStdin(), errw, errw
	if err := build.Run(); err != nil {
		return fmt.Errorf("podman build %s: %w", ref, err)
	}

	hostID, _ := imageID(exec.CommandContext(ctx, "podman", "image", "inspect", "--format", "{{.Id}}", ref))
	vmID, _ := imageID(a.sshTarget(v, false).Command(ctx, "podman", "image", "inspect", "--format", "{{.Id}}", ref))
	if hostID != "" && hostID == vmID {
		fmt.Fprintf(errw, "VM %s has %s already.\n", v.Name, ref)
	} else {
		fmt.Fprintf(errw, "$ brig image push %s %s\n", v.Name, ref)
		if err := a.pushImage(ctx, v, ref, cmd, errw); err != nil {
			return err
		}
	}

	if def.Template != "" {
		cli, err := openshell.Find()
		if err != nil {
			return err
		}
		// The image to go back to if the template cannot be created again.
		old, err := templateImage(ctx, v, ws, def.Template)
		if err != nil {
			fmt.Fprintf(errw, "Warning: cannot read template %s, so a failed update leaves no template: %s\n", def.Template, termtext.Escape(err.Error()))
		}
		fmt.Fprintf(errw, "$ openshell sandbox template delete %s; openshell sandbox template create %[1]s --image %s\n", def.Template, ref)
		if err := cli.ReplaceTemplate(ctx, v.GatewayName(), ws, def.Template, ref, old); errors.Is(err, openshell.ErrTemplateDeleted) {
			return fmt.Errorf("the image is in VM %s as %s, but template %s was deleted and not created again; create it with openshell %s sandbox template create %[3]s --image %[2]s: %[5]w",
				v.Name, ref, def.Template, strings.Join(openshell.WorkspaceArgs(v.GatewayName(), ws), " "), err)
		} else if err != nil {
			return fmt.Errorf("the image is in VM %s as %s, but its template %s was not updated: %w", v.Name, ref, def.Template, err)
		}
		fmt.Fprintf(out, "Built %s and pointed template %s at it.\n", ref, def.Template)
		fmt.Fprintf(errw, "Existing sandboxes keep their image; move them with brig agent update %s %s.\n", v.Name, name)
		return nil
	}
	fmt.Fprintf(out, "Built %s and copied it into VM %s.\n", ref, v.Name)
	return nil
}

// templateImage returns the image of the template called name in workspace
// ws of v's gateway, or "" if there is no such template; tests replace it.
var templateImage = func(ctx context.Context, v *vm.VM, ws, name string) (string, error) {
	c, err := dialGateway(ctx, v)
	if err != nil {
		return "", err
	}
	defer func() { _ = c.Close() }()
	list, err := c.Templates(ctx)
	if err != nil {
		return "", err
	}
	for _, t := range list {
		if t.Workspace == ws && t.Name == name {
			return t.Image, nil
		}
	}
	return "", nil
}

// imageID runs c, which prints an image ID, and returns the ID.
func imageID(c *exec.Cmd) (string, error) {
	out, err := c.Output()
	return strings.TrimSpace(string(out)), err
}
