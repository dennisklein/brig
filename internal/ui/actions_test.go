// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

package ui

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dennisklein/brig/internal/ui/action"
	"github.com/dennisklein/brig/internal/ui/run"
	"github.com/dennisklein/brig/internal/ui/store"
)

// fakeStarter records the commands it is asked to run.
type fakeStarter struct {
	st    *store.Store
	mu    sync.Mutex
	specs []run.Spec
}

func (f *fakeStarter) Start(s run.Spec) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.specs = append(f.specs, s)
	f.st.AddTask(store.Task{ID: "task1", Action: s.Action, Display: s.Command.Display, VM: s.VM, Start: time.Now(), Status: store.TaskRunning})
	return "task1", nil
}

func (f *fakeStarter) Running() int           { return 0 }
func (f *fakeStarter) Shutdown(time.Duration) {}

func (f *fakeStarter) started() []run.Spec {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.specs)
}

// post sends a form as the page's htmx would.
func (ts *testServer) post(target string, form url.Values) *httptest.ResponseRecorder {
	ts.t.Helper()
	r := request("POST", target)
	r.Body = http.NoBody
	r = httptest.NewRequest("POST", "http://127.0.0.1:7878"+target, strings.NewReader(form.Encode()))
	r.Host = "127.0.0.1:7878"
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.Header.Set("Origin", "http://127.0.0.1:7878")
	r.Header.Set(csrfHeader, ts.csrf)
	r.AddCookie(ts.cookie)
	return ts.do(r)
}

// shownBy returns the command that the preview of the form shows, which the
// dialog sends back with the run.
func (ts *testServer) shownBy(id string, form url.Values) string {
	ts.t.Helper()
	w := ts.post("/a/"+id+"/preview", form)
	if w.Code != http.StatusOK {
		ts.t.Fatalf("preview of %s: %d", id, w.Code)
	}
	m := regexp.MustCompile(`name="shown" value="([^"]*)"`).FindStringSubmatch(w.Body.String())
	if m == nil {
		ts.t.Fatalf("preview of %s shows no command:\n%s", id, w.Body)
	}
	return strings.NewReplacer("&#39;", "'", "&amp;", "&").Replace(m[1])
}

func values(kv ...string) url.Values {
	v := url.Values{}
	for c := range slices.Chunk(kv, 2) {
		v.Add(c[0], c[1])
	}
	return v
}

func TestEveryDialogRenders(t *testing.T) {
	ts := newTestServer(t)
	ts.login()
	for _, id := range action.IDs() {
		w := ts.get("/a/" + id + "?vm=dev&target=/mnt/pi-kit&dir=/home/dklein/src/agent-openshell")
		body := w.Body.String()
		if w.Code != http.StatusOK || !strings.Contains(body, `hx-post="/a/`+id+`/run"`) || !strings.Contains(body, `hx-post="/a/`+id+`/preview"`) {
			t.Errorf("%s: %d\n%s", id, w.Code, body)
		}
		if _, ok := dialogSpecs[id]; !ok {
			t.Errorf("action %s has no dialog spec", id)
		}
	}
	if w := ts.get("/a/nope"); w.Code != http.StatusNotFound {
		t.Errorf("unknown action: %d", w.Code)
	}
}

func TestDialogFieldsAreFilledIn(t *testing.T) {
	ts := newTestServer(t)
	ts.login()
	body := ts.get("/a/resources?vm=dev").Body.String()
	for _, want := range []string{`value="4"`, `value="8GiB"`, `value="20GiB"`, `value="40GiB"`, "Resources of dev"} {
		if !strings.Contains(body, want) {
			t.Errorf("resources dialog lacks %q", want)
		}
	}
	body = ts.get("/a/create").Body.String()
	for _, want := range []string{"New VM", `name="configs" checked`, `name="start" checked`, `<option value="default" selected>`, "~/src/agent-openshell"} {
		if !strings.Contains(body, want) {
			t.Errorf("create dialog lacks %q", want)
		}
	}
	if body := ts.get("/a/delete?vm=dev").Body.String(); !strings.Contains(body, `Type <b class="mono">dev</b> to confirm`) {
		t.Error("delete asks for no typed name")
	}
	if body := ts.get("/a/start?vm=dev").Body.String(); strings.Contains(body, "to confirm") {
		t.Error("start asks for a typed name")
	}
	if body := ts.get("/a/upgrade?vm=airgap").Body.String(); !strings.Contains(body, "f44-openshell0.1.1-20260921T143000Z → f44-openshell0.1.2-20261010T091500Z, OpenShell 0.1.1 → 0.1.2") {
		t.Errorf("upgrade says no versions:\n%s", body)
	}
}

