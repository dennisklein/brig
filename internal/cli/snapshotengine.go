// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"bufio"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/GSI-HPC/go-clikit/progress"
	"github.com/GSI-HPC/go-clikit/termtext"

	"github.com/dennisklein/brig/internal/guest"
	"github.com/dennisklein/brig/internal/osclient"
	"github.com/dennisklein/brig/internal/ui/store"
	"github.com/dennisklein/brig/internal/vm"
)

// sandboxGateway is what snapshots and recreate do through a gateway's
// gRPC API; tests replace dialSandboxGateway.
type sandboxGateway interface {
	Sandbox(ctx context.Context, workspace, name string) (osclient.SandboxRecord, error)
	Sandboxes(ctx context.Context) ([]store.Sandbox, error)
	Templates(ctx context.Context) ([]store.Template, error)
	Providers(ctx context.Context) ([]store.Provider, error)
	BasePolicy(ctx context.Context, workspace, sandbox string) (json.RawMessage, error)
	Services(ctx context.Context, workspace, sandbox string) ([]osclient.Service, error)
	CreateSandbox(ctx context.Context, r osclient.CreateRequest) error
	StopSandbox(ctx context.Context, workspace, name string) error
	StartSandbox(ctx context.Context, workspace, name string) error
	DeleteSandbox(ctx context.Context, workspace, name string) error
	Exec(ctx context.Context, r osclient.ExecRequest, stdout, stderr io.Writer) (int, error)
	Close() error
}

var dialSandboxGateway = func(ctx context.Context, v *vm.VM) (sandboxGateway, error) {
	return dialGateway(ctx, v)
}

// vmScript runs script in v as guest.User and returns its standard output;
// tests replace it.
var vmScript = func(ctx context.Context, a *app, v *vm.VM, script string) (string, error) {
	return a.guestRun(ctx, v, false, script)
}

// snapshotNow is the clock of snapshots; tests replace it.
var snapshotNow = time.Now

// snapshotTime is the format of a snapshot's time, in UTC, which names it.
const snapshotTime = "2006-01-02T15:04:05Z"

// snapshot is a snapshot of a sandbox's /sandbox, kept in the VM.
type snapshot struct {
	Workspace string    `json:"workspace"`
	Sandbox   string    `json:"sandbox"`
	Time      time.Time `json:"time"`
	Image     string    `json:"image,omitempty"`
	Template  string    `json:"template,omitempty"`
	// Size is what the snapshot holds, and Exclusive what it holds alone,
	// which deleting it frees; both -1 if unknown.
	Size      int64 `json:"size"`
	Exclusive int64 `json:"exclusive"`
	// Gone says that its sandbox no longer exists.
	Gone bool `json:"gone"`
	// Recreate says that an unfinished recreate needs it.
	Recreate bool `json:"recreate,omitempty"`
	manifest snapshotManifest
}

// Name returns the snapshot's name, SANDBOX@TIME.
func (s snapshot) Name() string { return s.Sandbox + "@" + s.Time.Format(snapshotTime) }

// path returns where the snapshot lives, relative to guest.SnapshotDir.
func (s snapshot) path() string {
	return s.Workspace + "/" + s.Sandbox + "/" + s.Time.Format(snapshotTime)
}

// MarshalJSON adds the name.
func (s snapshot) MarshalJSON() ([]byte, error) {
	type plain snapshot
	return json.Marshal(struct {
		Name string `json:"name"`
		plain
	}{s.Name(), plain(s)})
}

// parseSnapshotName reads SANDBOX@TIME.
func parseSnapshotName(name string) (sandbox string, t time.Time, err error) {
	sandbox, ts, ok := strings.Cut(name, "@")
	if ok && sandboxNameRE.MatchString(sandbox) {
		if t, err = time.Parse(snapshotTime, ts); err == nil {
			return sandbox, t, nil
		}
	}
	return "", time.Time{}, fmt.Errorf("invalid snapshot name %q: want SANDBOX@TIME, as brig sandbox snapshot list shows it", name)
}

// snapshotManifest is what a snapshot's manifest.json records of the
// sandbox, so that the sandbox can be created again from it.
type snapshotManifest struct {
	Sandbox osclient.SandboxRecord `json:"sandbox"`
	// ImageID is the ID of the sandbox's image in the VM, if known.
	ImageID string `json:"image_id,omitempty"`
	// Policy is the sandbox's base policy as protobuf JSON, if known.
	Policy   json.RawMessage    `json:"policy,omitempty"`
	Services []osclient.Service `json:"services"`
	Created  time.Time          `json:"created"`
}

