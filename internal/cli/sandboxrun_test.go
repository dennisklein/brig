// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/dennisklein/brig/internal/osclient"
	"github.com/dennisklein/brig/internal/ui/store"
	"github.com/dennisklein/brig/internal/vm"
)

// fakeRunGateway is a gateway for brig sandbox run and clean.
type fakeRunGateway struct {
	mu      sync.Mutex
	exit    int
	execs   []osclient.ExecRequest
	deleted []string
	delErr  error
	log     []string
}

func (g *fakeRunGateway) Exec(_ context.Context, r osclient.ExecRequest, stdout, stderr io.Writer) (int, error) {
	g.mu.Lock()
	g.execs = append(g.execs, r)
	g.mu.Unlock()
	_, _ = io.WriteString(stdout, "task output\n")
	_, _ = io.WriteString(stderr, "task progress\n")
	return g.exit, nil
}

func (g *fakeRunGateway) DeleteSandbox(_ context.Context, ws, name string) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.delErr != nil {
		return g.delErr
	}
	g.deleted = append(g.deleted, ws+"/"+name)
	return nil
}

func (g *fakeRunGateway) Watch(ctx context.Context, _, _ string, _ osclient.WatchFrom, each func(osclient.WatchEvent)) error {
	for _, l := range g.log {
		each(osclient.WatchEvent{Line: &osclient.LogLine{Level: "OCSF", Message: l}})
	}
	<-ctx.Done()
	return ctx.Err()
}

func (g *fakeRunGateway) Close() error { return nil }

// fakeRun replaces what brig sandbox run reaches outside: the gateway, whose
// only sandboxes are those of the given workspace that the faked openshell
// creates, openshell, which records its command lines, and the image copy.
func fakeRun(t *testing.T, g *fakeRunGateway) (ran *[][]string, pushed *[]string) {
	t.Helper()
	ran, pushed = &[][]string{}, &[]string{}
	var mu sync.Mutex
	var sandboxes []store.Sandbox
	origDial, origRun, origImage, origList, origSettle := dialRunGateway, runOpenShell, ensureVMImage, gatewaySandboxes, denialSettle
	dialRunGateway = func(context.Context, *vm.VM) (runGateway, error) { return g, nil }
	runOpenShell = func(_ context.Context, argv []string, _ io.Writer) error {
		mu.Lock()
		defer mu.Unlock()
		*ran = append(*ran, argv)
		switch {
		case slices.Contains(argv, "create"):
			s := store.Sandbox{Workspace: argv[4], Name: argv[slices.Index(argv, "--name")+1], Phase: "Ready", Labels: map[string]string{}}
			for i, arg := range argv {
				if arg == "--label" {
					k, v, _ := strings.Cut(argv[i+1], "=")
					s.Labels[k] = v
				}
			}
			sandboxes = append(sandboxes, s)
		case slices.Contains(argv, "download"):
			return os.WriteFile(argv[len(argv)-1], []byte("result"), 0o600)
		}
		return nil
	}
	ensureVMImage = func(_ context.Context, _ *app, _ *vm.VM, ref string, _ *cobra.Command) error {
		*pushed = append(*pushed, ref)
		return nil
	}
	gatewaySandboxes = func(context.Context, *vm.VM) ([]store.Sandbox, error) {
		mu.Lock()
		defer mu.Unlock()
		return slices.Clone(sandboxes), nil
	}
	denialSettle = 0
	t.Cleanup(func() {
		dialRunGateway, runOpenShell, ensureVMImage, gatewaySandboxes, denialSettle = origDial, origRun, origImage, origList, origSettle
	})
	return ran, pushed
}

func readResult(t *testing.T, file string) runResult {
	t.Helper()
	b, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	var r runResult
	if err := json.Unmarshal(b, &r); err != nil {
		t.Fatalf("%s: %v", b, err)
	}
	return r
}

