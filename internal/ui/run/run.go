// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

// Package run starts the brig commands that brig ui's dialogs make, and
// keeps their tasks in the store while they run.
//
// A task's command is a child process of its own, in its own process group
// with no input. Its progress comes from the activity log, where the child
// writes the same events that make a terminal's progress display: the runner
// hands the child a trace ID in TRACEPARENT, and the page finds the run with
// that trace.
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
	// ActivityLog is the event log that the children append to.
	ActivityLog string
	Store       *store.Store
	// Ended is called when a task has ended, for the collectors to look
	// again at what the command changed.
	Ended func()
	// KeepOK is how long a task that succeeded stays in the store.
	KeepOK time.Duration

	mu      sync.Mutex
	running map[string]*exec.Cmd
	wg      sync.WaitGroup
}

// Start implements Starter.
func (r *Runner) Start(s Spec) (string, error) {
	if len(s.Command.Argv) == 0 || s.Command.Argv[0] != "brig" {
		return "", errors.New("not a brig command")
	}
	trace, span := randomHex(16), randomHex(8)
	cmd := exec.Command(r.Brig, s.Command.Argv[1:]...)
	cmd.Env = childEnv(os.Environ(), r.ActivityLog, "00-"+trace+"-"+span+"-01")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	// A daemon that brig starts may keep the output pipes open after brig
	// ended; its output is not worth waiting for.
	cmd.WaitDelay = 2 * time.Second
	tail := &tailBuffer{max: outputKept}
	cmd.Stdout, cmd.Stderr = tail, tail
	if err := cmd.Start(); err != nil {
		return "", fmt.Errorf("could not start brig: %w", err)
	}
	t := store.Task{
		ID: randomHex(8), Action: s.Action, Display: s.Command.Display, VM: s.VM, Trace: trace,
		Start: time.Now(), Status: store.TaskRunning, ShowOutput: s.ShowOutput, Next: s.Next,
	}
	r.Store.AddTask(t)
	r.mu.Lock()
	if r.running == nil {
		r.running = map[string]*exec.Cmd{}
	}
	r.running[t.ID] = cmd
	r.mu.Unlock()
	r.wg.Add(1)
	go r.wait(t.ID, cmd, tail)
	return t.ID, nil
}

// wait follows the child until it ends, publishing its output on the way, and
// records how it ended.
func (r *Runner) wait(id string, cmd *exec.Cmd, tail *tailBuffer) {
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
	out := tail.escaped()
	end := time.Now()
	r.Store.UpdateTask(id, store.TopicTasks, func(t *store.Task) {
		t.End, t.Output = &end, out
		if err == nil {
			t.Status = store.TaskOK
			return
		}
		t.Status, t.Err = store.TaskFailed, failure(out, err)
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
	r.signal(syscall.SIGINT)
	done := make(chan struct{})
	go func() { r.wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(grace):
		r.signal(syscall.SIGKILL)
		<-done
	}
}

// signal sends sig to the process group of every running child.
func (r *Runner) signal(sig syscall.Signal) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, cmd := range r.running {
		_ = syscall.Kill(-cmd.Process.Pid, sig)
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

// failure says in one line why a command failed: brig's error line if it
// printed one, else how the process ended.
func failure(out string, err error) string {
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if msg, ok := strings.CutPrefix(lines[i], "Error: "); ok {
			return msg
		}
	}
	if ee, ok := errors.AsType[*exec.ExitError](err); ok {
		return "brig " + ee.String()
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
