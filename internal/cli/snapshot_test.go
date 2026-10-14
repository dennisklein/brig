// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dennisklein/brig/internal/osclient"
	"github.com/dennisklein/brig/internal/ui/store"
	"github.com/dennisklein/brig/internal/vm"
)

// fakeSnapGW is a gateway and VM for snapshots and recreate. A sandbox's
// /sandbox is a string in its volume; a snapshot copies it. Each
// operation can be made to fail.
type fakeSnapGW struct {
	mu        sync.Mutex
	sandboxes map[string]*osclient.SandboxRecord
	templates []store.Template
	providers []store.Provider
	nextID    int
	// ps is what the process listing prints; psErr makes it fail.
	ps    string
	psErr error
	// volumes holds each volume's /sandbox, images the VM's images.
	volumes map[string]string
	images  map[string]bool
	fs      string
	snaps   map[string]fakeSnap
	// fail counts the calls of an operation to fail: create, delete,
	// stop, start, and the VM's snapshot and restore.
	fail map[string]int
	// restores are the arguments of the restores.
	restores [][]string
	// interrupt is an operation that cancels, as Ctrl-C does.
	interrupt string
	cancel    context.CancelFunc
	calls     []string
	created   []osclient.CreateRequest
}

type fakeSnap struct{ data, manifest string }

func newFakeSnapGW() *fakeSnapGW {
	g := &fakeSnapGW{
		sandboxes: map[string]*osclient.SandboxRecord{},
		templates: []store.Template{{Workspace: "default", Name: "claude", Image: "localhost/claude-agent:2"}},
		providers: []store.Provider{{Workspace: "default", Name: "claude-code"}},
		volumes:   map[string]string{}, images: map[string]bool{"localhost/claude-agent:1": true, "localhost/claude-agent:2": true},
		fs: "btrfs", snaps: map[string]fakeSnap{}, fail: map[string]int{}, ps: "self 50\n1 0 openshell-sandb\n7 1 bash\n50 7 sh\n",
	}
	return g
}

// add adds a sandbox of template claude on image, with /sandbox data.
func (g *fakeSnapGW) add(name, phase, image, data string) *osclient.SandboxRecord {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.nextID++
	spec, _ := json.Marshal(map[string]any{"template": map[string]any{"image": image}, "providers": []string{"claude-code"}})
	r := &osclient.SandboxRecord{
		ID: strconv.Itoa(g.nextID), Workspace: "default", Name: name, Phase: phase, Image: image, Template: "claude",
		Labels: map[string]string{"app": name}, Spec: spec,
	}
	g.sandboxes["default/"+name] = r
	g.volumes[r.Volume()] = data
	return r
}

func (g *fakeSnapGW) failing(ctx context.Context, op string) error {
	g.calls = append(g.calls, op)
	if op == g.interrupt {
		g.interrupt = ""
		g.cancel()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if g.fail[op] > 0 {
		g.fail[op]--
		return fmt.Errorf("%s failed", op)
	}
	return nil
}

func (g *fakeSnapGW) Sandbox(_ context.Context, ws, name string) (osclient.SandboxRecord, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	r, ok := g.sandboxes[ws+"/"+name]
	if !ok {
		return osclient.SandboxRecord{}, fmt.Errorf("sandbox %s: %w", name, osclient.ErrNoSandbox)
	}
	out := *r
	out.Labels = maps.Clone(r.Labels)
	return out, nil
}

func (g *fakeSnapGW) Sandboxes(context.Context) ([]store.Sandbox, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	var out []store.Sandbox
	for _, r := range g.sandboxes {
		out = append(out, store.Sandbox{Workspace: r.Workspace, Name: r.Name, Phase: r.Phase, Image: r.Image, Template: r.Template, Labels: r.Labels})
	}
	slices.SortFunc(out, func(a, b store.Sandbox) int { return strings.Compare(a.Name, b.Name) })
	return out, nil
}

func (g *fakeSnapGW) Templates(context.Context) ([]store.Template, error) { return g.templates, nil }

func (g *fakeSnapGW) Providers(context.Context) ([]store.Provider, error) { return g.providers, nil }

func (g *fakeSnapGW) BasePolicy(context.Context, string, string) (json.RawMessage, error) {
	return json.RawMessage(`{"networkPolicies":{"npm":{"name":"npm"}}}`), nil
}

func (g *fakeSnapGW) Services(context.Context, string, string) ([]osclient.Service, error) {
	return []osclient.Service{{Name: "web", Port: 8080}}, nil
}

func (g *fakeSnapGW) CreateSandbox(ctx context.Context, r osclient.CreateRequest) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if err := g.failing(ctx, "create"); err != nil {
		return err
	}
	key := r.Workspace + "/" + r.Name
	if _, ok := g.sandboxes[key]; ok {
		return errors.New("sandbox exists")
	}
	g.created = append(g.created, r)
	var spec struct {
		Template struct {
			Image string `json:"image"`
		} `json:"template"`
	}
	_ = json.Unmarshal(r.Spec, &spec)
	image, tmpl := spec.Template.Image, ""
	switch {
	case r.Template != "":
		tmpl = r.Template
		for _, t := range g.templates {
			if t.Workspace == r.Workspace && t.Name == r.Template {
				image = t.Image
			}
		}
	case r.Image != "":
		image = r.Image
	}
	g.nextID++
	rec := &osclient.SandboxRecord{
		ID: strconv.Itoa(g.nextID), Workspace: r.Workspace, Name: r.Name, Phase: "Ready", Image: image, Template: tmpl,
		Labels: maps.Clone(r.Labels), Spec: r.Spec,
	}
	g.sandboxes[key] = rec
	g.volumes[rec.Volume()] = "seed"
	return nil
}