// snapshots does what the snapshot commands and recreate share, on one
// VM.
type snapshots struct {
	a  *app
	v  *vm.VM
	gw sandboxGateway
	// errw gets progress and warnings.
	errw io.Writer
	now  func() time.Time
}

func (a *app) snapshots(ctx context.Context, v *vm.VM, errw io.Writer) (*snapshots, error) {
	gw, err := dialSandboxGateway(ctx, v)
	if err != nil {
		return nil, err
	}
	return &snapshots{a: a, v: v, gw: gw, errw: errw, now: snapshotNow}, nil
}

func (s *snapshots) close() { _ = s.gw.Close() }

func (s *snapshots) run(ctx context.Context, script string) (string, error) {
	out, err := vmScript(ctx, s.a, s.v, script)
	if err != nil && strings.Contains(err.Error(), guest.NotBtrfs) {
		return out, fmt.Errorf("the data disk of VM %s is not btrfs, so it cannot hold snapshots; recreate the VM: brig delete %[1]s && brig create %[1]s", s.v.Name)
	}
	return out, err
}

// get reads a sandbox, a missing one as not found.
func (s *snapshots) get(ctx context.Context, ws, name string) (osclient.SandboxRecord, error) {
	r, err := s.gw.Sandbox(ctx, ws, name)
	if errors.Is(err, osclient.ErrNoSandbox) {
		return r, notFound(fmt.Errorf("VM %s has no sandbox %s in workspace %s", s.v.Name, name, ws))
	}
	return r, err
}

// phaseWait bounds a wait for a sandbox's phase; tests shorten it.
var phaseWait = sandboxReadyTimeout

// phasePoll is the first pause of a wait for a sandbox's phase.
var phasePoll = 500 * time.Millisecond

// wait waits until sandbox name is in phase, or with phase "", until it
// is gone. A sandbox in error fails the wait.
func (s *snapshots) wait(ctx context.Context, ws, name, phase string) error {
	ctx, cancel := context.WithTimeout(ctx, phaseWait)
	defer cancel()
	pause := phasePoll
	for {
		r, err := s.gw.Sandbox(ctx, ws, name)
		switch {
		case phase == "" && errors.Is(err, osclient.ErrNoSandbox):
			return nil
		case err == nil && r.Phase == phase:
			return nil
		case err == nil && r.Phase == "Error":
			return fmt.Errorf("sandbox %s is in error: %s", name, r.Problem)
		case err == nil && phase == "":
			err = fmt.Errorf("sandbox %s is still there, %s", name, strings.ToLower(r.Phase))
		case err == nil:
			err = fmt.Errorf("sandbox %s is %s", name, strings.ToLower(r.Phase))
		}
		select {
		case <-ctx.Done():
			what := "to be " + strings.ToLower(phase)
			if phase == "" {
				what = "to be gone"
			}
			return fmt.Errorf("waiting for sandbox %s %s: %w", name, what, err)
		case <-time.After(pause):
		}
		pause = min(2*pause, 5*time.Second)
	}
}

// stop stops a ready sandbox and waits until it is stopped.
func (s *snapshots) stop(ctx context.Context, ws, name string) error {
	return inSpan(ctx, progress.KindStep, "stop sandbox "+termtext.Escape(name), func(ctx context.Context) error {
		if err := s.gw.StopSandbox(ctx, ws, name); err != nil {
			return fmt.Errorf("stopping sandbox %s: %w", name, err)
		}
		return s.wait(ctx, ws, name, "Stopped")
	})
}

// start starts a stopped sandbox and waits until it is ready.
func (s *snapshots) start(ctx context.Context, ws, name string) error {
	return inSpan(ctx, progress.KindStep, "start sandbox "+termtext.Escape(name), func(ctx context.Context) error {
		if err := s.gw.StartSandbox(ctx, ws, name); err != nil {
			return fmt.Errorf("starting sandbox %s: %w", name, err)
		}
		return s.wait(ctx, ws, name, "Ready")
	})
}

// stoppable refuses a sandbox that is neither ready nor stopped, whose
// /sandbox may not be worth copying or may be changing.
func stoppable(r osclient.SandboxRecord) error {
	if r.Phase != "Ready" && r.Phase != "Stopped" {
		return notReadyf("sandbox %s is %s; brig copies /sandbox only of a ready or stopped sandbox", r.Name, strings.ToLower(r.Phase))
	}
	return nil
}