func TestPreview(t *testing.T) {
	ts := newTestServer(t)
	ts.login()
	w := ts.post("/a/stop/preview", values("vm", "dev", "force", "on"))
	body := w.Body.String()
	for _, want := range []string{"brig stop dev --force", `name="shown" value="brig stop dev --force"`, `data-copy="brig stop dev --force"`, `id="dialog-ok"`, `hx-swap-oob="true"`} {
		if !strings.Contains(body, want) {
			t.Errorf("preview lacks %q:\n%s", want, body)
		}
	}
	if strings.Contains(body, " disabled") {
		t.Error("a valid form disables the button")
	}
	// A form that cannot run says why, shows no command, and disables the
	// button.
	body = ts.post("/a/start/preview", values("vm", "dev")).Body.String()
	if !strings.Contains(body, "running already") || strings.Contains(body, `name="shown"`) || !strings.Contains(body, ` disabled`) {
		t.Errorf("start of a running VM:\n%s", body)
	}
	// The command is shown while the confirmation is missing, and the
	// button waits for it.
	body = ts.post("/a/delete/preview", values("vm", "dev")).Body.String()
	if !strings.Contains(body, "brig delete dev --force") || !strings.Contains(body, ` disabled`) {
		t.Errorf("delete without confirmation:\n%s", body)
	}
	body = ts.post("/a/delete/preview", values("vm", "dev", "confirm", "dev")).Body.String()
	if strings.Contains(body, ` disabled`) || !strings.Contains(body, `class="btn danger"`) {
		t.Errorf("delete with confirmation:\n%s", body)
	}
	// What the user typed is escaped.
	body = ts.post("/a/mount-add/preview", values("vm", "dev", "src", `/data/<img src=x>`, "target", "/mnt/x")).Body.String()
	if strings.Contains(body, "<img") {
		t.Errorf("a path was not escaped:\n%s", body)
	}
}

func TestRunStartsTheCommandOfTheForm(t *testing.T) {
	ts := newTestServer(t)
	ts.login()
	form := values("vm", "airgap")
	form.Set("shown", ts.shownBy("start", form))
	// A browser may send more than fields; none of it reaches the command.
	form.Set("argv", "rm")
	form.Add("argv", "-rf")
	form.Set("Argv", "rm")
	w := ts.post("/a/start/run", form)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `id="task-task1"`) || !strings.Contains(w.Body.String(), "brig start airgap") {
		t.Fatalf("run: %d\n%s", w.Code, w.Body)
	}
	specs := ts.starts.started()
	if len(specs) != 1 || !slices.Equal(specs[0].Command.Argv, []string{"brig", "start", "airgap"}) || specs[0].VM != "airgap" || specs[0].Action != "start" {
		t.Fatalf("started %+v", specs)
	}
}

func TestRunPassesWhatTheActionAsks(t *testing.T) {
	ts := newTestServer(t)
	ts.login()
	form := values("vm", "dev")
	form.Set("shown", ts.shownBy("sync-preview", form))
	if w := ts.post("/a/sync-preview/run", form); w.Code != http.StatusOK {
		t.Fatalf("run: %d", w.Code)
	}
	s := ts.starts.started()[0]
	if !s.ShowOutput || s.Next != "sync" || s.Command.Display != "brig sync dev --dry-run" {
		t.Errorf("started %+v", s)
	}
}

func TestRunRefuses(t *testing.T) {
	ts := newTestServer(t)
	ts.login()
	refused := func(name, id string, form url.Values, want string) {
		t.Helper()
		w := ts.post("/a/"+id+"/run", form)
		if w.Code != http.StatusUnprocessableEntity || !strings.Contains(w.Body.String(), want) {
			t.Errorf("%s: %d, want 422 with %q:\n%s", name, w.Code, want, w.Body)
		}
		if w.Header().Get("HX-Retarget") != "#preview" {
			t.Errorf("%s: the answer is not retargeted to the preview", name)
		}
		if n := len(ts.starts.started()); n != 0 {
			t.Errorf("%s: %d commands started", name, n)
		}
	}
	// No preview came first.
	refused("no shown", "start", values("vm", "airgap"), "changed while you looked")
	// The command differs from what the dialog showed.
	refused("stale", "start", values("vm", "airgap", "shown", "brig start dev"), "changed while you looked")
	// The form cannot run.
	refused("running", "start", values("vm", "dev", "shown", "brig start dev"), "running already")
	// Delete needs its name, whatever the browser did.
	del := values("vm", "dev")
	del.Set("shown", ts.shownBy("delete", del))
	refused("unconfirmed", "delete", del, "Type dev to confirm")
	del.Set("confirm", "dev2")
	refused("wrongly confirmed", "delete", del, "Type dev to confirm")
	// brig works on the VM already.
	ts.st.AddTask(store.Task{ID: "x", VM: "airgap", Status: store.TaskRunning})
	form := values("vm", "airgap")
	form.Set("shown", "brig start airgap")
	refused("busy", "start", form, "working on airgap already")
}