func (g *fakeSnapGW) setPhase(ctx context.Context, op, ws, name, phase string) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if err := g.failing(ctx, op); err != nil {
		return err
	}
	r, ok := g.sandboxes[ws+"/"+name]
	if !ok {
		return osclient.ErrNoSandbox
	}
	r.Phase = phase
	return nil
}

func (g *fakeSnapGW) StopSandbox(ctx context.Context, ws, name string) error {
	return g.setPhase(ctx, "stop", ws, name, "Stopped")
}

func (g *fakeSnapGW) StartSandbox(ctx context.Context, ws, name string) error {
	return g.setPhase(ctx, "start", ws, name, "Ready")
}

func (g *fakeSnapGW) DeleteSandbox(ctx context.Context, ws, name string) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if err := g.failing(ctx, "delete"); err != nil {
		return err
	}
	if r, ok := g.sandboxes[ws+"/"+name]; ok {
		delete(g.volumes, r.Volume())
		delete(g.sandboxes, ws+"/"+name)
	}
	return nil
}

func (g *fakeSnapGW) Exec(_ context.Context, _ osclient.ExecRequest, stdout, _ io.Writer) (int, error) {
	if g.psErr != nil {
		return 0, g.psErr
	}
	_, _ = io.WriteString(stdout, g.ps)
	return 0, nil
}

func (g *fakeSnapGW) Close() error { return nil }

var (
	volumeRE = regexp.MustCompile(`podman volume inspect --format '\{\{\.Mountpoint\}\}' '([^']*)'`)
	imageRE  = regexp.MustCompile(`printf 'image %s %s\\n' '([^']*)'`)
	mountRE  = regexp.MustCompile(`podman volume exists '([^']*)'`)
)

// unshareArgs returns the arguments that a snapshot script passes to its
// inner script after $root and $mp.
func unshareArgs(script string) []string {
	_, tail, _ := strings.Cut(script, "exec podman unshare")
	words := shellWords(strings.TrimSpace(tail))
	i := slices.Index(words, "$mp")
	return words[i+1:]
}

// shellWords splits a line as a POSIX shell does, for the quoting of
// shQuote and plain double quotes.
func shellWords(line string) []string {
	var words []string
	var w strings.Builder
	in := false
	quote := byte(0)
	for i := 0; i < len(line); i++ {
		c := line[i]
		switch {
		case quote != 0 && c == quote:
			quote = 0
		case quote != 0:
			w.WriteByte(c)
		case c == '\'' || c == '"':
			quote, in = c, true
		case c == '\\' && i+1 < len(line):
			i++
			w.WriteByte(line[i])
			in = true
		case c == ' ':
			if in {
				words = append(words, w.String())
				w.Reset()
				in = false
			}
		default:
			w.WriteByte(c)
			in = true
		}
	}
	if in {
		words = append(words, w.String())
	}
	return words
}

