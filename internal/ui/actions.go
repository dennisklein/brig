// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

package ui

import (
	"fmt"
	"html/template"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/dennisklein/brig/internal/ui/action"
	"github.com/dennisklein/brig/internal/ui/run"
	"github.com/dennisklein/brig/internal/ui/store"
)

// maxForm is the most a dialog may post, in bytes.
const maxForm = 64 << 10

// A dialog's form is posted twice: to /a/ID/preview as it changes, which
// answers with the command it would run, and to /a/ID/run, which builds the
// command again from the same fields and runs that. The browser never
// sends a command line, only field values, and the second build must give
// the command the first one showed (the "shown" field), or nothing runs.

// dialogSpec describes how an action's dialog looks.
type dialogSpec struct {
	// OK labels the button that runs the command.
	OK string
	// Open labels the buttons that open the dialog.
	Open   string
	Danger bool
	Title  func(d *dialog) string
	// Sub is the line below the title, if any.
	Sub func(d *dialog) string
	// Defaults fills the fields that the dialog opens with, for those that
	// the link does not give.
	Defaults func(d *dialog)
}

// vmTitle titles a dialog with a VM's name.
func vmTitle(format string) func(*dialog) string {
	return func(d *dialog) string { return fmt.Sprintf(format, d.Form.Get("vm")) }
}

var dialogSpecs = map[string]dialogSpec{
	"start":   {OK: "Start", Open: "Start", Title: vmTitle("Start %s")},
	"stop":    {OK: "Stop", Open: "Stop", Title: vmTitle("Stop %s")},
	"delete":  {OK: "Delete VM", Open: "Delete…", Danger: true, Title: vmTitle("Delete %s")},
	"upgrade": {OK: "Upgrade", Open: "Upgrade…", Title: vmTitle("Upgrade %s"), Sub: upgradeSub},
	"mount-add": {
		OK: "Add mount", Open: "Add mount…", Title: vmTitle("Share a host directory with %s"),
		Defaults: func(d *dialog) { d.def("access", "ro") },
	},
	"mount-rm": {OK: "Remove", Open: "Remove", Title: func(d *dialog) string { return "Stop sharing " + d.Form.Get("target") }},
	"profile": {
		OK: "Change", Open: "Change…", Title: vmTitle("Network profile of %s"),
		Defaults: func(d *dialog) { d.def("profile", d.VM.NetworkProfile) },
	},
	"resources": {
		OK: "Save", Open: "Change…", Title: vmTitle("Resources of %s"),
		Defaults: func(d *dialog) {
			d.def("cpus", fmt.Sprint(d.VM.CPUs))
			d.def("memory", d.VM.Memory.String())
			d.def("root-disk", d.VM.RootDisk.String())
			d.def("data-disk", d.VM.DataDisk.String())
		},
	},
	"config-add": {OK: "Add", Open: "Add directory…", Title: vmTitle("Add a config directory to %s")},
	"config-rm":  {OK: "Remove", Open: "Remove", Title: vmTitle("Remove a config directory from %s")},
	"create": {
		OK: "Create", Open: "New VM…", Title: func(*dialog) string { return "New VM" },
		Sub: func(d *dialog) string {
			if d.Snap.NewestImage == nil {
				return "There is no base image yet; brig builds one first."
			}
			return "From the newest base image, " + d.Snap.NewestImage.ID + "."
		},
		Defaults: func(d *dialog) {
			c := d.Snap.Config
			d.def("profile", c.Defaults.NetworkProfile)
			d.def("cpus", fmt.Sprint(c.Defaults.CPUs))
			d.def("memory", c.Defaults.Memory.String())
			if !d.Form.Has("name") {
				d.Form.Set("configs", "on")
				d.Form.Set("start", "on")
			}
		},
	},
	"sync-preview": {OK: "Run the preview", Open: "Preview sync", Title: vmTitle("Preview the sync of %s")},
	"sync":         {OK: "Sync", Open: "Sync…", Title: vmTitle("Sync %s")},
	"image-build": {
		OK: "Build", Open: "Build…", Title: func(*dialog) string { return "Build a base image" },
		Defaults: func(d *dialog) { d.def("fedora", fmt.Sprint(d.Snap.Config.Defaults.FedoraRelease)) },
	},
	"image-prune": {OK: "Prune", Open: "Prune…", Title: func(*dialog) string { return "Prune base images" }},
	"allow": {
		OK: "Allow", Open: "Allow…", Title: sandboxTitle("Allow a request for %s"), Sub: allowSub,
		Defaults: allowDefaults,
	},
	"policy-save-preview": {
		OK: "Run the preview", Open: "Save to file…", Title: sandboxTitle("Save the rules of %s to its policy file"),
	},
	"policy-save": {OK: "Save", Open: "Save…", Title: sandboxTitle("Save the rules of %s to its policy file")},
	"image-push": {
		OK: "Push", Open: "Push an image…", Title: func(*dialog) string { return "Push a container image into a VM" },
		Defaults: func(d *dialog) {
			if !d.Form.Has("vm") {
				if vms := runningVMs(d.Snap); len(vms) > 0 {
					d.Form.Set("vm", vms[0].Name)
				}
			}
		},
	},
}