func TestRunDeleteWithConfirmation(t *testing.T) {
	ts := newTestServer(t)
	ts.login()
	form := values("vm", "airgap", "confirm", "airgap")
	form.Set("shown", ts.shownBy("delete", form))
	if w := ts.post("/a/delete/run", form); w.Code != http.StatusOK {
		t.Fatalf("run: %d\n%s", w.Code, w.Body)
	}
	if s := ts.starts.started(); len(s) != 1 || s[0].Command.Display != "brig delete airgap" {
		t.Errorf("started %+v", s)
	}
}

func TestActionsNeedTheSession(t *testing.T) {
	ts := newTestServer(t)
	ts.login()
	for _, c := range []struct {
		name, origin, csrf string
		cookie             bool
	}{
		{"no cookie", "http://127.0.0.1:7878", ts.csrf, false},
		{"no origin", "", ts.csrf, true},
		{"foreign origin", "http://evil.example", ts.csrf, true},
		{"no csrf", "http://127.0.0.1:7878", "", true},
	} {
		r := httptest.NewRequest("POST", "http://127.0.0.1:7878/a/start/run", strings.NewReader("vm=airgap&shown=brig+start+airgap"))
		r.Host = "127.0.0.1:7878"
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		if c.origin != "" {
			r.Header.Set("Origin", c.origin)
		}
		if c.csrf != "" {
			r.Header.Set(csrfHeader, c.csrf)
		}
		if c.cookie {
			r.AddCookie(ts.cookie)
		}
		if w := ts.do(r); w.Code != http.StatusForbidden {
			t.Errorf("%s: %d", c.name, w.Code)
		}
	}
	// A GET never starts anything.
	if w := ts.get("/a/start/run?vm=airgap"); w.Code == http.StatusOK {
		t.Errorf("GET of run: %d", w.Code)
	}
	if n := len(ts.starts.started()); n != 0 {
		t.Errorf("%d commands started", n)
	}
}

func TestDismiss(t *testing.T) {
	ts := newTestServer(t)
	ts.login()
	end := time.Now()
	ts.st.AddTask(store.Task{ID: "done", Display: "brig stop dev", Status: store.TaskFailed, End: &end, Err: "boom"})
	ts.st.AddTask(store.Task{ID: "busy", Display: "brig sync dev", Status: store.TaskRunning})
	body := ts.get("/").Body.String()
	for _, want := range []string{`id="task-done"`, "brig stop dev", "boom", `hx-post="/tasks/done/dismiss"`, `id="task-busy"`, "Starting…"} {
		if !strings.Contains(body, want) {
			t.Errorf("the page lacks %q", want)
		}
	}
	// A running task cannot be dismissed.
	ts.post("/tasks/busy/dismiss", nil)
	if _, ok := ts.st.Snapshot().Task("busy"); !ok {
		t.Error("a running task was dismissed")
	}
	w := ts.post("/tasks/done/dismiss", nil)
	if _, ok := ts.st.Snapshot().Task("done"); ok || strings.Contains(w.Body.String(), `id="task-done"`) {
		t.Error("a finished task stays")
	}
}

func TestTaskCardShowsTheStepsOfItsRun(t *testing.T) {
	ts := newTestServer(t)
	ts.login()
	end := time.Now()
	ts.st.AddTask(store.Task{
		ID: "t", Display: "brig sync dev --dry-run", Trace: "abc", Status: store.TaskOK, End: &end, ShowOutput: true,
		Output: "profile llm unchanged\n<b>x</b>", Next: "sync", VM: "dev",
	})
	runs := ts.st.Snapshot().Activity
	ts.st.SetActivity(append([]store.Run{{ID: "r", Trace: "abc", Title: "brig sync dev", Steps: []store.Span{{Name: "Compare with the gateway", Status: "ok", Done: true}}}}, runs...))
	body := ts.get("/").Body.String()
	for _, want := range []string{"Compare with the gateway", "profile llm unchanged", "&lt;b&gt;x&lt;/b&gt;", `hx-get="/a/sync?vm=dev"`, "Sync…"} {
		if !strings.Contains(body, want) {
			t.Errorf("the task card lacks %q", want)
		}
	}
}