// script plays the VM's part for the scripts of package guest.
func (g *fakeSnapGW) script(ctx context.Context, _ *app, _ *vm.VM, script string) (string, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	vol := ""
	if m := volumeRE.FindStringSubmatch(script); m != nil {
		vol = m[1]
	}
	switch {
	case strings.Contains(script, "echo 'volume yes'"):
		out := "fs " + g.fs + "\n"
		for _, m := range imageRE.FindAllStringSubmatch(script, -1) {
			if g.images[m[1]] {
				out += "image " + m[1] + " sha256:" + m[1] + " /sandbox\n"
			} else {
				out += "image " + m[1] + " - -\n"
			}
		}
		for _, m := range mountRE.FindAllStringSubmatch(script, -1) {
			out += "mount " + m[1] + " yes\n"
		}
		if data, ok := g.volumes[vol]; ok {
			out += "volume yes\n"
			if strings.Contains(script, "du -sb") {
				out += fmt.Sprintf("size %d\n", len(data))
			}
		} else {
			out += "volume no\n"
		}
		return out, nil
	case strings.Contains(script, `copy_tree "$mp" "$tmp/data"`):
		if err := g.failing(ctx, "snapshot"); err != nil {
			return "", err
		}
		args := unshareArgs(script)
		if _, ok := g.snaps[args[0]]; ok {
			return "", fmt.Errorf("brig: snapshot %s exists", args[0])
		}
		g.snaps[args[0]] = fakeSnap{g.volumes[vol], args[1]}
		return "", nil
	case strings.Contains(script, "_data.brig-new"):
		if err := g.failing(ctx, "restore"); err != nil {
			return "", err
		}
		args := unshareArgs(script)
		g.restores = append(g.restores, args)
		sn, ok := g.snaps[args[0]]
		if !ok {
			return "", errors.New("no snapshot")
		}
		if _, ok := g.volumes[vol]; !ok {
			return "", errors.New("no volume")
		}
		g.volumes[vol] = sn.data
		return "", nil
	case strings.Contains(script, "for d in */*/*"):
		var b strings.Builder
		for _, rel := range slices.Sorted(maps.Keys(g.snaps)) {
			fmt.Fprintf(&b, "snapshot %s\n%s\n", rel, g.snaps[rel].manifest)
		}
		for _, rel := range slices.Sorted(maps.Keys(g.snaps)) {
			fmt.Fprintf(&b, "%d %d - %s/data\n", 100, 10, rel)
		}
		return b.String(), nil
	case strings.Contains(script, "rmdir --"):
		for _, rel := range unshareArgs(script) {
			delete(g.snaps, rel)
		}
		return "", nil
	}
	return "", fmt.Errorf("unknown script\n%s", script)
}

// fakeSnapshots replaces the gateway and the VM for the test.
func fakeSnapshots(t *testing.T, g *fakeSnapGW) {
	t.Helper()
	origDial, origScript, origWait, origPoll, origNow := dialSandboxGateway, vmScript, phaseWait, phasePoll, snapshotNow
	dialSandboxGateway = func(context.Context, *vm.VM) (sandboxGateway, error) { return g, nil }
	vmScript = g.script
	phaseWait, phasePoll = time.Second, time.Millisecond
	clock := time.Date(2026, 10, 14, 15, 2, 11, 0, time.UTC)
	var mu sync.Mutex
	snapshotNow = func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		clock = clock.Add(time.Second)
		return clock
	}
	t.Cleanup(func() {
		dialSandboxGateway, vmScript, phaseWait, phasePoll, snapshotNow = origDial, origScript, origWait, origPoll, origNow
	})
}

func TestParseProcesses(t *testing.T) {
	out := "self 50\n1 0 openshell-sandb\n7 1 bash\n214 7 claude\n215 214 node\n50 49 sh\n49 1 sh\n51 50 sed\n300 1 my tool\n"
	procs, err := parseProcesses(out)
	if err != nil {
		t.Fatal(err)
	}
	want := []process{{214, "claude"}, {215, "node"}, {300, "my tool"}}
	if !slices.Equal(procs, want) {
		t.Errorf("processes %v", procs)
	}
	if _, err := parseProcesses("1 0 init\n"); err == nil {
		t.Error("a list without self was accepted")
	}
}