// sandboxTitle titles a dialog with a sandbox's name.
func sandboxTitle(format string) func(*dialog) string {
	return func(d *dialog) string { return fmt.Sprintf(format, d.Form.Get("sandbox")) }
}

func init() {
	for id, spec := range map[string]dialogSpec{
		"sandbox-create": {
			OK: "Create sandbox", Open: "New sandbox…", Title: func(*dialog) string { return "New sandbox" },
			Sub: func(d *dialog) string {
				if d.Gateway.VM == "" {
					return ""
				}
				return "On " + d.Gateway.Name() + ". Everything stays on its VM; the workspace is /sandbox."
			},
			Defaults: func(d *dialog) {
				if d.Form.Get("vm") == "" {
					if gws := readyGateways(d.Snap); len(gws) > 0 {
						d.Form.Set("vm", gws[0].VM)
					}
				}
				d.Gateway, _ = d.Snap.Gateway(d.Form.Get("vm"))
			},
		},
		"sandbox-start":  {OK: "Start", Open: "Start", Title: sandboxTitle("Start %s")},
		"sandbox-stop":   {OK: "Stop", Open: "Stop", Title: sandboxTitle("Stop %s")},
		"sandbox-delete": {OK: "Delete sandbox", Open: "Delete…", Danger: true, Title: sandboxTitle("Delete %s")},
		"sandbox-attach": {OK: "Attach", Open: "Attach provider…", Title: sandboxTitle("Attach a provider to %s")},
		"sandbox-detach": {OK: "Detach", Open: "Detach", Title: sandboxTitle("Detach a provider from %s")},
	} {
		dialogSpecs[id] = spec
	}
}

// readyGateways lists the gateways that answer, by VM name.
func readyGateways(sn store.Snapshot) []store.Gateway {
	return slices.DeleteFunc(slices.Clone(sn.Gateways), func(g store.Gateway) bool { return g.State != store.GatewayReady })
}

// unattached lists the gateway's providers that the sandbox lacks.
func unattached(g store.Gateway, s store.Sandbox) []store.Provider {
	return slices.DeleteFunc(slices.Clone(g.Providers), func(p store.Provider) bool { return slices.Contains(s.Providers, p.Name) })
}

func upgradeSub(d *dialog) string {
	n := d.Snap.NewestImage
	if n == nil || d.VM.Name == "" {
		return ""
	}
	return fmt.Sprintf("%s → %s, OpenShell %s → %s", d.VM.Image, n.ID, cmpEmpty(d.VM.OpenShell, "unknown"), n.OpenShell)
}

func cmpEmpty(s, empty string) string {
	if s == "" {
		return empty
	}
	return s
}

func runningVMs(sn store.Snapshot) []store.VM {
	return slices.DeleteFunc(slices.Clone(sn.VMs), func(v store.VM) bool { return !v.Running() })
}

// dialog is what a dialog's templates are rendered from.
type dialog struct {
	*view
	ID   string
	Form url.Values
	// VM is the VM that the form's field "vm" names, if there is one.
	VM store.VM
	// Gateway is its gateway, and Sandbox the sandbox that the field
	// "sandbox" names on it, if there are such.
	Gateway store.Gateway
	Sandbox store.Sandbox
	// Typed is what the user must type to confirm, or "".
	Typed string
	Spec  dialogSpec
	// Body is the dialog's fields, rendered from its own template.
	Body template.HTML
}

// def sets the field to v unless the dialog was opened with a value for it.
func (d *dialog) def(field, v string) {
	if !d.Form.Has(field) {
		d.Form.Set(field, v)
	}
}

// Heading is the dialog's title.
func (d *dialog) Heading() string { return d.Spec.Title(d) }

// Lead is the line below the heading.
func (d *dialog) Lead() string {
	if d.Spec.Sub == nil {
		return ""
	}
	return d.Spec.Sub(d)
}

// dialogURL returns the URL of the dialog of the action id, with the fields
// given as key and value pairs filled in.
func dialogURL(id string, kv ...string) string {
	q := url.Values{}
	for i := 0; i+1 < len(kv); i += 2 {
		q.Add(kv[i], kv[i+1])
	}
	if len(q) == 0 {
		return "/a/" + id
	}
	return "/a/" + id + "?" + q.Encode()
}

// actButton is a button that opens an action's dialog.
type actButton struct {
	URL, Label, Class string
	// Disabled and Why say that brig is busy with the VM.
	Disabled bool
	Why      string
}