func TestOutputOfASuccessfulTaskStaysHidden(t *testing.T) {
	ts := newTestServer(t)
	ts.login()
	end := time.Now()
	ts.st.AddTask(store.Task{ID: "t", Display: "brig start dev", Status: store.TaskOK, End: &end, Output: "Started VM dev."})
	if strings.Contains(ts.get("/").Body.String(), "Started VM dev.") {
		t.Error("the output of a command that succeeded shows")
	}
}

func TestButtonsWaitForBusyVMs(t *testing.T) {
	ts := newTestServer(t)
	ts.login()
	body := ts.get("/vms/dev").Body.String()
	for _, want := range []string{`hx-get="/a/stop?vm=dev"`, `hx-get="/a/delete?vm=dev"`, `hx-get="/a/resources?vm=dev"`} {
		if !strings.Contains(body, want) {
			t.Errorf("the VM page lacks %q", want)
		}
	}
	if strings.Contains(body, `hx-get="/a/stop?vm=dev" hx-target="#layer" hx-swap="innerHTML" disabled`) {
		t.Error("buttons wait for nothing")
	}
	ts.st.AddTask(store.Task{ID: "t", VM: "dev", Status: store.TaskRunning})
	body = ts.get("/vms/dev").Body.String()
	if !strings.Contains(body, `hx-get="/a/stop?vm=dev" hx-target="#layer" hx-swap="innerHTML" disabled title="brig is working on dev already"`) {
		t.Error("the buttons of a VM that brig works on do not wait")
	}
	if strings.Contains(ts.get("/vms/review").Body.String(), "working on") {
		t.Error("another VM waits too")
	}
}

func TestBusyVM(t *testing.T) {
	now := time.Now()
	open := func(title string, ago time.Duration) store.Run {
		return store.Run{Title: title, Start: now.Add(-ago)}
	}
	done := now
	for _, c := range []struct {
		name string
		sn   store.Snapshot
		vm   string
		want bool
	}{
		{"nothing", store.Snapshot{}, "dev", false},
		{"task", store.Snapshot{Tasks: []store.Task{{VM: "dev"}}}, "dev", true},
		{"finished task", store.Snapshot{Tasks: []store.Task{{VM: "dev", End: &done}}}, "dev", false},
		{"task of another VM", store.Snapshot{Tasks: []store.Task{{VM: "review"}}}, "dev", false},
		{"terminal run", store.Snapshot{Activity: []store.Run{open("brig start dev", time.Minute)}}, "dev", true},
		{"update", store.Snapshot{Activity: []store.Run{open("brig update dev", time.Minute)}}, "dev", true},
		{"run of another VM", store.Snapshot{Activity: []store.Run{open("brig start review", time.Minute)}}, "dev", false},
		{"run that ended", store.Snapshot{Activity: []store.Run{{Title: "brig start dev", Start: now, End: &done}}}, "dev", false},
		{"run that never ended", store.Snapshot{Activity: []store.Run{open("brig start dev", 3*time.Hour)}}, "dev", false},
		{"command without a lock", store.Snapshot{Activity: []store.Run{open("brig image push dev", time.Minute)}}, "dev", false},
		{"image build", store.Snapshot{Activity: []store.Run{open("brig image build", time.Minute)}}, "dev", false},
	} {
		if got := busyVM(c.sn, c.vm); got != c.want {
			t.Errorf("%s: %v, want %v", c.name, got, c.want)
		}
	}
}

func TestEventsCarryTheTasks(t *testing.T) {
	ts := newTestServer(t)
	ts.login()
	for _, target := range []string{"/", "/vms", "/vms/dev", "/images", "/host", "/activity", "/nope"} {
		if body := ts.get(target).Body.String(); !strings.Contains(body, `id="tasks" sse-swap="tasks"`) || !strings.Contains(body, `<div id="layer">`) {
			t.Errorf("%s has no place for task cards and dialogs", target)
		}
		if body := ts.get(target).Body.String(); !strings.Contains(body, "sse-connect=\"/events?f=tasks&") {
			t.Errorf("%s does not listen for task cards:\n%s", target, body[:min(len(body), 900)])
		}
	}
}
