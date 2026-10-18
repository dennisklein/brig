// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"cmp"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/GSI-HPC/go-clikit/termtext"
	"github.com/spf13/cobra"

	"github.com/dennisklein/brig/internal/osclient"
	"github.com/dennisklein/brig/internal/ui/action"
	"github.com/dennisklein/brig/internal/ui/store"
	"github.com/dennisklein/brig/internal/vm"
)

const (
	// runLabel marks the sandboxes of brig sandbox run with the run's ID.
	runLabel = "brig.dev/run"
	// expiresLabel holds the Unix time after which brig sandbox clean
	// --runs deletes a run's sandbox.
	expiresLabel = "brig.dev/expires"
	// sandboxHome is where a sandbox keeps its files, and where the paths
	// of --upload and --download start.
	sandboxHome = "/sandbox"
)

var (
	// runLife is how long a run without --timeout counts as live.
	runLife = 24 * time.Hour
	// runGrace is how long past its --timeout a run counts as live, for
	// the sandbox to start, upload and download.
	runGrace = time.Hour
	// cleanupTimeout bounds the deletion of a run's sandbox, which goes on
	// after Ctrl-C and waits for its container to stop.
	cleanupTimeout = 3 * time.Minute
	// denialSettle is how long a run waits after the command, for the log
	// lines of its last requests.
	denialSettle = time.Second
	// createSettle is how long the cleanup of an interrupted run waits for
	// its sandbox to appear: the gateway may store it after openshell
	// sandbox create was killed.
	createSettle = 10 * time.Second
)

// runGateway is what brig sandbox run and clean do through a gateway's gRPC
// API; tests replace dialRunGateway.
type runGateway interface {
	Exec(ctx context.Context, r osclient.ExecRequest, stdout, stderr io.Writer) (int, error)
	DeleteSandbox(ctx context.Context, workspace, name string) error
	Watch(ctx context.Context, workspace, sandbox string, from osclient.WatchFrom, each func(osclient.WatchEvent)) error
	Close() error
}

var dialRunGateway = func(ctx context.Context, v *vm.VM) (runGateway, error) {
	return dialGateway(ctx, v)
}

// runOpenShell runs the openshell command line argv with its output on w,
// leaving out the OPENSHELL_SANDBOX_POLICY of brig's environment, since the
// command line names the policy; tests replace it.
var runOpenShell = func(ctx context.Context, argv []string, w io.Writer) error {
	c := exec.CommandContext(ctx, argv[0], argv[1:]...)
	c.Stdout, c.Stderr = w, w
	c.Env = slices.DeleteFunc(os.Environ(), func(e string) bool { return strings.HasPrefix(e, "OPENSHELL_SANDBOX_POLICY=") })
	if err := c.Run(); err != nil {
		return fmt.Errorf("%s: %w", strings.Join(argv[:min(len(argv), 8)], " "), err)
	}
	return nil
}

// ensureVMImage copies the image ref from the host's Podman into v unless
// v has the same image already, by ID; an image the host lacks is left for
// the gateway to pull. Tests replace it.
var ensureVMImage = func(ctx context.Context, a *app, v *vm.VM, ref string, cmd *cobra.Command) error {
	inspect := []string{"podman", "image", "inspect", "--format", "{{.Id}}", ref}
	host, err := exec.CommandContext(ctx, inspect[0], inspect[1:]...).Output()
	if err != nil {
		return nil
	}
	guest, err := a.sshTarget(v, false).Command(ctx, inspect...).Output()
	if err == nil && strings.TrimSpace(string(guest)) == strings.TrimSpace(string(host)) {
		return nil
	}
	fmt.Fprintf(cmd.ErrOrStderr(), "Copying image %s into VM %s.\n", termtext.Escape(ref), v.Name)
	return a.pushImage(ctx, v, ref, cmd, cmd.ErrOrStderr())
}

// transfer is an --upload or --download: Src is copied to Dest.
type transfer struct {
	Src, Dest string
	// abs is an upload's Src made absolute: openshell names what it
	// uploads after its source's base name, which "." lacks.
	abs string
}