// process is a program running in a sandbox.
type process struct {
	PID  int    `json:"pid"`
	Name string `json:"name"`
}

func (p process) String() string { return fmt.Sprintf("%s (pid %d)", p.Name, p.PID) }

// psScript prints "self PID" for itself, then "PID PPID NAME" for each
// process of the sandbox, reading /proc, since a minimal image may lack
// ps.
const psScript = `echo "self $$"
for p in /proc/[0-9]*; do
	read -r name <"$p/comm" 2>/dev/null || continue
	ppid=$(sed -n 's/^PPid:[[:space:]]*//p' "$p/status" 2>/dev/null)
	echo "${p#/proc/} ${ppid:-0} $name"
done
`

// idleProcesses are the programs that hold no work of their own: the
// sandbox's supervisor, its init, the login shell that is the usual main
// process, and the shells of terminals. Names are as /proc/PID/comm has
// them, cut to 15 characters.
var idleProcesses = []string{
	"openshell-sandb", "openshell", "tini", "catatonit", "dumb-init", "sleep", "sshd",
	"sh", "ash", "dash", "bash", "zsh", "fish", "login",
}

// running lists the programs that run in a ready sandbox apart from its
// supervisor and shells, and this check's own.
func (s *snapshots) running(ctx context.Context, ws, name string) ([]process, error) {
	var out, errOut strings.Builder
	code, err := s.gw.Exec(ctx, osclient.ExecRequest{Workspace: ws, Sandbox: name, Command: []string{"sh", "-c", psScript}, Timeout: 30 * time.Second},
		&out, &errOut)
	if err == nil && code != 0 {
		err = fmt.Errorf("exit status %d: %s", code, strings.TrimSpace(errOut.String()))
	}
	if err != nil {
		return nil, err
	}
	return parseProcesses(out.String())
}

// parseProcesses reads what psScript prints and leaves out idle processes
// and the script with its ancestors.
func parseProcesses(out string) ([]process, error) {
	all := map[int]psEntry{}
	self := -1
	sc := bufio.NewScanner(strings.NewReader(out))
	for sc.Scan() {
		f := strings.SplitN(sc.Text(), " ", 3)
		if len(f) == 2 && f[0] == "self" {
			self, _ = strconv.Atoi(f[1])
			continue
		}
		if len(f) != 3 {
			continue
		}
		pid, err1 := strconv.Atoi(f[0])
		ppid, err2 := strconv.Atoi(f[1])
		if err1 == nil && err2 == nil {
			all[pid] = psEntry{ppid, f[2]}
		}
	}
	if self < 0 {
		return nil, errors.New("the process list is incomplete")
	}
	// The script, the processes that started it, and its own children.
	mine := map[int]bool{}
	for p := self; p > 0 && !mine[p]; p = all[p].ppid {
		mine[p] = true
	}
	var procs []process
	for pid, p := range all {
		if !mine[pid] && !descends(all, pid, self) && !slices.Contains(idleProcesses, p.name) {
			procs = append(procs, process{pid, p.name})
		}
	}
	slices.SortFunc(procs, func(a, b process) int { return a.PID - b.PID })
	return procs, nil
}

// psEntry is a process as psScript lists it.
type psEntry struct {
	ppid int
	name string
}

// descends tells whether process pid descends from process anc.
func descends(all map[int]psEntry, pid, anc int) bool {
	seen := map[int]bool{}
	for p := all[pid].ppid; p > 0 && !seen[p]; p = all[p].ppid {
		if p == anc {
			return true
		}
		seen[p] = true
	}
	return false
}

// approval decides whether a command may go ahead with what it is about to
// do: with yes it may; on a terminal it asks; otherwise it may unless
// refuse is set, which says why it needs a yes.
type approval struct {
	ctx context.Context
	in  io.Reader
	w   io.Writer
	yes bool
}

func (ap approval) ask(question, refuse string) error {
	if ap.yes {
		return nil
	}
	if confirm := confirmer(ap.ctx, ap.in, ap.w, false); confirm != nil {
		ok, err := confirm(question + " [y/N] ")
		switch {
		case err != nil:
			return err
		case !ok:
			return errDeclined
		}
		return nil
	}
	if refuse != "" {
		return fmt.Errorf("%s; give --yes to go ahead without a terminal", refuse)
	}
	return nil
}