// act returns the button that opens the dialog of the action id, with the
// fields in kv filled in. It is disabled while a command works on the VM.
func act(v *view, id, label, class string, kv ...string) actButton {
	b := actButton{URL: dialogURL(id, kv...), Label: label, Class: class}
	if label == "" {
		b.Label = actionLabel(id)
	}
	for i := 0; i+1 < len(kv); i += 2 {
		if kv[i] == "vm" && busyVM(v.Snap, kv[i+1]) {
			b.Disabled, b.Why = true, "brig is working on "+kv[i+1]+" already"
		}
	}
	return b
}

// actionLabel labels the buttons that open the dialog of the action id.
func actionLabel(id string) string { return dialogSpecs[id].Open }

// profileNames returns the names of the network profiles, sorted.
func profileNames(sn store.Snapshot) []string {
	names := make([]string, 0, len(sn.Profiles))
	for n := range sn.Profiles {
		names = append(names, n)
	}
	slices.Sort(names)
	return names
}

// vmCommands are the brig commands that take a VM's lock, and so exclude
// each other on it.
var vmCommands = map[string]bool{
	"start": true, "stop": true, "delete": true, "update": true, "upgrade": true, "sync": true, "create": true,
}

// staleRun is how long an unfinished run in the activity log counts as
// working: a command that was killed leaves its run open for good.
const staleRun = 2 * time.Hour

// busyVM reports whether a brig command is working on the named VM, one that
// brig ui started or one that runs in a terminal and shows in the activity
// log. Its actions wait, since the commands take the VM's lock.
func busyVM(sn store.Snapshot, name string) bool {
	for _, t := range sn.Tasks {
		if t.VM == name && !t.Done() {
			return true
		}
	}
	for _, r := range sn.Activity {
		if r.Done() || time.Since(r.Start) > staleRun {
			continue
		}
		if f := strings.Fields(r.Title); len(f) >= 3 && f[0] == "brig" && vmCommands[f[1]] && f[2] == name {
			return true
		}
	}
	return false
}

// taskCard is what a task card is rendered from: the task, and the steps of
// its run in the activity log once they show there.
type taskCard struct {
	store.Task
	Run *store.Run
}

func taskView(sn store.Snapshot, t store.Task) taskCard {
	c := taskCard{Task: t}
	for i := range sn.Activity {
		if t.Trace == "" {
			break
		}
		if sn.Activity[i].Trace == t.Trace {
			c.Run = &sn.Activity[i]
			break
		}
	}
	return c
}

// nextAct returns the button that offers the action that follows a task.
func nextAct(t taskCard) actButton {
	var kv []string
	if t.VM != "" {
		kv = append(kv, "vm", t.VM)
	}
	kv = append(kv, t.NextForm...)
	return actButton{URL: dialogURL(t.Next, kv...), Label: actionLabel(t.Next), Class: "sm primary"}
}

// previewView is what the answer to a preview is rendered from.
type previewView struct {
	OK     string
	Danger bool
	Cmd    action.Command
	// Err is why the form cannot run; Warn is something to read before it
	// does. Blocked disables the button.
	Err, Warn string
	Blocked   bool
}

// lookupAction returns the action of the request, or answers 404.
func (s *Server) lookupAction(w http.ResponseWriter, r *http.Request) (action.Action, bool) {
	a, ok := action.Lookup(r.PathValue("id"))
	if !ok {
		s.notFound(w, r)
	}
	return a, ok
}

func (s *Server) actionContext() action.Context {
	return action.Context{Snap: s.st.Snapshot(), Home: s.home}
}

// dialog answers with a dialog, filled in with the fields in the query.
func (s *Server) dialog(w http.ResponseWriter, r *http.Request) {
	a, ok := s.lookupAction(w, r)
	if !ok {
		return
	}
	v := s.newView(r, "", "")
	d := &dialog{view: v, ID: a.ID, Form: maps(r.URL.Query()), Spec: dialogSpecs[a.ID]}
	d.VM, _ = v.Snap.VM(d.Form.Get("vm"))
	d.Gateway, _ = v.Snap.Gateway(d.Form.Get("vm"))
	d.Sandbox, _ = d.Gateway.Sandbox(d.Form.Get("sandbox"))
	if d.Spec.Defaults != nil {
		d.Spec.Defaults(d)
	}
	d.Typed = a.Want(d.Form)
	body, err := s.tmpl.fragment("dlg-"+a.ID, d)
	if err != nil {
		s.log.Error("rendering dialog", "action", a.ID, "err", err)
		http.Error(w, "brig ui could not render this dialog; its log says why", http.StatusInternalServerError)
		return
	}
	d.Body = template.HTML(body) //nolint:gosec // G203: html/template output, escaped in context
	s.fragmentResponse(w, http.StatusOK, "dialog", d)
}