// parseUpload reads --upload SRC[:DIR]: SRC is a host file or directory,
// DIR the sandbox directory, relative to /sandbox, that gets it. Dest is
// where SRC lands, relative to /sandbox.
func parseUpload(arg string) (transfer, error) {
	src, dir, _ := strings.Cut(arg, ":")
	if src == "" {
		return transfer{}, fmt.Errorf("invalid upload %q: want SRC[:DIR]", arg)
	}
	if _, err := os.Stat(src); err != nil {
		return transfer{}, fmt.Errorf("invalid upload %q: %w", arg, err)
	}
	if err := checkSandboxPath(dir, true); err != nil {
		return transfer{}, fmt.Errorf("invalid upload %q: %w", arg, err)
	}
	abs, err := filepath.Abs(src)
	if err != nil {
		return transfer{}, err
	}
	return transfer{Src: src, Dest: path.Join(dir, filepath.Base(abs)), abs: abs}, nil
}

// parseDownload reads --download PATH[:DEST]: PATH is relative to
// /sandbox, DEST a host path, by default PATH's base name.
func parseDownload(arg string) (transfer, error) {
	src, dest, _ := strings.Cut(arg, ":")
	if err := checkSandboxPath(src, false); err != nil {
		return transfer{}, fmt.Errorf("invalid download %q: %w", arg, err)
	}
	if dest == "" {
		dest = path.Base(src)
	}
	return transfer{Src: src, Dest: dest}, nil
}

// checkSandboxPath accepts a clean path relative to /sandbox that stays in
// it, and the empty path for /sandbox itself if empty is set.
func checkSandboxPath(p string, empty bool) error {
	switch {
	case p == "" && empty:
		return nil
	case p == "" || path.IsAbs(p) || path.Clean(p) != p || p == "." || p == ".." || strings.HasPrefix(p, "../"):
		return fmt.Errorf("sandbox path %q must be a clean path relative to %s, inside it", p, sandboxHome)
	}
	return nil
}

// runPlan is what a brig sandbox run does.
type runPlan struct {
	target sandboxTarget
	// id is the run's ID, which the sandbox's runLabel holds.
	id, name           string
	image, template    string
	policy             string
	providers          []string
	env                []string
	uploads, downloads []transfer
	uploadAll          bool
	workdir            string
	timeout            time.Duration
	command            []string
	// life is how long the sandbox counts as live after its create; it
	// expires then.
	life    time.Duration
	expires time.Time
}

func (p runPlan) createArgv() []string {
	args := []string{"sandbox", "create", "--name", p.name, "--detach", "--no-tty", "--no-auto-providers"}
	for _, pr := range p.providers {
		args = append(args, "--provider", pr)
	}
	args = append(args, "--label", runLabel+"="+p.id, "--label", expiresLabel+"="+strconv.FormatInt(p.expires.Unix(), 10))
	if p.template != "" {
		args = append(args, "--template", p.template)
	} else {
		args = append(args, "--from", p.image)
	}
	if p.policy != "" {
		args = append(args, "--policy", p.policy)
	}
	return p.target.openshell(args...)
}

func (p runPlan) uploadArgv(u transfer) []string {
	// A trailing slash makes DIR the directory that gets SRC, even a
	// single file.
	args := []string{"sandbox", "upload", p.name, u.abs, path.Join(sandboxHome, path.Dir(u.Dest)) + "/"}
	if p.uploadAll {
		args = append(args, "--no-git-ignore")
	}
	return p.target.openshell(args...)
}

func (p runPlan) execArgv() []string {
	args := []string{"sandbox", "exec", "--name", p.name, "--no-tty"}
	if p.workdir != "" {
		args = append(args, "--workdir", p.workdir)
	}
	if p.timeout > 0 {
		args = append(args, "--timeout", strconv.Itoa(int(math.Ceil(p.timeout.Seconds()))))
	}
	for _, e := range p.env {
		args = append(args, "--env", e)
	}
	return p.target.openshell(append(append(args, "--"), p.command...)...)
}

func (p runPlan) downloadArgv(d transfer) []string {
	return p.target.openshell("sandbox", "download", p.name, path.Join(sandboxHome, d.Src), d.Dest)
}

func (p runPlan) envMap() map[string]string {
	m := make(map[string]string, len(p.env))
	for _, e := range p.env {
		k, v, _ := strings.Cut(e, "=")
		m[k] = v
	}
	return m
}