// errDeclined is the error of a command whose question was answered no.
var errDeclined = errors.New("nothing changed")

// stopCheck says what stopping sandbox r ends, if it is ready: note is a
// sentence naming the programs that run in it, for a question, and refuse
// says why a command without a terminal needs --yes; both are empty if
// only idle programs run.
func (s *snapshots) stopCheck(ctx context.Context, r osclient.SandboxRecord) (note, refuse string) {
	if r.Phase != "Ready" {
		return "", ""
	}
	procs, err := s.running(ctx, r.Workspace, r.Name)
	if err != nil {
		return fmt.Sprintf("Cannot tell what runs in sandbox %s (%s), which stopping it ends.", termtext.Escape(r.Name), termtext.Escape(err.Error())),
			fmt.Sprintf("cannot tell what runs in sandbox %s, which stopping it ends", r.Name)
	}
	if len(procs) == 0 {
		return "", ""
	}
	names := make([]string, len(procs))
	for i, p := range procs {
		names[i] = termtext.Escape(p.String())
	}
	list := strings.Join(names, ", ")
	return fmt.Sprintf("Sandbox %s runs %s.", termtext.Escape(r.Name), list),
		fmt.Sprintf("sandbox %s runs %s, which stopping it ends", r.Name, list)
}

// createOpts are the choices of a snapshot.
type createOpts struct {
	// leaveStopped leaves a ready sandbox stopped afterwards.
	leaveStopped bool
}

// create snapshots sandbox r, which is ready or stopped and which the
// caller may stop. A ready sandbox is stopped for the copy and, unless
// opts.leaveStopped, started again, also when the copy fails.
func (s *snapshots) create(ctx context.Context, r osclient.SandboxRecord, opts createOpts) (snapshot, error) {
	if err := stoppable(r); err != nil {
		return snapshot{}, err
	}
	m := snapshotManifest{Sandbox: r, Services: []osclient.Service{}, Created: s.now().UTC()}
	var err error
	if m.Policy, err = s.gw.BasePolicy(ctx, r.Workspace, r.Name); err != nil {
		fmt.Fprintf(s.errw, "Warning: the snapshot does not record the policy of sandbox %s: %s\n", termtext.Escape(r.Name), termtext.Escape(err.Error()))
	}
	if m.Services, err = s.gw.Services(ctx, r.Workspace, r.Name); err != nil {
		return snapshot{}, fmt.Errorf("the services of sandbox %s: %w", r.Name, err)
	}
	vp, err := s.plan(ctx, r.Volume(), []string{r.Image}, nil, false)
	if err != nil {
		return snapshot{}, err
	}
	if !vp.volume {
		return snapshot{}, fmt.Errorf("sandbox %s has no workspace volume, as for an image with its own WORKDIR, so there is no /sandbox to copy", r.Name)
	}
	m.ImageID = vp.images[r.Image].id
	snap := snapshot{Workspace: r.Workspace, Sandbox: r.Name, Time: m.Created.Truncate(time.Second), Image: r.Image, Template: r.Template, manifest: m}
	data, err := json.Marshal(m)
	if err != nil {
		return snapshot{}, err
	}
	script, err := guest.SnapshotCreateScript(r.Volume(), snap.path(), string(data))
	if err != nil {
		return snapshot{}, err
	}
	ready := r.Phase == "Ready"
	if ready {
		if err := s.stop(ctx, r.Workspace, r.Name); err != nil {
			return snapshot{}, s.restart(ctx, r, ready, err)
		}
	}
	var out string
	err = inSpan(ctx, progress.KindStep, "take snapshot "+termtext.Escape(snap.Name()), func(ctx context.Context) error {
		out, err = s.run(ctx, script)
		return err
	})
	if err != nil {
		return snapshot{}, s.restart(ctx, r, ready, fmt.Errorf("snapshot of sandbox %s: %w", r.Name, err))
	}
	for line := range strings.Lines(out) {
		if f, ok := strings.CutPrefix(strings.TrimSpace(line), "credential "); ok {
			fmt.Fprintf(s.errw, "Warning: snapshot %s holds /sandbox/%s, which seems to hold a real credential, as /login leaves it; the snapshot keeps it until it is deleted\n",
				termtext.Escape(snap.Name()), termtext.Escape(f))
		}
	}
	if ready && !opts.leaveStopped {
		if err := s.start(ctx, r.Workspace, r.Name); err != nil {
			return snap, fmt.Errorf("took snapshot %s, but %w", snap.Name(), err)
		}
	}
	return snap, nil
}