func TestSandboxRun(t *testing.T) {
	a := testApp(t)
	testVM(t, a, "dev")
	dir := t.TempDir()
	t.Chdir(dir)
	if err := os.Mkdir("task", 0o750); err != nil {
		t.Fatal(err)
	}
	g := &fakeRunGateway{log: []string{
		"NET:OPEN [MED] DENIED /usr/bin/node(42) -> registry.npmjs.org:443 [reason:no rule]",
		"NET:OPEN [MED] DENIED /usr/bin/node(43) -> registry.npmjs.org:443 [reason:no rule]",
		"HTTP:GET [INFO] ALLOWED curl(88) -> GET https://api.example.com/v1 [policy:p engine:mechanistic]",
	}}
	ran, pushed := fakeRun(t, g)
	out, err := run(t, "sandbox", "run", "dev", "-w", "team", "--image", "localhost/pi:1", "--provider", "llm",
		"--upload", "task", "--upload", "task:in/put", "--download", "task/out", "--workdir", "task", "--env", "CI=1",
		"--timeout", "90s", "--result", "run.json", "--", "make", "-C", "task")
	if err != nil {
		t.Fatalf("brig sandbox run: %v: %s", err, out)
	}
	if !strings.Contains(out, "task output\n") || !strings.Contains(out, "task progress\n") {
		t.Errorf("output %q lacks the command's", out)
	}
	if !slices.Equal(*pushed, []string{"localhost/pi:1"}) {
		t.Errorf("pushed %q", *pushed)
	}

	r := readResult(t, "run.json")
	name := "run-" + r.Run
	if r.Sandbox != name || !strings.HasPrefix(r.Run, time.Now().UTC().Format("0102-")) || len(name) > maxSandboxName || len(r.Run) != len("1013-0912-k3f9") {
		t.Errorf("run %q in sandbox %q", r.Run, r.Sandbox)
	}
	gw := []string{"openshell", "-g", "brig-dev", "--workspace", "team", "sandbox"}
	expiresArg := (*ran)[0][slices.IndexFunc((*ran)[0], func(s string) bool { return strings.HasPrefix(s, expiresLabel+"=") })]
	expires, _ := strconv.ParseInt(strings.TrimPrefix(expiresArg, expiresLabel+"="), 10, 64)
	if d := time.Until(time.Unix(expires, 0)); d < time.Hour || d > time.Hour+2*time.Minute {
		t.Errorf("the sandbox expires in %v", d)
	}
	want := [][]string{
		append(slices.Clone(gw), "create", "--name", name, "--detach", "--no-tty", "--no-auto-providers", "--provider", "llm",
			"--label", runLabel+"="+r.Run, "--label", expiresArg, "--from", "localhost/pi:1"),
		append(slices.Clone(gw), "upload", name, filepath.Join(dir, "task"), "/sandbox/"),
		append(slices.Clone(gw), "upload", name, filepath.Join(dir, "task"), "/sandbox/in/put/"),
		append(slices.Clone(gw), "download", name, "/sandbox/task/out", "out"),
	}
	if len(*ran) != len(want) {
		t.Fatalf("ran %q\nwant %q", *ran, want)
	}
	for i := range want {
		if !slices.Equal((*ran)[i], want[i]) {
			t.Errorf("ran %q\nwant %q", (*ran)[i], want[i])
		}
	}
	if len(g.execs) != 1 {
		t.Fatalf("execs %+v", g.execs)
	}
	e := g.execs[0]
	if e.Workspace != "team" || e.Sandbox != name || !slices.Equal(e.Command, []string{"make", "-C", "task"}) || e.Workdir != "task" ||
		e.Env["CI"] != "1" || e.Timeout != 90*time.Second || e.Stdin != nil {
		t.Errorf("exec %+v", e)
	}
	if !slices.Equal(g.deleted, []string{"team/" + name}) {
		t.Errorf("deleted %q", g.deleted)
	}

	wantDenied := []runDenial{{Host: "registry.npmjs.org", Port: 443, Binary: "/usr/bin/node", Count: 2}}
	if r.VM != "dev" || r.Workspace != "team" || r.Image != "localhost/pi:1" || r.ExitCode == nil || *r.ExitCode != 0 || r.TimedOut || r.Kept ||
		r.Error != "" || !slices.Equal(r.Uploads, []runUpload{{"task", "task"}, {"task", "in/put/task"}}) ||
		!slices.Equal(r.Downloads, []runDown{{Path: "task/out", Dest: "out", Bytes: 6}}) || !slices.Equal(r.Denied, wantDenied) {
		t.Errorf("result %+v", r)
	}
}

