// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

package run

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dennisklein/brig/internal/openshell"
	"github.com/dennisklein/brig/internal/ui/action"
	"github.com/dennisklein/brig/internal/ui/store"
)

// fakeBrig is a shell script that behaves as its first argument says.
const fakeBrig = `#!/bin/sh
case "$1" in
ok)
	echo "args: $*"
	echo "progress=$BRIG_PROGRESS log=$BRIG_PROGRESS_LOG trace=$TRACEPARENT"
	echo "stdin: $(cat)"
	;;
fail)
	echo "working"
	echo "Error: the gateway did not answer" >&2
	echo "escape: $(printf '\033[2J')" >&2
	exit 3
	;;
big)
	i=0
	while [ $i -lt 4000 ]; do echo "line $i of the output of the build, long enough to fill the buffer"; i=$((i+1)); done
	;;
wait)
	trap 'echo interrupted; exit 130' INT
	touch "$FAKE_MARK"
	sleep 30 &
	wait
	;;
daemon)
	( sleep 3 ) &
	echo "left a daemon behind"
	;;
esac
`

func newRunner(t *testing.T) (*Runner, *store.Store) {
	t.Helper()
	dir := t.TempDir()
	exe := filepath.Join(dir, "brig")
	if err := os.WriteFile(exe, []byte(fakeBrig), 0o755); err != nil { //nolint:gosec // G306: a script to run
		t.Fatal(err)
	}
	st := store.New()
	return &Runner{Brig: exe, ActivityLog: filepath.Join(dir, "activity.jsonl"), Store: st}, st
}

func command(args ...string) action.Command {
	return action.NewCommand(append([]string{"brig"}, args...)...)
}