// restart starts sandbox r again after cause if it was ready, and returns
// cause with what failed.
func (s *snapshots) restart(ctx context.Context, r osclient.SandboxRecord, ready bool, cause error) error {
	if !ready {
		return cause
	}
	ctx = context.WithoutCancel(ctx)
	if err := s.start(ctx, r.Workspace, r.Name); err != nil {
		return fmt.Errorf("%w; starting the sandbox again failed too: %w", cause, err)
	}
	return cause
}

// restore replaces the /sandbox of sandbox r, which is ready or stopped and
// which the caller may stop, with snapshot snap, leaving out resets, and
// starts it again if it was ready.
func (s *snapshots) restore(ctx context.Context, snap snapshot, r osclient.SandboxRecord, resets []string) error {
	if err := stoppable(r); err != nil {
		return err
	}
	script, err := guest.SnapshotRestoreScript(r.Volume(), snap.path(), resets)
	if err != nil {
		return err
	}
	ready := r.Phase == "Ready"
	if ready {
		if err := s.stop(ctx, r.Workspace, r.Name); err != nil {
			return s.restart(ctx, r, ready, err)
		}
	}
	var out string
	err = inSpan(ctx, progress.KindStep, "restore snapshot "+termtext.Escape(snap.Name())+" into sandbox "+termtext.Escape(r.Name), func(ctx context.Context) error {
		out, err = s.run(ctx, script)
		return err
	})
	if err != nil {
		return s.restart(ctx, r, ready, fmt.Errorf("restoring snapshot %s into sandbox %s: %w", snap.Name(), r.Name, err))
	}
	if n, ok := strings.CutPrefix(strings.TrimSpace(out), "others "); ok && n != "0" {
		fmt.Fprintf(s.errw, "Warning: the sandbox's user changed; %s files of other owners kept their owner and may not be writable now\n", termtext.Escape(n))
	}
	if ready {
		return s.start(ctx, r.Workspace, r.Name)
	}
	return nil
}

// list lists the VM's snapshots, newest first within each sandbox, and
// with sizes, their sizes. It marks the snapshots whose sandbox is gone
// and those that an unfinished recreate needs.
func (s *snapshots) list(ctx context.Context, sizes bool) ([]snapshot, error) {
	out, err := s.run(ctx, guest.SnapshotListScript(sizes))
	if err != nil {
		return nil, err
	}
	snaps, err := parseSnapshots(out)
	if err != nil {
		return nil, err
	}
	all, err := s.gw.Sandboxes(ctx)
	if err != nil {
		return nil, err
	}
	pending := s.a.recreateSnapshots(s.v)
	for i, sn := range snaps {
		snaps[i].Gone = !slices.ContainsFunc(all, func(x store.Sandbox) bool { return x.Workspace == sn.Workspace && x.Name == sn.Sandbox })
		snaps[i].Recreate = pending[sn.path()]
	}
	return snaps, nil
}

// recreateDir is the directory, in a VM's directory, of the journals of
// the recreates under way, each of which names the snapshot it needs.
const recreateDir = "recreate"

// recreateSnapshots returns the snapshots that unfinished recreates in v
// need, by path.
func (a *app) recreateSnapshots(v *vm.VM) map[string]bool {
	out := map[string]bool{}
	files, _ := filepath.Glob(filepath.Join(a.vmFile(v, recreateDir), "*.json"))
	for _, f := range files {
		var j struct {
			Snapshot string `json:"snapshot"`
		}
		if data, err := os.ReadFile(f); err == nil && json.Unmarshal(data, &j) == nil && j.Snapshot != "" {
			out[j.Snapshot] = true
		}
	}
	return out
}