func TestSandboxRunPassesTheExitStatusAndKeepsFailures(t *testing.T) {
	a := testApp(t)
	testVM(t, a, "dev")
	dir := t.TempDir()
	g := &fakeRunGateway{exit: 3}
	ran, pushed := fakeRun(t, g)
	result := filepath.Join(dir, "run.json")
	out, err := run(t, "sandbox", "run", "dev", "--template", "pi", "--name", "fix", "--keep", "failed", "--result", result, "--", "false")
	var exit exitError
	if !errors.As(err, &exit) || exit.code != 3 {
		t.Fatalf("brig sandbox run: %v, want exit status 3: %s", err, out)
	}
	if len(g.deleted) != 0 || !strings.Contains(out, "Kept sandbox fix of VM dev") {
		t.Errorf("deleted %q: %s", g.deleted, out)
	}
	if len(*pushed) != 0 || !slices.Contains((*ran)[0], "--template") || slices.Contains((*ran)[0], "--from") {
		t.Errorf("pushed %q, ran %q", *pushed, *ran)
	}
	if r := readResult(t, result); r.ExitCode == nil || *r.ExitCode != 3 || !r.Kept || r.Template != "pi" || r.Sandbox != "fix" {
		t.Errorf("result %+v", r)
	}

	// --keep never deletes a failed run's sandbox; a sandbox of that name
	// that is not a run's is not touched.
	g.deleted = nil
	if _, err := run(t, "sandbox", "run", "dev", "--template", "pi", "--name", "fix", "--", "false"); exitStatus(err) != exitExists {
		t.Errorf("a run into an existing sandbox: %v", err)
	}
	if _, err := run(t, "sandbox", "run", "dev", "--template", "pi", "--", "false"); !errors.As(err, &exit) || exit.code != 3 || len(g.deleted) != 1 {
		t.Errorf("--keep never: %v, deleted %q", err, g.deleted)
	}
}

func TestSandboxRunSaysWhyItCouldNotDelete(t *testing.T) {
	a := testApp(t)
	testVM(t, a, "dev")
	g := &fakeRunGateway{delErr: errors.New("the gateway is busy")}
	fakeRun(t, g)
	result := filepath.Join(t.TempDir(), "run.json")
	out, err := run(t, "sandbox", "run", "dev", "--template", "pi", "--name", "fix", "--result", result, "--", "true")
	var exit exitError
	if !errors.As(err, &exit) || exit.code != exitFailure {
		t.Fatalf("err %v: %s", err, out)
	}
	if !strings.Contains(out, "Could not delete sandbox fix of VM dev (workspace default): deleting sandbox fix: the gateway is busy\n"+
		"Delete it with brig sandbox delete dev fix -w default.\n") {
		t.Errorf("output %q", out)
	}
	if r := readResult(t, result); !r.Kept || r.ExitCode == nil || *r.ExitCode != 0 || !strings.Contains(r.Error, "the gateway is busy") {
		t.Errorf("result %+v", r)
	}
}

// TestSandboxRunLeavesASandboxCreatedMeanwhileAlone has another command
// create a sandbox of the run's name after the run found the name free.
func TestSandboxRunLeavesASandboxCreatedMeanwhileAlone(t *testing.T) {
	for _, keep := range []string{"never", "failed"} {
		t.Run(keep, func(t *testing.T) {
			a := testApp(t)
			testVM(t, a, "dev")
			g := &fakeRunGateway{}
			fakeRun(t, g)
			var created bool
			runOpenShell = func(context.Context, []string, io.Writer) error {
				created = true
				return errors.New("sandbox fix already exists")
			}
			gatewaySandboxes = func(context.Context, *vm.VM) ([]store.Sandbox, error) {
				if !created {
					return nil, nil
				}
				return []store.Sandbox{{Workspace: "default", Name: "fix", Phase: "Ready", Labels: map[string]string{runLabel: "other"}}}, nil
			}
			result := filepath.Join(t.TempDir(), "run.json")
			out, err := run(t, "sandbox", "run", "dev", "--template", "pi", "--name", "fix", "--keep", keep, "--result", result, "--", "true")
			if err == nil || !strings.Contains(err.Error(), "already exists") {
				t.Fatalf("err %v: %s", err, out)
			}
			if len(g.deleted) != 0 || strings.Contains(strings.ToLower(out), "delete it with") {
				t.Errorf("deleted %q: %s", g.deleted, out)
			}
			if r := readResult(t, result); r.Kept || strings.Contains(r.Error, "is not this run's") {
				t.Errorf("result %+v", r)
			}
		})
	}
}

