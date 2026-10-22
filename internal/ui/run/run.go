// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

// Package run starts the brig and openshell commands that brig ui's dialogs
// make, and keeps their tasks in the store while they run.
//
// A task's command is a child process of its own, in its own process group
// with no input. The progress of a brig command comes from the activity log,
// where the child writes the same events that make a terminal's progress
// display: the runner hands the child a trace ID in TRACEPARENT, and the page
// finds the run with that trace. An openshell command writes no events, so
// the runner writes one span for it under that trace.
package run

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/GSI-HPC/go-clikit/termtext"

	"github.com/dennisklein/brig/internal/openshell"
	"github.com/dennisklein/brig/internal/ui/action"
	"github.com/dennisklein/brig/internal/ui/store"
)

const (
	// outputKept is how much of the end of a command's output a task
	// keeps, in bytes.
	outputKept = 32 << 10
	// outputEvery is how often a running task's output is published.
	outputEvery = 500 * time.Millisecond
)

// ErrClosing is the error of Start once Shutdown has begun.
var ErrClosing = errors.New("brig ui is shutting down")

// Spec describes a command to run as a task.
type Spec struct {
	// Action is the ID of the action that built the command.
	Action  string
	Command action.Command
	// VM is the VM the command works on, if any.
	VM string
	// ShowOutput and Next are those of the action.
	ShowOutput bool
	Next       string
	// NextForm fills the fields of Next's dialog, as key and value pairs.
	NextForm []string
}

// Starter starts tasks. The runner executes them; the demo pretends to.
type Starter interface {
	// Start starts the command in the background and returns the ID of its
	// task, which the store holds from now on.
	Start(s Spec) (string, error)
	// Running returns how many tasks run.
	Running() int
	// Shutdown asks running tasks to end and waits for them, up to grace.
	Shutdown(grace time.Duration)
}

// Runner executes tasks as child processes.
type Runner struct {
	// Brig is the brig executable, which runs in place of "brig" in a
	// command.
	Brig string
	// OpenShell is the openshell CLI, which runs in place of "openshell" in
	// a command; nil if it is not installed.
	OpenShell *openshell.CLI
	// ActivityLog is the event log that the children append to.
	ActivityLog string
	Store       *store.Store
	// Ended is called when a task has ended, for the collectors to look
	// again at what the command changed.
	Ended func()
	// KeepOK is how long a task that succeeded stays in the store.
	KeepOK time.Duration

	mu      sync.Mutex
	running map[string]*child
	// closing is set once Shutdown has begun: no task starts after it.
	closing bool
	wg      sync.WaitGroup
}

// child is a running task's process.
type child struct {
	cmd *exec.Cmd
	// interrupted is set once the child got its SIGINT. brig takes the
	// first SIGINT as Ctrl-C and rolls back; a second one would kill it in
	// the middle of the rollback, so it never gets one.
	interrupted bool
}

// Start implements Starter.
func (r *Runner) Start(s Spec) (string, error) {
	if len(s.Command.Argv) == 0 {
		return "", errors.New("no command to run")
	}
	r.mu.Lock()
	if r.closing {
		r.mu.Unlock()
		return "", ErrClosing
	}
	// Shutdown waits for this task from now on, also while it starts.
	r.wg.Add(1)
	r.mu.Unlock()
	id, err := r.start(s)
	if err != nil {
		r.wg.Done()
	}
	return id, err
}

// start starts the child of a task that Start has counted in the wait
// group, and the goroutine that follows it, which ends the count.
func (r *Runner) start(s Spec) (string, error) {
	trace, span := randomHex(16), randomHex(8)
	var cmd *exec.Cmd
	var endSpan func(error)
	switch s.Command.Argv[0] {
	case "brig":
		cmd = exec.Command(r.Brig, s.Command.Argv[1:]...)
		cmd.Env = childEnv(os.Environ(), r.ActivityLog, "00-"+trace+"-"+span+"-01")
	case "openshell":
		if r.OpenShell == nil {
			return "", openshell.ErrNotInstalled
		}
		env, err := r.OpenShell.Environ(os.Environ(), nil)
		if err != nil {
			return "", err
		}
		cmd = exec.Command(r.OpenShell.Path, s.Command.Argv[1:]...)
		cmd.Env = env
	default:
		return "", errors.New("not a brig or openshell command")
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	// A daemon that brig starts may keep the output pipes open after brig
	// ended; its output is not worth waiting for.
	cmd.WaitDelay = 2 * time.Second
	tail := &tailBuffer{max: outputKept}
	cmd.Stdout, cmd.Stderr = tail, tail
	if err := cmd.Start(); err != nil {
		return "", fmt.Errorf("could not start %s: %w", s.Command.Argv[0], err)
	}
	if s.Command.Argv[0] == "openshell" {
		endSpan = openSpan(r.ActivityLog, trace, span, s.Command.Argv)
	}
	t := store.Task{
		ID: randomHex(8), Action: s.Action, Display: s.Command.Display, VM: s.VM, Trace: trace,
		Start: time.Now(), Status: store.TaskRunning, ShowOutput: s.ShowOutput, Next: s.Next, NextForm: s.NextForm,
	}
	r.Store.AddTask(t)
	r.mu.Lock()
	if r.running == nil {
		r.running = map[string]*child{}
	}
	late := r.closing
	r.running[t.ID] = &child{cmd: cmd, interrupted: late}
	r.mu.Unlock()
	if late {
		// Shutdown began while the child started.
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGINT)
	}
	go r.wait(t.ID, s.Command.Argv[0], cmd, tail, endSpan)
	return t.ID, nil
}