// parseSnapshots reads what guest.SnapshotListScript prints.
func parseSnapshots(out string) ([]snapshot, error) {
	var snaps []snapshot
	sizes := map[string][2]int64{}
	lines := strings.Split(out, "\n")
	for i := 0; i < len(lines); i++ {
		line := lines[i]
		if rel, ok := strings.CutPrefix(line, "snapshot "); ok {
			sn := snapshot{Size: -1, Exclusive: -1}
			parts := strings.Split(rel, "/")
			if len(parts) != 3 {
				return nil, fmt.Errorf("the VM lists a snapshot %q", rel)
			}
			t, err := time.Parse(snapshotTime, parts[2])
			if err != nil {
				return nil, fmt.Errorf("the VM lists a snapshot %q: %w", rel, err)
			}
			sn.Workspace, sn.Sandbox, sn.Time = parts[0], parts[1], t
			if i+1 < len(lines) {
				i++
				if err := json.Unmarshal([]byte(lines[i]), &sn.manifest); err != nil {
					return nil, fmt.Errorf("the manifest of snapshot %s: %w", rel, err)
				}
			}
			sn.Image, sn.Template = sn.manifest.Sandbox.Image, sn.manifest.Sandbox.Template
			snaps = append(snaps, sn)
			continue
		}
		f := strings.Fields(line)
		if len(f) == 4 && strings.HasSuffix(f[3], "/data") {
			total, err1 := strconv.ParseInt(f[0], 10, 64)
			excl, err2 := strconv.ParseInt(f[1], 10, 64)
			if err1 == nil && err2 == nil {
				sizes[strings.TrimSuffix(f[3], "/data")] = [2]int64{total, excl}
			}
		}
	}
	for i, sn := range snaps {
		if sz, ok := sizes[sn.path()]; ok {
			snaps[i].Size, snaps[i].Exclusive = sz[0], sz[1]
		}
	}
	slices.SortFunc(snaps, func(a, b snapshot) int {
		return cmp.Or(strings.Compare(a.Workspace, b.Workspace), strings.Compare(a.Sandbox, b.Sandbox), b.Time.Compare(a.Time))
	})
	return snaps, nil
}

// find returns the snapshot of workspace ws called name.
func (s *snapshots) find(ctx context.Context, ws, name string) (snapshot, error) {
	sandbox, t, err := parseSnapshotName(name)
	if err != nil {
		return snapshot{}, err
	}
	snaps, err := s.list(ctx, false)
	if err != nil {
		return snapshot{}, err
	}
	for _, sn := range snaps {
		if sn.Workspace == ws && sn.Sandbox == sandbox && sn.Time.Equal(t) {
			return sn, nil
		}
	}
	return snapshot{}, notFound(fmt.Errorf("VM %s has no snapshot %s in workspace %s", s.v.Name, name, ws))
}

// remove deletes snapshots.
func (s *snapshots) remove(ctx context.Context, snaps []snapshot) error {
	if len(snaps) == 0 {
		return nil
	}
	rels := make([]string, len(snaps))
	for i, sn := range snaps {
		rels[i] = sn.path()
	}
	script, err := guest.SnapshotDeleteScript(rels)
	if err != nil {
		return err
	}
	_, err = s.run(ctx, script)
	return err
}

// vmPlan is what guest.SnapshotPlanScript found in the VM.
type vmPlan struct {
	volume bool
	fs     string
	size   int64
	images map[string]vmImage
	mounts map[string]bool
}

// vmImage is an image in the VM: its ID and working directory, both empty
// if the VM lacks the image.
type vmImage struct{ id, workdir string }

func (s *snapshots) plan(ctx context.Context, volume string, images, mounts []string, size bool) (vmPlan, error) {
	out, err := s.run(ctx, guest.SnapshotPlanScript(volume, images, mounts, size))
	if err != nil {
		return vmPlan{}, fmt.Errorf("checking VM %s: %w", s.v.Name, err)
	}
	p := vmPlan{size: -1, images: map[string]vmImage{}, mounts: map[string]bool{}}
	for line := range strings.Lines(out) {
		f := strings.Fields(line)
		switch {
		case len(f) == 2 && f[0] == "volume":
			p.volume = f[1] == "yes"
		case len(f) == 2 && f[0] == "fs":
			p.fs = f[1]
		case len(f) == 2 && f[0] == "size":
			p.size, _ = strconv.ParseInt(f[1], 10, 64)
		case len(f) == 4 && f[0] == "image" && f[2] != "-":
			wd := f[3]
			if wd == "-" {
				wd = ""
			}
			p.images[f[1]] = vmImage{f[2], wd}
		case len(f) == 3 && f[0] == "mount":
			p.mounts[f[1]] = f[2] == "yes"
		}
	}
	if p.fs != "btrfs" {
		return p, fmt.Errorf("the data disk of VM %s is %s, not btrfs, so it cannot hold snapshots; recreate the VM: brig delete %[1]s && brig create %[1]s", s.v.Name, cmp.Or(p.fs, "of an unknown type"))
	}
	return p, nil
}