// maxSandboxName is the longest sandbox name that the gateway takes: a
// DNS-1123 label of at most 19 characters.
const maxSandboxName = 19

// sandboxNameRE matches a DNS-1123 label.
var sandboxNameRE = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?$`)

// newRunID returns a run's ID: the month, day and time in UTC and four
// random characters, short enough that run-ID fits maxSandboxName.
func newRunID(now time.Time) string {
	const chars = "0123456789abcdefghijklmnopqrstuvwxyz"
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	for i := range b {
		b[i] = chars[int(b[i])%len(chars)]
	}
	return now.UTC().Format("0102-1504") + "-" + string(b)
}

// runResult is the record that brig sandbox run writes to --result.
type runResult struct {
	VM        string `json:"vm"`
	Workspace string `json:"workspace"`
	Sandbox   string `json:"sandbox"`
	Run       string `json:"run"`
	Image     string `json:"image,omitempty"`
	Template  string `json:"template,omitempty"`
	// ExitCode is the command's, null if it did not run to the end.
	ExitCode  *int        `json:"exit_code"`
	TimedOut  bool        `json:"timed_out"`
	Started   time.Time   `json:"started"`
	Duration  float64     `json:"duration_s"`
	Kept      bool        `json:"kept"`
	Uploads   []runUpload `json:"uploads"`
	Downloads []runDown   `json:"downloads"`
	Denied    []runDenial `json:"denied"`
	// Error is why brig failed, if it did.
	Error string `json:"error,omitempty"`
}

type runUpload struct {
	Src  string `json:"src"`
	Dest string `json:"dest"`
}

type runDown struct {
	Path string `json:"path"`
	Dest string `json:"dest"`
	// Bytes is the size of the files at Dest after the download.
	Bytes int64 `json:"bytes"`
}

// runDenial counts the requests that the policy denied to one binary for
// one host and port.
type runDenial struct {
	Host   string `json:"host"`
	Port   int    `json:"port,omitempty"`
	Binary string `json:"binary,omitempty"`
	Count  int    `json:"count"`
}

// denials collects the denied requests of a sandbox's log.
type denials struct {
	mu   sync.Mutex
	seen map[runDenial]int
}

func (d *denials) add(ev osclient.WatchEvent) {
	if ev.Line == nil {
		return
	}
	r, ok := osclient.ParseRequest(ev.Line.Message)
	if !ok || !r.Denied() || r.Host == "" {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.seen == nil {
		d.seen = map[runDenial]int{}
	}
	d.seen[runDenial{Host: r.Host, Port: r.Port, Binary: r.Process}]++
}

// list returns the denials, the most frequent first.
func (d *denials) list() []runDenial {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := []runDenial{}
	for k, n := range d.seen {
		k.Count = n
		out = append(out, k)
	}
	slices.SortFunc(out, func(a, b runDenial) int {
		return cmp.Or(b.Count-a.Count, strings.Compare(a.Host, b.Host), a.Port-b.Port, strings.Compare(a.Binary, b.Binary))
	})
	return out
}

// signalStatus returns the exit status for the signal that cancelled ctx,
// 128 plus its number as shells have it, or 0.
func signalStatus(ctx context.Context) int {
	if ctx.Err() == nil {
		return 0
	}
	cause := context.Cause(ctx)
	for _, s := range []syscall.Signal{syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP} {
		if cause != nil && cause.Error() == s.String()+" signal received" {
			return 128 + int(s)
		}
	}
	return 0
}

func newSandboxRunCmd() *cobra.Command {
	var (
		image, template, name, policy, workdir, result, keep, workspace string
		providers, env, uploads, downloads                              []string
		uploadAll, start, dryRun, stdin                                 bool
		timeout                                                         time.Duration
	)
	cmd := &cobra.Command{
		Use:   "run [VM] (--image IMG | --template T) [flags] -- COMMAND [ARG...]",
		Short: "Run a command in a new sandbox, with files in and out, and delete it",
		Long: `Run a command in a sandbox of its own: create the sandbox, copy files into
it, run the command, copy files out and delete the sandbox. Without VM, the
VM is the one that brig use selected.

