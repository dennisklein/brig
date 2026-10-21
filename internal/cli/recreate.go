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
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/GSI-HPC/go-clikit/progress"
	"github.com/GSI-HPC/go-clikit/termtext"

	"github.com/dennisklein/brig/internal/bytesize"
	"github.com/dennisklein/brig/internal/ocsync"
	"github.com/dennisklein/brig/internal/osclient"
	"github.com/dennisklein/brig/internal/ui/store"
	"github.com/dennisklein/brig/internal/vm"
)

// recreatedFrom is the annotation of a recreated sandbox that holds the
// image of the sandbox it replaced.
const recreatedFrom = "brig.dev/recreated-from"

// recreatePlan is what brig sandbox recreate does to one sandbox, as
// --dry-run prints it.
type recreatePlan struct {
	VM        string `json:"vm"`
	Workspace string `json:"workspace"`
	Sandbox   string `json:"sandbox"`
	Phase     string `json:"phase"`
	FromImage string `json:"from_image"`
	ToImage   string `json:"to_image"`
	// Template is the template the new sandbox comes from; without one,
	// it gets the old sandbox's spec with ToImage.
	Template  string             `json:"template,omitempty"`
	Providers []string           `json:"providers"`
	Services  []osclient.Service `json:"services"`
	// Policy says which policy the new sandbox gets and how it compares
	// with the sandbox's policy file.
	Policy string `json:"policy"`
	// Size is what /sandbox holds, -1 if unknown.
	Size int64 `json:"size"`
	// Running are the programs that stopping the sandbox ends; Unknown
	// says why brig cannot tell.
	Running []process `json:"running"`
	Unknown string    `json:"running_unknown,omitempty"`
	Resets  []string  `json:"resets,omitempty"`
	Notes   []string  `json:"notes,omitempty"`
	Steps   []string  `json:"steps"`

	old    osclient.SandboxRecord
	policy json.RawMessage
}

// recreateJournal is the record of a recreate under way, which brig keeps
// on the host until the recreate is done or rolled back. It is also the
// lock that keeps a second recreate of the sandbox away.
type recreateJournal struct {
	// Phase is the last step done: planned, snapshotted, deleted, created
	// or restored.
	Phase string `json:"phase"`
	// Snapshot is the snapshot's path, once it is taken.
	Snapshot string `json:"snapshot,omitempty"`
	// Ready says that the old sandbox was ready, and so is started again.
	Ready    bool                   `json:"ready"`
	Template string                 `json:"template,omitempty"`
	Image    string                 `json:"image"`
	Resets   []string               `json:"resets,omitempty"`
	Old      osclient.SandboxRecord `json:"old"`
	Policy   json.RawMessage        `json:"policy"`
	Services []osclient.Service     `json:"services"`
	Started  time.Time              `json:"started"`

	file string
}

func (a *app) journalFile(v *vm.VM, ws, name string) string {
	return a.vmFile(v, filepath.Join(recreateDir, ws+"--"+name+".json"))
}

// readJournal reads the journal of a recreate of sandbox name, nil if
// there is none.
func (a *app) readJournal(v *vm.VM, ws, name string) (*recreateJournal, error) {
	file := a.journalFile(v, ws, name)
	data, err := os.ReadFile(file)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	j := &recreateJournal{file: file}
	if err := json.Unmarshal(data, j); err != nil {
		return nil, fmt.Errorf("%s: %w", file, err)
	}
	return j, nil
}

// save writes the journal at the given phase.
func (j *recreateJournal) save(phase string) error {
	j.Phase = phase
	data, err := json.MarshalIndent(j, "", "  ")
	if err != nil {
		return err
	}
	tmp := j.file + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, j.file)
}