func TestPruneSnapshots(t *testing.T) {
	now := time.Date(2026, 10, 14, 0, 0, 0, 0, time.UTC)
	day := func(n int) time.Time { return now.AddDate(0, 0, -n) }
	snaps := []snapshot{
		{Workspace: "default", Sandbox: "gone", Time: day(1), Gone: true},
		{Workspace: "default", Sandbox: "webapp", Time: day(1)},
		{Workspace: "default", Sandbox: "webapp", Time: day(5)},
		{Workspace: "default", Sandbox: "webapp", Time: day(40)},
		{Workspace: "default", Sandbox: "busy", Time: day(50), Gone: true, Recreate: true},
		{Workspace: "team", Sandbox: "webapp", Time: day(60)},
	}
	names := func(s []snapshot) []string {
		var out []string
		for _, sn := range s {
			out = append(out, sn.Workspace+"/"+sn.Name())
		}
		return out
	}
	got := names(pruneSnapshots(snaps, 1, 0, now))
	if want := []string{"default/gone@2026-10-13T00:00:00Z", "default/webapp@2026-10-09T00:00:00Z", "default/webapp@2026-09-04T00:00:00Z"}; !slices.Equal(got, want) {
		t.Errorf("plain prune %q", got)
	}
	got = names(pruneSnapshots(snaps, 3, 30*24*time.Hour, now))
	if want := []string{"default/gone@2026-10-13T00:00:00Z", "default/webapp@2026-09-04T00:00:00Z", "team/webapp@2026-08-15T00:00:00Z"}; !slices.Equal(got, want) {
		t.Errorf("prune --keep 3 --older-than 30d %q", got)
	}
	if d, err := parseAge("30d"); err != nil || d != 30*24*time.Hour {
		t.Errorf("30d: %v, %v", d, err)
	}
	for _, bad := range []string{"", "d", "-1d", "0h", "soon"} {
		if _, err := parseAge(bad); err == nil {
			t.Errorf("%q was accepted", bad)
		}
	}
}

func TestSnapshotCommands(t *testing.T) {
	g := newFakeSnapGW()
	w := g.add("webapp", "Ready", "localhost/claude-agent:1", "work")
	testApp(t)
	fakeSnapshots(t, g)
	a, _ := newApp()
	testVM(t, a, "dev")
	out, err := run(t, "sandbox", "snapshot", "create", "dev", "webapp")
	if err != nil || out != "Took snapshot webapp@2026-10-14T15:02:12Z of sandbox webapp.\n" {
		t.Fatalf("create: %v: %q", err, out)
	}
	if r, _ := g.Sandbox(context.Background(), "default", "webapp"); r.Phase != "Ready" {
		t.Errorf("the sandbox is %s after the snapshot", r.Phase)
	}
	g.volumes[w.Volume()] = "later"
	fork := g.add("fork", "Stopped", "localhost/claude-agent:1", "seed")

	out, err = run(t, "sandbox", "snapshot", "list", "dev")
	if err != nil || !strings.Contains(out, "webapp@2026-10-14T15:02:12Z  default    localhost/claude-agent:1  100B  10B") {
		t.Errorf("list: %v:\n%s", err, out)
	}
	out, err = run(t, "sandbox", "snapshot", "restore", "dev", "webapp@2026-10-14T15:02:12Z", "fork")
	if err != nil || g.volumes[fork.Volume()] != "work" || g.sandboxes["default/fork"].Phase != "Stopped" {
		t.Errorf("restore: %v: %s", err, out)
	}
	out, err = run(t, "sandbox", "snapshot", "restore", "dev", "webapp@2026-10-14T15:02:12Z", "fork", "--reset", "../x")
	if err == nil {
		t.Errorf("restore --reset ../x: %s", out)
	}
	if _, err := run(t, "sandbox", "snapshot", "restore", "dev", "webapp@2026-10-14T15:02:13Z", "fork"); exitStatus(err) != exitNotFound {
		t.Errorf("restore of a missing snapshot: %v", err)
	}

	// The sandbox goes; plain prune removes its snapshot.
	_ = g.DeleteSandbox(context.Background(), "default", "webapp")
	out, err = run(t, "sandbox", "snapshot", "prune", "dev", "--dry-run")
	if err != nil || !strings.Contains(out, "Would delete snapshot webapp@2026-10-14T15:02:12Z (workspace default), whose sandbox is gone, 10B.") || len(g.snaps) != 1 {
		t.Errorf("prune --dry-run: %v:\n%s", err, out)
	}
	out, err = run(t, "sandbox", "snapshot", "delete", "dev", "webapp@2026-10-14T15:02:12Z")
	if err != nil || out != "Deleted snapshot webapp@2026-10-14T15:02:12Z.\n" || len(g.snaps) != 0 {
		t.Errorf("delete: %v: %s", err, out)
	}
}