The sandbox is called run-<time>-<random>, or --name. It has the providers
that --provider names and no others, and the policy --policy, or else the
VM's default sandbox policy. With --image, brig first copies the image from
the host's Podman into the VM if the VM lacks it.

--upload SRC[:DIR] copies the host file or directory SRC into the sandbox
directory DIR, which is relative to /sandbox and is /sandbox itself by
default. As openshell does, it leaves out what .gitignore names, unless
--upload-all. --download PATH[:DEST] copies the sandbox's PATH, relative to
/sandbox, to the host's DEST, by default PATH's base name in the current
directory. Downloads happen also when the command fails.

The command's standard output and error are brig's, and brig exits with the
command's exit status, 124 if --timeout stopped it. brig's own failures exit
with the statuses that brig --help lists, and Ctrl-C deletes the sandbox and
exits with 130 (143 for SIGTERM). With --stdin, the command gets brig's
standard input, up to 960 KiB; otherwise it gets none.

--keep failed keeps the sandbox when the command or brig fails, --keep always
keeps it anyway. A kept sandbox carries the labels brig.dev/run and
brig.dev/expires, and brig sandbox clean --runs deletes it once it expires:
a day after brig created it, or with --timeout, an hour plus the timeout
after that.

--result FILE writes a JSON record of the run: its sandbox, exit status,
transfers and the requests that the policy denied, by host and binary.
--dry-run prints the openshell commands that the run amounts to.`,
		Example: `  brig sandbox run dev --image localhost/pi-agent:2026-10-08 --upload ./task \
    --download task/out --result run.json -- make -C task
  brig sandbox run --template claude --provider claude-code --timeout 30m \
    --keep failed --upload ./repo --workdir repo -- claude -p "fix the tests"`,
		Args: func(cmd *cobra.Command, args []string) error {
			if n := len(vmArgs(cmd, args)); n > 1 {
				return fmt.Errorf("accepts at most one VM name before --, received %d", n)
			}
			if len(args) == len(vmArgs(cmd, args)) {
				return errors.New("give the command after --")
			}
			return nil
		},
		ValidArgsFunction: completeVMs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if (image == "") == (template == "") {
				return errors.New("give --image or --template")
			}
			if !slices.Contains([]string{"never", "failed", "always"}, keep) {
				return fmt.Errorf("invalid --keep %q: want never, failed or always", keep)
			}
			if name != "" && (len(name) > maxSandboxName || !sandboxNameRE.MatchString(name)) {
				return fmt.Errorf("invalid sandbox name %q: want at most %d lowercase letters, digits and hyphens, starting and ending with a letter or digit", name, maxSandboxName)
			}
			if timeout < 0 {
				return errors.New("--timeout must not be negative")
			}
			for _, e := range env {
				if k, _, ok := strings.Cut(e, "="); !ok || k == "" {
					return fmt.Errorf("invalid --env %q: want KEY=VALUE", e)
				}
			}
			ws, err := chosenWorkspace(workspace)
			if err != nil {
				return err
			}
			now := time.Now()
			p := runPlan{
				id: newRunID(now), image: image, template: template, policy: policy, providers: providers,
				env: env, uploadAll: uploadAll, workdir: workdir, timeout: timeout,
				command: args[len(vmArgs(cmd, args)):],
			}
			p.name = cmp.Or(name, "run-"+p.id)
			p.life = runLife
			if timeout > 0 {
				p.life = timeout + runGrace
			}
			p.expires = now.Add(p.life)
			for _, arg := range uploads {
				u, err := parseUpload(arg)
				if err != nil {
					return err
				}
				p.uploads = append(p.uploads, u)
			}
			for _, arg := range downloads {
				d, err := parseDownload(arg)
				if err != nil {
					return err
				}
				p.downloads = append(p.downloads, d)
			}
			a, err := newApp()
			if err != nil {
				return err
			}
			v, err := a.resolveVMArg(cmd, vmArgs(cmd, args))
			if err != nil {
				return err
			}
			p.target = sandboxTarget{vm: v, workspace: ws}
			if p.policy == "" {
				if p.policy, err = defaultPolicy(v); err != nil {
					return err
				}
			}
			if dryRun {
				return printRunPlan(cmd.OutOrStdout(), p)
			}
			var in []byte
			if stdin {
				if in, err = io.ReadAll(io.LimitReader(cmd.InOrStdin(), osclient.MaxStdin+1)); err != nil {
					return fmt.Errorf("reading standard input: %w", err)
				}
				if len(in) > osclient.MaxStdin {
					return fmt.Errorf("standard input is more than the %d bytes that a command can get", osclient.MaxStdin)
				}
			}
			return a.sandboxRun(cmd, p, keep, start, in, result)
		},
	}
	addWorkspaceFlag(cmd, &workspace)
	f := cmd.Flags()
	f.StringVar(&image, "image", "", "create the sandbox from this container image")
	f.StringVar(&template, "template", "", "create the sandbox from this sandbox template of the gateway")
	f.StringVar(&name, "name", "", "name the sandbox (default: run-<time>-<random>)")
	f.StringArrayVar(&providers, "provider", nil, "attach this provider (repeatable)")
	f.StringVar(&policy, "policy", "", "the sandbox's policy file (default: the VM's default sandbox policy)")
	f.StringArrayVar(&env, "env", nil, "set a non-secret environment variable for the command: KEY=VALUE (repeatable); attach a provider for secrets")
	f.StringArrayVar(&uploads, "upload", nil, "copy host file or directory SRC into sandbox directory DIR, relative to /sandbox, before the command: SRC[:DIR] (repeatable)")
	f.BoolVar(&uploadAll, "upload-all", false, "upload what .gitignore names too")
	f.StringArrayVar(&downloads, "download", nil, "copy the sandbox's PATH, relative to /sandbox, to the host's DEST after the command: PATH[:DEST] (repeatable)")
	f.StringVar(&workdir, "workdir", "", "run the command in this directory of the sandbox")
	f.DurationVar(&timeout, "timeout", 0, "stop the command after this long, e.g. 30m, and exit with 124 (default: no limit)")
	f.StringVar(&keep, "keep", "never", "keep the sandbox: never, failed or always")
	f.StringVar(&result, "result", "", "write a JSON record of the run to this file")
	f.BoolVar(&start, "start", false, "start the VM if it is stopped")
	f.BoolVarP(&stdin, "stdin", "i", false, "pass standard input to the command, up to 960 KiB")
	f.BoolVar(&dryRun, "dry-run", false, "print the openshell commands that the run amounts to, and do nothing")
	_ = cmd.RegisterFlagCompletionFunc("keep", cobra.FixedCompletions([]string{"never", "failed", "always"}, cobra.ShellCompDirectiveNoFileComp))
	_ = cmd.RegisterFlagCompletionFunc("image", func(c *cobra.Command, _ []string, _ string) ([]string, cobra.ShellCompDirective) {
		return completePodmanImages(c.Context())
	})
	cmd.MarkFlagsMutuallyExclusive("image", "template")
	// Standard output is the command's alone.
	cmd.Annotations = map[string]string{noProgress: "true"}
	return cmd
}

// printRunPlan prints the commands that p amounts to.
func printRunPlan(w io.Writer, p runPlan) error {
	lines := [][]string{}
	if p.image != "" {
		lines = append(lines, []string{"brig", "image", "push", p.target.vm.Name, p.image})
	}
	lines = append(lines, p.createArgv())
	for _, u := range p.uploads {
		lines = append(lines, p.uploadArgv(u))
	}
	lines = append(lines, p.execArgv())
	for _, d := range p.downloads {
		lines = append(lines, p.downloadArgv(d))
	}
	lines = append(lines, p.target.openshell("sandbox", "delete", p.name))
	for _, l := range lines {
		if _, err := fmt.Fprintln(w, action.Quote(l)); err != nil {
			return err
		}
	}
	return nil
}

// sandboxRun carries out p, keeping its sandbox as keep says, and writes
// the record of the run to result unless empty.
func (a *app) sandboxRun(cmd *cobra.Command, p runPlan, keep string, start bool, stdin []byte, result string) error {
	ctx := cmd.Context()
	v := p.target.vm
	if start {
		if err := startIfStopped(ctx, a, v, cmd.ErrOrStderr()); err != nil {
			return err
		}
	}
	gw, err := dialRunGateway(ctx, v)
	if err != nil {
		return err
	}
	defer func() { _ = gw.Close() }()
	if _, err := findSandbox(ctx, v, p.target.workspace, p.name); err == nil {
		return alreadyExists(fmt.Errorf("VM %s has a sandbox %s in workspace %s already", v.Name, p.name, p.target.workspace))
	} else if exitStatus(err) != exitNotFound {
		return err
	}
	if p.image != "" {
		if err := ensureVMImage(ctx, a, v, p.image, cmd); err != nil {
			return err
		}
	}
	// brig sandbox clean --runs deletes the sandbox once it expires, so its
	// life starts after the image copy, which can take long.
	p.expires = time.Now().Add(p.life)

	r := &runResult{
		VM: v.Name, Workspace: p.target.workspace, Sandbox: p.name, Run: p.id, Image: p.image, Template: p.template,
		Started: time.Now().UTC(), Uploads: []runUpload{}, Downloads: []runDown{}, Denied: []runDenial{},
	}
	runErr := runSteps(ctx, cmd, gw, p, stdin, r)

	// Clean up also after Ctrl-C.
	cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cleanupTimeout)
	defer cancel()
	interrupted := ctx.Err() != nil
	failed := runErr != nil || r.ExitCode == nil || *r.ExitCode != 0
	r.Kept = keep == "always" || keep == "failed" && failed
	stderr := cmd.ErrOrStderr()
	var delErr error
	if !r.Kept {
		// The gateway answers once the sandbox's container is gone, which
		// can take a minute.
		fmt.Fprintf(stderr, "Deleting sandbox %s...\n", p.name)
		if delErr = deleteRun(cctx, gw, p, interrupted); delErr != nil {
			r.Kept = true
		}
	}
	if r.Kept {
		// A failed create may have left no sandbox to keep, or found one of
		// the same name that another command created after the check above.
		s, err := findRun(cctx, p, interrupted)
		r.Kept = exitStatus(err) != exitNotFound && (err != nil || s.Labels[runLabel] == p.id)
		if !r.Kept {
			delErr = nil
		}
	}
	switch {
	case r.Kept && delErr != nil:
		fmt.Fprintf(stderr, "Could not delete sandbox %s of VM %s (workspace %s): %s\nDelete it with brig sandbox delete %[2]s %[1]s -w %[3]s.\n",
			p.name, v.Name, p.target.workspace, termtext.Escape(delErr.Error()))
	case r.Kept:
		fmt.Fprintf(stderr, "Kept sandbox %s of VM %s (workspace %s); delete it with brig sandbox delete %[2]s %[1]s -w %[3]s.\n",
			p.name, v.Name, p.target.workspace)
	}
	r.Duration = math.Round(time.Since(r.Started).Seconds()*10) / 10
	if err := errors.Join(runErr, delErr); err != nil {
		r.Error = err.Error()
	}
	if result != "" {
		if err := writeRunResult(result, r); err != nil {
			runErr = errors.Join(runErr, err)
		}
	}
	if code := signalStatus(ctx); code != 0 {
		fmt.Fprintf(stderr, "Interrupted: %s.\n", context.Cause(ctx))
		return exitError{code}
	}
	switch {
	case runErr != nil:
		return runErr
	case *r.ExitCode != 0:
		return exitError{*r.ExitCode}
	case delErr != nil:
		// The message above says why.
		return exitError{exitFailure}
	}
	return nil
}

// runSteps creates p's sandbox and runs p in it, recording what it did in
// r. Once the sandbox is ready, it downloads whatever happened.
func runSteps(ctx context.Context, cmd *cobra.Command, gw runGateway, p runPlan, stdin []byte, r *runResult) error {
	stderr := cmd.ErrOrStderr()
	ws := p.target.workspace
	if err := runOpenShell(ctx, p.createArgv(), stderr); err != nil {
		return err
	}
	if err := waitSandboxReady(ctx, p.target.vm, ws, p.name); err != nil {
		return err
	}

	var d denials
	wctx, stopWatch := context.WithCancel(ctx)
	watched := make(chan struct{})
	go func() {
		defer close(watched)
		err := gw.Watch(wctx, ws, p.name, osclient.WatchFrom{Since: r.Started}, d.add)
		if err != nil && wctx.Err() == nil {
			fmt.Fprintf(stderr, "Warning: cannot follow the log of sandbox %s, so the record lists no denied requests: %s\n",
				p.name, termtext.Escape(err.Error()))
		}
	}()

	err := uploadAndExec(ctx, cmd, gw, p, stdin, r)
	for _, t := range p.downloads {
		if ctx.Err() != nil {
			break
		}
		if derr := runOpenShell(ctx, p.downloadArgv(t), stderr); derr != nil {
			err = errors.Join(err, derr)
			continue
		}
		r.Downloads = append(r.Downloads, runDown{Path: t.Src, Dest: t.Dest, Bytes: treeSize(t.Dest)})
	}

	select {
	case <-ctx.Done():
	case <-time.After(denialSettle):
	}
	stopWatch()
	<-watched
	r.Denied = d.list()
	return err
}

func uploadAndExec(ctx context.Context, cmd *cobra.Command, gw runGateway, p runPlan, stdin []byte, r *runResult) error {
	for _, u := range p.uploads {
		if err := runOpenShell(ctx, p.uploadArgv(u), cmd.ErrOrStderr()); err != nil {
			return err
		}
		r.Uploads = append(r.Uploads, runUpload{Src: u.Src, Dest: u.Dest})
	}
	begin := time.Now()
	code, err := gw.Exec(ctx, osclient.ExecRequest{
		Workspace: p.target.workspace, Sandbox: p.name, Command: p.command, Workdir: p.workdir,
		Env: p.envMap(), Timeout: p.timeout, Stdin: stdin,
	}, cmd.OutOrStdout(), cmd.ErrOrStderr())
	if err != nil {
		return fmt.Errorf("running the command in sandbox %s: %w", p.name, err)
	}
	r.ExitCode = &code
	r.TimedOut = p.timeout > 0 && code == 124 && time.Since(begin) >= p.timeout
	return nil
}

// deleteRun deletes p's sandbox if it is there and is the run's: a sandbox
// of the same name that another command created stays. With settle, it
// waits for the sandbox as findRun does.
func deleteRun(ctx context.Context, gw runGateway, p runPlan, settle bool) error {
	s, err := findRun(ctx, p, settle)
	switch {
	case exitStatus(err) == exitNotFound:
		return nil
	case err != nil:
		return fmt.Errorf("deleting sandbox %s: %w", p.name, err)
	case s.Labels[runLabel] != p.id:
		return fmt.Errorf("sandbox %s is not this run's, so it stays", p.name)
	}
	if err := gw.DeleteSandbox(ctx, p.target.workspace, p.name); err != nil {
		return fmt.Errorf("deleting sandbox %s: %w", p.name, err)
	}
	return nil
}

// findRun finds p's sandbox. With settle, it waits up to createSettle for
// a sandbox that is not there yet, as after an interrupted create.
func findRun(ctx context.Context, p runPlan, settle bool) (store.Sandbox, error) {
	deadline := time.Now().Add(createSettle)
	for {
		s, err := findSandbox(ctx, p.target.vm, p.target.workspace, p.name)
		if !settle || exitStatus(err) != exitNotFound || !time.Now().Before(deadline) {
			return s, err
		}
		select {
		case <-ctx.Done():
			return s, err
		case <-time.After(createSettle / 10):
		}
	}
}

// treeSize returns the size of the regular files at or under p.
func treeSize(p string) int64 {
	var n int64
	_ = filepath.WalkDir(p, func(_ string, e fs.DirEntry, err error) error {
		if err == nil && e.Type().IsRegular() {
			if fi, err := e.Info(); err == nil {
				n += fi.Size()
			}
		}
		return nil
	})
	return n
}

// writeRunResult writes r to file as JSON.
func writeRunResult(file string, r *runResult) error {
	f, err := os.Create(file)
	if err != nil {
		return fmt.Errorf("writing the run's record: %w", err)
	}
	if err := writeJSON(f, r); err != nil {
		_ = f.Close()
		return fmt.Errorf("writing the run's record: %w", err)
	}
	return f.Close()
}