// TestSandboxRunDeletesTheSandboxOfAnInterruptedCreate has Ctrl-C kill
// openshell sandbox create before the gateway stores the sandbox.
func TestSandboxRunDeletesTheSandboxOfAnInterruptedCreate(t *testing.T) {
	a := testApp(t)
	testVM(t, a, "dev")
	g := &fakeRunGateway{}
	fakeRun(t, g)
	origSettle := createSettle
	createSettle = 5 * time.Second
	t.Cleanup(func() { createSettle = origSettle })
	ctx, cancel := context.WithCancelCause(t.Context())
	create := runOpenShell
	runOpenShell = func(ctx context.Context, argv []string, w io.Writer) error {
		cancel(errors.New("interrupt signal received"))
		go func() {
			time.Sleep(100 * time.Millisecond)
			_ = create(ctx, argv, w)
		}()
		return context.Canceled
	}
	cmd := newRootCmd(&progressRun{})
	var out strings.Builder
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs([]string{"sandbox", "run", "dev", "--template", "pi", "--name", "fix", "--", "true"})
	var exit exitError
	if err := cmd.ExecuteContext(ctx); !errors.As(err, &exit) || exit.code != 130 {
		t.Fatalf("err %v, want exit status 130: %s", err, out.String())
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if !slices.Equal(g.deleted, []string{"default/fix"}) {
		t.Errorf("deleted %q: %s", g.deleted, out.String())
	}
}

// TestSandboxRunExpiresAfterTheImageCopy checks that a run's sandbox counts
// as live from its create, not from the start of the run.
func TestSandboxRunExpiresAfterTheImageCopy(t *testing.T) {
	a := testApp(t)
	testVM(t, a, "dev")
	ran, _ := fakeRun(t, &fakeRunGateway{})
	origGrace := runGrace
	runGrace = 0
	t.Cleanup(func() { runGrace = origGrace })
	var copied time.Time
	ensureVMImage = func(context.Context, *app, *vm.VM, string, *cobra.Command) error {
		time.Sleep(1100 * time.Millisecond)
		copied = time.Now()
		return nil
	}
	if out, err := run(t, "sandbox", "run", "dev", "--image", "img", "--timeout", "2s", "--", "true"); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	arg := (*ran)[0][slices.IndexFunc((*ran)[0], func(s string) bool { return strings.HasPrefix(s, expiresLabel+"=") })]
	expires, _ := strconv.ParseInt(strings.TrimPrefix(arg, expiresLabel+"="), 10, 64)
	if want := copied.Add(2 * time.Second).Unix(); expires < want {
		t.Errorf("the sandbox expires at %d, before %d", expires, want)
	}
}

func TestSandboxRunStdin(t *testing.T) {
	a := testApp(t)
	testVM(t, a, "dev")
	g := &fakeRunGateway{}
	fakeRun(t, g)
	cmd := newRootCmd(&progressRun{})
	cmd.SetIn(strings.NewReader("the task"))
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	cmd.SetArgs([]string{"sandbox", "run", "dev", "--image", "img", "-i", "--", "cat"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if len(g.execs) != 1 || string(g.execs[0].Stdin) != "the task" {
		t.Errorf("execs %+v", g.execs)
	}
}

func TestSandboxRunDryRun(t *testing.T) {
	a := testApp(t)
	testVM(t, a, "dev")
	dir := t.TempDir()
	t.Chdir(dir)
	if err := os.WriteFile("prompt.md", nil, 0o600); err != nil {
		t.Fatal(err)
	}
	ran, _ := fakeRun(t, &fakeRunGateway{})
	out, err := run(t, "sandbox", "run", "dev", "--dry-run", "--image", "img", "--name", "x", "--policy", "/p.yaml", "--upload", "prompt.md:in",
		"--upload-all", "--download", "out:/tmp/o", "--timeout", "90s", "--env", "A=b c", "--", "sh", "-c", "echo $A")
	if err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	gw := "openshell -g brig-dev --workspace default sandbox "
	want := []string{
		"brig image push dev img",
		gw + "create --name x --detach --no-tty --no-auto-providers --label brig.dev/run=",
		gw + "upload x " + filepath.Join(dir, "prompt.md") + " /sandbox/in/ --no-git-ignore",
		gw + "exec --name x --no-tty --timeout 90 --env 'A=b c' -- sh -c 'echo $A'",
		gw + "download x /sandbox/out /tmp/o",
		gw + "delete x",
	}
	if len(lines) != len(want) {
		t.Fatalf("dry run printed\n%s", out)
	}
	for i := range want {
		if !strings.HasPrefix(lines[i], want[i]) {
			t.Errorf("line %q\nwant %q", lines[i], want[i])
		}
	}
	if !strings.HasSuffix(lines[1], " --from img --policy /p.yaml") {
		t.Errorf("create %q", lines[1])
	}
	if len(*ran) != 0 {
		t.Errorf("a dry run ran %q", *ran)
	}
}

func TestSandboxRunArgs(t *testing.T) {
	a := testApp(t)
	testVM(t, a, "dev")
	fakeRun(t, &fakeRunGateway{})
	for _, tc := range []struct {
		args    []string
		wantErr string
	}{
		{[]string{"dev", "--image", "i"}, "give the command after --"},
		{[]string{"dev", "web", "--image", "i", "--", "true"}, "at most one VM name"},
		{[]string{"dev", "--", "true"}, "give --image or --template"},
		{[]string{"dev", "--image", "i", "--template", "t", "--", "true"}, "none of the others can be"},
		{[]string{"dev", "--image", "i", "--keep", "sometimes", "--", "true"}, "invalid --keep"},
		{[]string{"dev", "--image", "i", "--name", "-x", "--", "true"}, "invalid sandbox name"},
		{[]string{"dev", "--image", "i", "--name", "Web", "--", "true"}, "invalid sandbox name"},
		{[]string{"dev", "--image", "i", "--name", "a-name-of-twenty-chr", "--", "true"}, "invalid sandbox name"},
		{[]string{"dev", "--image", "i", "--env", "=x", "--", "true"}, "want KEY=VALUE"},
		{[]string{"dev", "--image", "i", "--upload", "/does/not/exist", "--", "true"}, "no such file"},
		{[]string{"dev", "--image", "i", "--upload", ".:/abs", "--", "true"}, "relative to /sandbox"},
		{[]string{"dev", "--image", "i", "--download", "../etc", "--", "true"}, "relative to /sandbox"},
		{[]string{"dev", "--image", "i", "--download", ":x", "--", "true"}, "relative to /sandbox"},
	} {
		if _, err := run(t, append([]string{"sandbox", "run"}, tc.args...)...); err == nil || !strings.Contains(err.Error(), tc.wantErr) {
			t.Errorf("sandbox run %q: %v, want an error containing %q", tc.args, err, tc.wantErr)
		}
	}
}

func TestSignalStatus(t *testing.T) {
	if signalStatus(t.Context()) != 0 {
		t.Error("a live context has a signal status")
	}
	for cause, want := range map[string]int{"interrupt signal received": 130, "terminated signal received": 143, "other": 0} {
		ctx, cancel := context.WithCancelCause(t.Context())
		cancel(errors.New(cause))
		if got := signalStatus(ctx); got != want {
			t.Errorf("%s: %d, want %d", cause, got, want)
		}
	}
}