// await waits for the task to end.
func await(t *testing.T, st *store.Store, id string) store.Task {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if task, ok := st.Snapshot().Task(id); ok && task.Done() {
			return task
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("the task did not end")
	return store.Task{}
}

func TestRunSucceeds(t *testing.T) {
	r, st := newRunner(t)
	ended := make(chan struct{}, 1)
	r.Ended = func() { ended <- struct{}{} }
	t.Setenv("BRIG_PROGRESS", "tty")
	t.Setenv("TRACEPARENT", "00-0af7651916cd43dd8448eb211c80319c-b7ad6b7169203331-01")
	id, err := r.Start(Spec{Action: "start", Command: command("ok", "dev"), VM: "dev", Next: "sync"})
	if err != nil {
		t.Fatal(err)
	}
	task, ok := st.Snapshot().Task(id)
	if !ok || task.Display != "brig ok dev" || task.VM != "dev" || task.Action != "start" || task.Next != "sync" {
		t.Fatalf("task: %+v", task)
	}
	task = await(t, st, id)
	if task.Status != store.TaskOK || task.Err != "" {
		t.Errorf("task: %+v", task)
	}
	// Arguments arrive as given, the environment tells the child to leave
	// the display to the page and where to log, and the trace is ours.
	for _, want := range []string{
		"args: ok dev",
		"progress=none log=" + r.ActivityLog,
		"trace=00-" + task.Trace + "-",
		"stdin: \n",
	} {
		if !strings.Contains(task.Output, strings.TrimSuffix(want, "\n")) {
			t.Errorf("output lacks %q:\n%s", want, task.Output)
		}
	}
	if strings.Contains(task.Output, "0af7651916cd43dd8448eb211c80319c") || len(task.Trace) != 32 {
		t.Errorf("the trace of the environment was kept: %q\n%s", task.Trace, task.Output)
	}
	select {
	case <-ended:
	case <-time.After(5 * time.Second):
		t.Error("nobody was told that the task ended")
	}
}

func TestRunFails(t *testing.T) {
	r, st := newRunner(t)
	id, err := r.Start(Spec{Command: command("fail")})
	if err != nil {
		t.Fatal(err)
	}
	task := await(t, st, id)
	if task.Status != store.TaskFailed || task.Err != "the gateway did not answer" {
		t.Errorf("task: %+v", task)
	}
	if strings.ContainsRune(task.Output, 0x1b) || !strings.Contains(task.Output, "working") {
		t.Errorf("output: %q", task.Output)
	}
}

func TestRunKeepsTheEndOfTheOutput(t *testing.T) {
	r, st := newRunner(t)
	id, err := r.Start(Spec{Command: command("big")})
	if err != nil {
		t.Fatal(err)
	}
	task := await(t, st, id)
	if len(task.Output) > outputKept || !strings.HasSuffix(strings.TrimSpace(task.Output), "line 3999 of the output of the build, long enough to fill the buffer") {
		t.Errorf("kept %d bytes, ending %q", len(task.Output), task.Output[max(0, len(task.Output)-80):])
	}
	if !strings.HasPrefix(task.Output, "line ") {
		t.Errorf("starts in a line's middle: %q", task.Output[:40])
	}
}

func TestRunIgnoresADaemonHoldingThePipes(t *testing.T) {
	r, st := newRunner(t)
	start := time.Now()
	id, err := r.Start(Spec{Command: command("daemon")})
	if err != nil {
		t.Fatal(err)
	}
	task := await(t, st, id)
	if task.Status != store.TaskOK || time.Since(start) > 10*time.Second {
		t.Errorf("task: %+v after %v", task, time.Since(start))
	}
	// The daemon is in the child's process group, which Shutdown reaches.
}

func TestRunRemovesFinishedTasks(t *testing.T) {
	r, st := newRunner(t)
	r.KeepOK = 20 * time.Millisecond
	ok, _ := r.Start(Spec{Command: command("ok")})
	failed, _ := r.Start(Spec{Command: command("fail")})
	await(t, st, failed)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, still := st.Snapshot().Task(ok); !still {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if _, still := st.Snapshot().Task(ok); still {
		t.Error("a task that succeeded stays")
	}
	if _, still := st.Snapshot().Task(failed); !still {
		t.Error("a task that failed went away")
	}
}

func TestShutdownInterrupts(t *testing.T) {
	r, st := newRunner(t)
	mark := filepath.Join(t.TempDir(), "trap-installed")
	t.Setenv("FAKE_MARK", mark)
	id, err := r.Start(Spec{Command: command("wait")})
	if err != nil {
		t.Fatal(err)
	}
	// Interrupt only once the script has its trap.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(mark); err == nil {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	r.Shutdown(5 * time.Second)
	task := await(t, st, id)
	if task.Status != store.TaskFailed || !strings.Contains(task.Output, "interrupted") {
		t.Errorf("task: %+v", task)
	}
	if r.Running() != 0 {
		t.Error("a child still runs")
	}
}

// fakeOpenShell prints its arguments and the environment that matters.
const fakeOpenShell = `#!/bin/sh
echo "args: $*"
echo "xdg=$XDG_CONFIG_HOME gateway=$OPENSHELL_GATEWAY trace=$TRACEPARENT"
case "$1" in
fail) echo "Error: sandbox gone" >&2; exit 2 ;;
esac
`

func newOpenShellRunner(t *testing.T) (*Runner, *store.Store) {
	t.Helper()
	r, st := newRunner(t)
	dir := t.TempDir()
	exe := filepath.Join(dir, "openshell")
	if err := os.WriteFile(exe, []byte(fakeOpenShell), 0o755); err != nil { //nolint:gosec // G306: a script to run
		t.Fatal(err)
	}
	r.OpenShell = &openshell.CLI{Path: exe, ConfigHome: filepath.Join(dir, "config", "openshell")}
	return r, st
}

func TestRunOpenShell(t *testing.T) {
	r, st := newOpenShellRunner(t)
	t.Setenv("OPENSHELL_GATEWAY", "somewhere-else")
	id, err := r.Start(Spec{Action: "sandbox-delete", Command: action.NewCommand("openshell", "-g", "brig-dev", "sandbox", "delete", "webapp")})
	if err != nil {
		t.Fatal(err)
	}
	task := await(t, st, id)
	if task.Status != store.TaskOK {
		t.Fatalf("task: %+v", task)
	}
	for _, want := range []string{
		"args: -g brig-dev sandbox delete webapp",
		"xdg=" + filepath.Dir(r.OpenShell.ConfigHome) + " gateway= trace=",
	} {
		if !strings.Contains(task.Output, want) {
			t.Errorf("output lacks %q:\n%s", want, task.Output)
		}
	}
	// The activity log lists the command under the task's trace.
	log, err := os.ReadFile(r.ActivityLog)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"trace":"` + task.Trace + `"`, `"program":"openshell"`, `"name":"sandbox delete webapp"`, `"status":"ok"`} {
		if !strings.Contains(string(log), want) {
			t.Errorf("activity log lacks %s:\n%s", want, log)
		}
	}
}

func TestRunOpenShellFails(t *testing.T) {
	r, st := newOpenShellRunner(t)
	id, err := r.Start(Spec{Command: action.NewCommand("openshell", "fail")})
	if err != nil {
		t.Fatal(err)
	}
	task := await(t, st, id)
	if task.Status != store.TaskFailed || task.Err != "sandbox gone" {
		t.Errorf("task: %+v", task)
	}
	log, _ := os.ReadFile(r.ActivityLog)
	if !strings.Contains(string(log), `"status":"failed"`) {
		t.Errorf("activity log: %s", log)
	}
}

func TestRunKeepsTheNextForm(t *testing.T) {
	r, st := newOpenShellRunner(t)
	id, err := r.Start(Spec{
		Action: "allow", Command: action.NewCommand("openshell", "-g", "brig-dev", "policy", "update", "webapp"),
		Next: "policy-save-preview", NextForm: []string{"vm", "dev", "sandbox", "webapp"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if task := await(t, st, id); task.Next != "policy-save-preview" || strings.Join(task.NextForm, " ") != "vm dev sandbox webapp" {
		t.Errorf("next: %q %q", task.Next, task.NextForm)
	}
}

func TestRunOpenShellNeedsTheCLI(t *testing.T) {
	r, _ := newRunner(t)
	if _, err := r.Start(Spec{Command: action.NewCommand("openshell", "sandbox", "list")}); !errors.Is(err, openshell.ErrNotInstalled) {
		t.Errorf("err = %v", err)
	}
}

func TestStartRefusesWhatIsNotBrig(t *testing.T) {
	r, _ := newRunner(t)
	for _, argv := range [][]string{nil, {"rm", "-rf", "/"}} {
		if _, err := r.Start(Spec{Command: action.Command{Argv: argv}}); err == nil {
			t.Errorf("started %q", argv)
		}
	}
}

func TestChildEnv(t *testing.T) {
	env := childEnv([]string{"PATH=/bin", "BRIG_PROGRESS=tty", "BRIG_PROGRESS_LOG=/x", "TRACEPARENT=a", "TRACESTATE=b", "HOME=/h"}, "/log", "00-t-s-01")
	want := []string{"PATH=/bin", "HOME=/h", "BRIG_PROGRESS=none", "TRACEPARENT=00-t-s-01", "BRIG_PROGRESS_LOG=/log"}
	if strings.Join(env, "\n") != strings.Join(want, "\n") {
		t.Errorf("env %q, want %q", env, want)
	}
	if env := childEnv(nil, "", "t"); len(env) != 2 {
		t.Errorf("without a log: %q", env)
	}
}