// lock creates the journal file, refusing if a recreate of the sandbox is
// unfinished.
func (j *recreateJournal) lock(vmName string) error {
	if err := os.MkdirAll(filepath.Dir(j.file), 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(j.file, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if errors.Is(err, fs.ErrExist) {
		return alreadyExists(fmt.Errorf("a recreate of sandbox %s is unfinished; finish it with brig sandbox recreate %s %[1]s -w %s --resume, or undo it with --rollback",
			j.Old.Name, vmName, j.Old.Workspace))
	}
	if err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return j.save("planned")
}

func (j *recreateJournal) remove() error {
	if err := os.Remove(j.file); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

// recreateTarget is where brig sandbox recreate moves a sandbox: the
// template, else the image.
type recreateTarget struct{ template, image string }

// planOpts are the choices of a recreate besides its target.
type planOpts struct {
	resets []string
	// dry also measures /sandbox and compares the policy with the
	// sandbox's policy file.
	dry bool
	// copyImage says that a target image that the VM lacks is copied
	// from the host first.
	copyImage bool
	// policy is the policy file to give the new sandbox: "file" for the
	// sandbox's own, as brig policy diff finds it, else a file's path or
	// name; empty keeps the sandbox's current base policy.
	policy string
}

// planRecreate checks what recreating sandbox ws/name on target needs,
// without changing anything.
func (s *snapshots) planRecreate(ctx context.Context, ws, name string, target recreateTarget, o planOpts) (*recreatePlan, error) {
	old, err := s.get(ctx, ws, name)
	if err != nil {
		return nil, err
	}
	if err := stoppable(old); err != nil {
		return nil, err
	}
	p := &recreatePlan{
		VM: s.v.Name, Workspace: ws, Sandbox: name, Phase: old.Phase, FromImage: old.Image, Template: target.template,
		Providers: old.Providers(), Resets: o.resets, Size: -1, Running: []process{}, old: old,
	}
	if p.Providers == nil {
		p.Providers = []string{}
	}
	if target.template != "" {
		tmpls, err := s.gw.Templates(ctx)
		if err != nil {
			return nil, err
		}
		i := slices.IndexFunc(tmpls, func(t store.Template) bool { return t.Workspace == ws && t.Name == target.template })
		if i < 0 {
			return nil, notFound(fmt.Errorf("VM %s has no template %s in workspace %s", s.v.Name, target.template, ws))
		}
		p.ToImage = tmpls[i].Image
	} else {
		p.ToImage = target.image
	}
	if p.ToImage == "" {
		return nil, fmt.Errorf("cannot tell which image sandbox %s would get", name)
	}

	provs, err := s.gw.Providers(ctx)
	if err != nil {
		return nil, err
	}
	var missing []string
	for _, pr := range p.Providers {
		if !slices.ContainsFunc(provs, func(x store.Provider) bool { return x.Workspace == ws && x.Name == pr }) {
			missing = append(missing, pr)
		}
	}
	if len(missing) > 0 {
		return nil, notFound(fmt.Errorf("sandbox %s attaches provider %s, which workspace %s of VM %s lacks; create it with brig sync %[4]s",
			name, strings.Join(missing, ", "), ws, s.v.Name))
	}
	if p.policy, err = s.gw.BasePolicy(ctx, ws, name); err != nil {
		return nil, fmt.Errorf("the policy of sandbox %s: %w", name, err)
	}
	if p.Services, err = s.gw.Services(ctx, ws, name); err != nil {
		return nil, fmt.Errorf("the services of sandbox %s: %w", name, err)
	}

	mounts := old.VolumeMounts()
	vp, err := s.plan(ctx, old.Volume(), []string{p.ToImage, old.Image}, mounts, o.dry)
	if err != nil {
		return nil, err
	}
	if !vp.volume {
		return nil, fmt.Errorf("sandbox %s has no workspace volume, as for an image with its own WORKDIR, so there is no /sandbox to move", name)
	}
	p.Size = vp.size
	img, ok := vp.images[p.ToImage]
	switch {
	case !ok && o.copyImage:
		p.Notes = append(p.Notes, "brig copies the image from the host's Podman into the VM first")
	case !ok:
		return nil, notFound(fmt.Errorf("VM %s lacks image %s; copy it in with brig image push %[1]s %[2]s", s.v.Name, p.ToImage))
	case img.workdir != "" && img.workdir != "/" && img.workdir != sandboxHome:
		return nil, fmt.Errorf("image %s works in %s, so its sandboxes get no workspace volume to restore /sandbox into", p.ToImage, img.workdir)
	}
	if _, ok := vp.images[old.Image]; !ok {
		return nil, notFound(fmt.Errorf("VM %s lacks image %s of sandbox %s, which a rollback would need", s.v.Name, old.Image, name))
	}
	for _, m := range mounts {
		if !vp.mounts[m] {
			return nil, notFound(fmt.Errorf("sandbox %s attaches volume %s, which VM %s lacks", name, m, s.v.Name))
		}
	}

	p.Policy = "the sandbox's current base policy"
	if o.policy != "" {
		path, data, err := filePolicy(ctx, s.a, s.v, ws, name, o.policy)
		if err != nil {
			return nil, err
		}
		// The sandbox keeps Docker, which brig sandbox create --docker
		// gave it, unless the file has a rule of its own for it.
		data, kept, err := osclient.KeepRule(p.policy, data, dockerRuleKey)
		if err != nil {
			return nil, fmt.Errorf("the policy of sandbox %s: %w", name, err)
		}
		p.policy, p.Policy = data, "policy file "+path
		if kept {
			p.Policy += ", with rule " + dockerRuleKey + " of the sandbox"
		}
	} else if o.dry {
		p.Policy += policyNote(ctx, s.a, s.v, ws, name)
	}
	if old.Phase == "Ready" {
		if procs, err := s.running(ctx, ws, name); err != nil {
			p.Unknown = err.Error()
		} else if procs != nil {
			p.Running = procs
		}
	}
	if cmd := old.Command(); len(cmd) > 0 {
		p.Notes = append(p.Notes, fmt.Sprintf("the sandbox's command, %s, runs briefly against an empty /sandbox before the restore, since a sandbox cannot be created stopped",
			strings.Join(cmd, " ")))
	}
	if target.template == "" && old.Template != "" {
		p.Notes = append(p.Notes, fmt.Sprintf("the new sandbox does not come from template %s, so brig agent update leaves it alone", old.Template))
	}
	if old.Phase == "Ready" {
		p.Steps = append(p.Steps, "stop")
	}
	p.Steps = append(p.Steps, "snapshot", "delete", "create", "restore")
	if old.Phase == "Ready" {
		p.Steps = append(p.Steps, "start")
	}
	return p, nil
}

// policyNote says how a sandbox's base policy compares with its policy
// file; tests replace it.
var policyNote = func(ctx context.Context, a *app, v *vm.VM, ws, name string) string {
	rep, err := a.policyDiff(ctx, v, ws, name, "", false, io.Discard)
	switch {
	case err != nil:
		return "; cannot compare it with a policy file: " + err.Error()
	case rep.Source == nil:
		return "; brig cannot tell which policy file it came from"
	case !rep.Drift:
		return ", the same as " + rep.Source.Path
	}
	return fmt.Sprintf("; %d rules differ from %s (brig policy diff %s %s)", len(rep.Rules), rep.Source.Path, v.Name, name)
}

// filePolicy reads the policy file that --policy names, choice, for
// sandbox name: "file" for the sandbox's own, as brig policy diff finds it,
// else a file's path or name. Tests replace it.
var filePolicy = func(ctx context.Context, a *app, v *vm.VM, ws, name, choice string) (string, json.RawMessage, error) {
	var path string
	if choice == "file" {
		rep, err := a.policyDiff(ctx, v, ws, name, "", false, io.Discard)
		if err != nil {
			return "", nil, err
		}
		if rep.Source == nil {
			return "", nil, fmt.Errorf("cannot tell which policy file sandbox %s came from (see brig policy diff); name it with --policy FILE", name)
		}
		path = rep.Source.Path
	} else {
		var set *ocsync.Set
		if len(v.OpenShellConfigs) > 0 {
			var err error
			if set, err = ocsync.Load(v.OpenShellConfigs); err != nil {
				return "", nil, err
			}
		}
		var err error
		if path, err = policyFileNamed(set, choice); err != nil {
			return "", nil, err
		}
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", nil, err
	}
	pol, err := osclient.PolicyJSON(data)
	if err != nil {
		return "", nil, fmt.Errorf("%s: %w", path, err)
	}
	return path, pol, nil
}

// printRecreatePlan prints p as --dry-run does.
func printRecreatePlan(w io.Writer, p *recreatePlan) {
	e := termtext.Escape
	fmt.Fprintf(w, "Sandbox %s (workspace %s), %s\n", e(p.Sandbox), e(p.Workspace), strings.ToLower(p.Phase))
	to := e(p.ToImage)
	if p.Template != "" {
		to += " (template " + e(p.Template) + ")"
	}
	fmt.Fprintf(w, "  image     %s -> %s\n", e(p.FromImage), to)
	if len(p.Providers) > 0 {
		fmt.Fprintf(w, "  providers %s (all present)\n", e(strings.Join(p.Providers, ", ")))
	}
	fmt.Fprintf(w, "  policy    %s\n", e(p.Policy))
	if len(p.Services) > 0 {
		names := make([]string, len(p.Services))
		for i, s := range p.Services {
			names[i] = fmt.Sprintf("%s:%d", s.Name, s.Port)
		}
		fmt.Fprintf(w, "  services  %s, exposed again\n", e(strings.Join(names, ", ")))
	}
	if p.Size >= 0 {
		fmt.Fprintf(w, "  /sandbox  %s; a snapshot shares its blocks\n", bytesize.Size(p.Size).Approx())
	}
	if len(p.Resets) > 0 {
		fmt.Fprintf(w, "  reset     %s, set up fresh from the new image\n", e(strings.Join(p.Resets, ", ")))
	}
	switch {
	case p.Unknown != "":
		fmt.Fprintf(w, "  running   cannot tell (%s): asks before stopping\n", e(p.Unknown))
	case len(p.Running) > 0:
		names := make([]string, len(p.Running))
		for i, r := range p.Running {
			names[i] = r.String()
		}
		fmt.Fprintf(w, "  running   %s: will be stopped\n", e(strings.Join(names, ", ")))
	}
	for _, n := range p.Notes {
		fmt.Fprintf(w, "  note      %s\n", e(n))
	}
	fmt.Fprintf(w, "  steps     %s\n", strings.Join(p.Steps, ", "))
}

// recreate carries out plan p: it snapshots the old sandbox, deletes it,
// creates the new one, restores the snapshot into it and starts it. A
// failure once the old sandbox is gone rolls back. It returns the
// snapshot.
func (s *snapshots) recreate(ctx context.Context, p *recreatePlan) (snapshot, error) {
	j := &recreateJournal{
		file: s.a.journalFile(s.v, p.Workspace, p.Sandbox), Ready: p.old.Phase == "Ready", Template: p.Template, Image: p.ToImage,
		Resets: p.Resets, Old: p.old, Policy: p.policy, Services: p.Services, Started: s.now().UTC(),
	}
	if err := j.lock(s.v.Name); err != nil {
		return snapshot{}, err
	}
	return s.forward(ctx, j)
}

// forward takes the recreate of j on from its phase, and rolls back on a
// failure once the old sandbox is gone.
func (s *snapshots) forward(ctx context.Context, j *recreateJournal) (snapshot, error) {
	ws, name := j.Old.Workspace, j.Old.Name
	snap := snapshotOf(j)
	if j.Phase == "planned" {
		r, err := s.gw.Sandbox(ctx, ws, name)
		if err == nil {
			snap, err = s.create(ctx, r, createOpts{leaveStopped: true})
		}
		if err != nil {
			return snapshot{}, s.undoKeep(ctx, j, err)
		}
		j.Snapshot = snap.path()
		if err := j.save("snapshotted"); err != nil {
			return snap, s.undoKeep(ctx, j, err)
		}
	}
	if j.Phase == "snapshotted" {
		err := inSpan(ctx, progress.KindStep, "delete sandbox "+termtext.Escape(name), func(ctx context.Context) error {
			if err := s.gw.DeleteSandbox(ctx, ws, name); err != nil {
				return fmt.Errorf("deleting sandbox %s: %w", name, err)
			}
			return s.wait(ctx, ws, name, "")
		})
		if err != nil {
			if _, gerr := s.gw.Sandbox(context.WithoutCancel(ctx), ws, name); !errors.Is(gerr, osclient.ErrNoSandbox) {
				return snap, s.undoKeep(ctx, j, err)
			}
			return snap, s.rollback(ctx, j, err)
		}
		if err := j.save("deleted"); err != nil {
			return snap, s.rollback(ctx, j, err)
		}
	}
	if j.Phase == "deleted" {
		if err := s.createStopped(ctx, j, false); err != nil {
			return snap, s.rollback(ctx, j, err)
		}
		if err := j.save("created"); err != nil {
			return snap, s.rollback(ctx, j, err)
		}
	}
	if j.Phase == "created" {
		r, err := s.gw.Sandbox(ctx, ws, name)
		if err == nil {
			err = s.restore(ctx, snap, r, j.Resets)
		}
		if err != nil {
			return snap, s.rollback(ctx, j, err)
		}
		if err := j.save("restored"); err != nil {
			return snap, s.rollback(ctx, j, err)
		}
	}
	if j.Phase == "restored" {
		r, err := s.gw.Sandbox(ctx, ws, name)
		if err == nil && j.Ready && r.Phase != "Ready" {
			err = s.start(ctx, ws, name)
		}
		if err != nil {
			return snap, s.rollback(ctx, j, err)
		}
	}
	return snap, j.remove()
}

func snapshotOf(j *recreateJournal) snapshot {
	sn := snapshot{Workspace: j.Old.Workspace, Sandbox: j.Old.Name, Size: -1, Exclusive: -1}
	if parts := strings.Split(j.Snapshot, "/"); len(parts) == 3 {
		sn.Time, _ = time.Parse(snapshotTime, parts[2])
	}
	return sn
}

// createStopped creates the sandbox of j, the new one, or with old, the
// old one again, waits until it is ready and stops it, ready for the
// restore. A sandbox of that name that is there already is a leftover of
// an earlier attempt, since the old one is gone, and goes first.
func (s *snapshots) createStopped(ctx context.Context, j *recreateJournal, old bool) error {
	ws, name := j.Old.Workspace, j.Old.Name
	if err := s.deleteLeftover(ctx, ws, name); err != nil {
		return err
	}
	req := osclient.CreateRequest{
		Workspace: ws, Name: name, Spec: j.Old.Spec, Policy: j.Policy, Labels: maps.Clone(j.Old.Labels), Services: j.Services,
	}
	what := "create sandbox " + termtext.Escape(name) + " on " + termtext.Escape(j.Image)
	if old {
		what = "create sandbox " + termtext.Escape(name) + " again on " + termtext.Escape(j.Old.Image)
	} else {
		req.Template, req.Image = j.Template, j.Image
		req.Annotations = map[string]string{recreatedFrom: j.Old.Image}
	}
	return inSpan(ctx, progress.KindStep, what, func(ctx context.Context) error {
		if err := s.gw.CreateSandbox(ctx, req); err != nil {
			return fmt.Errorf("creating sandbox %s: %w", name, err)
		}
		if err := s.wait(ctx, ws, name, "Ready"); err != nil {
			return err
		}
		if err := s.gw.StopSandbox(ctx, ws, name); err != nil {
			return fmt.Errorf("stopping sandbox %s: %w", name, err)
		}
		return s.wait(ctx, ws, name, "Stopped")
	})
}

// deleteLeftover deletes sandbox name if it is there and waits until it is
// gone.
func (s *snapshots) deleteLeftover(ctx context.Context, ws, name string) error {
	if _, err := s.gw.Sandbox(ctx, ws, name); errors.Is(err, osclient.ErrNoSandbox) {
		return nil
	}
	if err := s.gw.DeleteSandbox(ctx, ws, name); err != nil {
		return fmt.Errorf("deleting sandbox %s: %w", name, err)
	}
	return s.wait(ctx, ws, name, "")
}

// undoKeep ends a recreate that failed while the old sandbox was still
// there: it starts the sandbox again if it was ready and drops the journal.
// A snapshot taken stays.
func (s *snapshots) undoKeep(ctx context.Context, j *recreateJournal, cause error) error {
	ctx = context.WithoutCancel(ctx)
	errs := []error{cause}
	if j.Ready {
		if r, err := s.gw.Sandbox(ctx, j.Old.Workspace, j.Old.Name); err == nil && r.Phase == "Stopped" {
			if err := s.start(ctx, j.Old.Workspace, j.Old.Name); err != nil {
				errs = append(errs, fmt.Errorf("starting sandbox %s again: %w", j.Old.Name, err))
			}
		}
	}
	return errors.Join(append(errs, j.remove())...)
}

// rollbackTimeout bounds a rollback, which goes on after Ctrl-C.
var rollbackTimeout = 15 * time.Minute

// rollback brings back the old sandbox after cause, once it is gone: it
// deletes the new one, creates the old one from its spec on its image,
// restores the snapshot and starts it if it was ready.
func (s *snapshots) rollback(ctx context.Context, j *recreateJournal, cause error) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), rollbackTimeout)
	defer cancel()
	ws, name := j.Old.Workspace, j.Old.Name
	fmt.Fprintf(s.errw, "Recreating sandbox %s failed: %s\nRolling back to %s.\n", termtext.Escape(name), termtext.Escape(cause.Error()), termtext.Escape(j.Old.Image))
	err := s.back(ctx, j)
	if err == nil {
		return fmt.Errorf("recreating sandbox %s failed, and it is back on %s: %w", name, j.Old.Image, cause)
	}
	return fmt.Errorf("recreating sandbox %s failed: %w\nThe rollback failed too: %w\nSnapshot %s holds its /sandbox. "+
		"Retry with brig sandbox recreate %s %[1]s -w %s --resume, or roll back with --rollback",
		name, cause, err, snapshotOf(j).Name(), s.v.Name, ws)
}

// back rolls the recreate of j back from its phase.
func (s *snapshots) back(ctx context.Context, j *recreateJournal) error {
	ws, name := j.Old.Workspace, j.Old.Name
	switch j.Phase {
	case "planned", "snapshotted":
		// The old sandbox is still there, unless a delete got through.
		if _, err := s.gw.Sandbox(ctx, ws, name); !errors.Is(err, osclient.ErrNoSandbox) {
			return s.undoKeep(ctx, j, nil)
		}
		if j.Snapshot == "" {
			return fmt.Errorf("sandbox %s is gone and no snapshot of it was taken", name)
		}
	}
	if err := s.createStopped(ctx, j, true); err != nil {
		return err
	}
	r, err := s.gw.Sandbox(ctx, ws, name)
	if err != nil {
		return err
	}
	if err := s.restore(ctx, snapshotOf(j), r, nil); err != nil {
		return err
	}
	if j.Ready {
		if err := s.start(ctx, ws, name); err != nil {
			return err
		}
	}
	return j.remove()
}