// wait follows the child until it ends, publishing its output on the way, and
// records how it ended.
func (r *Runner) wait(id, program string, cmd *exec.Cmd, tail *tailBuffer, endSpan func(error)) {
	defer r.wg.Done()
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	tick := time.NewTicker(outputEvery)
	defer tick.Stop()
	shown := ""
	var err error
loop:
	for {
		select {
		case err = <-done:
			break loop
		case <-tick.C:
			if out := tail.escaped(); out != shown {
				shown = out
				r.Store.UpdateTask(id, store.TopicOutput, func(t *store.Task) { t.Output = out })
			}
		}
	}
	if errors.Is(err, exec.ErrWaitDelay) {
		err = nil
	}
	r.mu.Lock()
	delete(r.running, id)
	r.mu.Unlock()
	if endSpan != nil {
		endSpan(err)
	}
	out := tail.escaped()
	end := time.Now()
	r.Store.UpdateTask(id, store.TopicTasks, func(t *store.Task) {
		t.End, t.Output = &end, out
		if err == nil {
			t.Status = store.TaskOK
			return
		}
		t.Status, t.Err = store.TaskFailed, failure(program, out, err)
	})
	if r.Ended != nil {
		r.Ended()
	}
	if err == nil && r.KeepOK > 0 {
		time.AfterFunc(r.KeepOK, func() { r.Store.RemoveTask(id) })
	}
}

// Shutdown implements Starter: it interrupts the children, so that brig
// rolls back and cleans up as it does for Ctrl-C, and kills those that have
// not ended after grace.
func (r *Runner) Shutdown(grace time.Duration) {
	r.interruptAll()
	done := make(chan struct{})
	go func() { r.wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(grace):
		r.kill()
		<-done
	}
}

// interruptAll stops new tasks and sends SIGINT to the process group of
// every running child that has not had one yet.
func (r *Runner) interruptAll() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.closing = true
	for _, c := range r.running {
		if !c.interrupted {
			c.interrupted = true
			_ = syscall.Kill(-c.cmd.Process.Pid, syscall.SIGINT)
		}
	}
}

// kill sends SIGKILL to the process group of every running child.
func (r *Runner) kill() {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, c := range r.running {
		_ = syscall.Kill(-c.cmd.Process.Pid, syscall.SIGKILL)
	}
}

// Running returns how many children run.
func (r *Runner) Running() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.running)
}

// childEnv returns env for a brig child: no progress display, the event log
// brig ui reads, and the trace that tells the child's run. The environment
// does not change the command line, so what the dialog showed still holds.
func childEnv(env []string, activityLog, traceparent string) []string {
	out := make([]string, 0, len(env)+3)
	for _, kv := range env {
		switch name, _, _ := strings.Cut(kv, "="); name {
		case "BRIG_PROGRESS", "BRIG_PROGRESS_LOG", "TRACEPARENT", "TRACESTATE":
		default:
			out = append(out, kv)
		}
	}
	out = append(out, "BRIG_PROGRESS=none", "TRACEPARENT="+traceparent)
	if activityLog != "" {
		out = append(out, "BRIG_PROGRESS_LOG="+activityLog)
	}
	return out
}

// failure says in one line why a command failed: the program's error line if
// it printed one, else how the process ended.
func failure(program, out string, err error) string {
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		msg, ok := strings.CutPrefix(strings.TrimSpace(lines[i]), "Error:")
		if !ok {
			continue
		}
		// The openshell CLI writes "Error:   × what failed", then a line per
		// cause.
		msg = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(msg), "×"))
		for _, l := range lines[i+1:] {
			l = strings.TrimSpace(l)
			for _, arrow := range []string{"├─▶", "╰─▶", "|->", "`->"} {
				if c, ok := strings.CutPrefix(l, arrow); ok {
					msg += ": " + strings.TrimSpace(c)
				}
			}
		}
		if msg != "" {
			return msg
		}
	}
	if ee, ok := errors.AsType[*exec.ExitError](err); ok {
		return program + " " + ee.String()
	}
	return err.Error()
}

func randomHex(n int) string {
	b := make([]byte, n)
	// crypto/rand never fails; it ends the process when it cannot.
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// tailBuffer keeps the end of what is written to it.
type tailBuffer struct {
	max int
	mu  sync.Mutex
	buf []byte
	cut bool
}

func (b *tailBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.buf = append(b.buf, p...)
	if over := len(b.buf) - b.max; over > 0 {
		b.buf, b.cut = append(b.buf[:0], b.buf[over:]...), true
	}
	return len(p), nil
}

// escaped returns the kept output with control characters escaped, from the
// start of its first whole line when the start was cut off.
func (b *tailBuffer) escaped() string {
	b.mu.Lock()
	buf := bytes.Clone(b.buf)
	cut := b.cut
	b.mu.Unlock()
	if cut {
		if _, rest, ok := bytes.Cut(buf, []byte("\n")); ok {
			buf = rest
		}
	}
	return termtext.EscapeLines(string(buf))
}