// maps copies query values, so that a dialog can fill in more.
func maps(q url.Values) url.Values {
	out := make(url.Values, len(q))
	for k, v := range q {
		out[k] = slices.Clone(v)
	}
	return out
}

// postedForm returns the fields that the request posted.
func (s *Server) postedForm(w http.ResponseWriter, r *http.Request) (url.Values, bool) {
	r.Body = http.MaxBytesReader(w, r.Body, maxForm)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "the form is unreadable or too large", http.StatusBadRequest)
		return nil, false
	}
	return r.PostForm, true
}

// evaluate builds the command of the form and decides whether it may run.
func (s *Server) evaluate(a action.Action, f url.Values) previewView {
	spec := dialogSpecs[a.ID]
	p := previewView{OK: spec.OK, Danger: spec.Danger}
	c := s.actionContext()
	cmd, err := a.Build(f, c)
	if err != nil {
		p.Err, p.Blocked = err.Error(), true
		return p
	}
	p.Cmd = cmd
	if vm := a.VM(f); vm != "" && busyVM(c.Snap, vm) {
		p.Err, p.Blocked = "brig is working on "+vm+" already; wait for it to end (see Activity).", true
	} else if !a.Confirmed(f) {
		p.Blocked = true
	}
	return p
}

// preview answers with the command that the form would run.
func (s *Server) preview(w http.ResponseWriter, r *http.Request) {
	a, ok := s.lookupAction(w, r)
	if !ok {
		return
	}
	f, ok := s.postedForm(w, r)
	if !ok {
		return
	}
	s.fragmentResponse(w, http.StatusOK, "preview", s.evaluate(a, f))
}

// runAction runs the command of the form as a task.
func (s *Server) runAction(w http.ResponseWriter, r *http.Request) {
	a, ok := s.lookupAction(w, r)
	if !ok {
		return
	}
	f, ok := s.postedForm(w, r)
	if !ok {
		return
	}
	p := s.evaluate(a, f)
	switch {
	case p.Err != "":
	case !a.Confirmed(f):
		p.Err = "Type " + a.Want(f) + " to confirm."
	case f.Get("shown") != p.Cmd.Display:
		// The state changed under the dialog, such as a VM that stopped.
		p.Warn = "The command changed while you looked. Check it, then press the button again."
		p.Blocked = false
		s.rejected(w, p)
		return
	}
	if p.Err != "" {
		p.Blocked = true
		s.rejected(w, p)
		return
	}
	_, err := s.starter.Start(run.Spec{
		Action: a.ID, Command: p.Cmd, VM: a.VM(f), ShowOutput: a.ShowOutput, Next: a.Next, NextForm: p.Cmd.NextForm,
	})
	if err != nil {
		s.log.Error("starting a task", "action", a.ID, "err", err)
		p.Err, p.Blocked = err.Error(), true
		s.rejected(w, p)
		return
	}
	s.fragmentResponse(w, http.StatusOK, "tasks", s.newView(r, "", ""))
}

// rejected answers a run that did not start: htmx puts the preview, with the
// reason, into the dialog.
func (s *Server) rejected(w http.ResponseWriter, p previewView) {
	w.Header().Set("HX-Retarget", "#preview")
	w.Header().Set("HX-Reswap", "innerHTML")
	s.fragmentResponse(w, http.StatusUnprocessableEntity, "preview", p)
}

// dismiss removes a finished task from the cards.
func (s *Server) dismiss(w http.ResponseWriter, r *http.Request) {
	if t, ok := s.st.Snapshot().Task(r.PathValue("id")); ok && t.Done() {
		s.st.RemoveTask(t.ID)
	}
	s.fragmentResponse(w, http.StatusOK, "tasks", s.newView(r, "", ""))
}

// fragmentResponse writes the named fragment.
func (s *Server) fragmentResponse(w http.ResponseWriter, status int, name string, data any) {
	html, err := s.tmpl.fragment(name, data)
	if err != nil {
		s.log.Error("rendering fragment", "fragment", name, "err", err)
		http.Error(w, "rendering failed", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(html)) //nolint:gosec // G705: html/template output, escaped in context
}

// terminal answers with the dialog that shows the commands to connect to a
// sandbox from the user's own terminal.
func (s *Server) terminal(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	v := s.newView(r, "", "")
	g, ok := v.Snap.Gateway(q.Get("vm"))
	if !ok || !action.ValidSandboxName(q.Get("sandbox")) {
		s.notFound(w, r)
		return
	}
	sb, ok := g.Sandbox(q.Get("sandbox"))
	if !ok {
		s.notFound(w, r)
		return
	}
	v.Gateway, v.Sandbox = g, sb
	s.fragmentResponse(w, http.StatusOK, "terminal", v)
}
