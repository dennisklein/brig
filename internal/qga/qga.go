// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

// Package qga speaks the protocol of the QEMU guest agent: it runs commands
// in a guest and reads and writes the guest's files. It builds the agent's
// JSON commands and reads its replies; an Agent carries them, such as
// libvirt's agent command call.
//
// The guest agent runs as root in the guest and trusts its caller. Replies
// come from the guest, so this package bounds what it keeps of them.
package qga

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// Agent sends one JSON command to a guest agent and returns its JSON reply.
// It waits at most timeout for the reply.
type Agent interface {
	AgentCommand(cmd string, timeout time.Duration) (string, error)
}

// callTimeout bounds each agent call except those that move file data.
const callTimeout = 10 * time.Second

// Chunk is the most file data that one agent call moves. libvirt passes the
// reply as one string of at most 4 MiB, which must hold the data in base64.
const Chunk = 1 << 20

type request struct {
	Execute   string `json:"execute"`
	Arguments any    `json:"arguments,omitempty"`
}

type agentError struct {
	Class string `json:"class"`
	Desc  string `json:"desc"`
}

// call runs command with args and decodes the reply's return value into
// ret, which may be nil.
func call(a Agent, timeout time.Duration, command string, args, ret any) error {
	cmd, err := json.Marshal(request{Execute: command, Arguments: args})
	if err != nil {
		return err
	}
	out, err := a.AgentCommand(string(cmd), timeout)
	if err != nil {
		return err
	}
	var reply struct {
		Return json.RawMessage `json:"return"`
		Error  *agentError     `json:"error"`
	}
	if err := json.Unmarshal([]byte(out), &reply); err != nil {
		return fmt.Errorf("%s: unreadable reply from the guest agent: %w", command, err)
	}
	if reply.Error != nil {
		return fmt.Errorf("%s: %s: %s", command, reply.Error.Class, reply.Error.Desc)
	}
	if ret == nil {
		return nil
	}
	if len(reply.Return) == 0 {
		return fmt.Errorf("%s: the guest agent's reply has no return value", command)
	}
	if err := json.Unmarshal(reply.Return, ret); err != nil {
		return fmt.Errorf("%s: unreadable reply from the guest agent: %w", command, err)
	}
	return nil
}

// Ping checks that the guest agent answers.
func Ping(a Agent) error { return call(a, 5*time.Second, "guest-ping", nil, nil) }

// WaitReady pings the guest agent until it answers or ctx ends.
func WaitReady(ctx context.Context, a Agent, every time.Duration) error {
	for {
		err := Ping(a)
		if err == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("the guest agent did not answer: %w", err)
		case <-time.After(every):
		}
	}
}

// ExecRequest is a command to run in the guest.
type ExecRequest struct {
	// Argv is the program and its arguments. The agent looks the program
	// up in its PATH if it has no slash.
	Argv []string
	// Env holds NAME=VALUE entries; empty keeps the agent's environment.
	Env []string
	// Stdin is the command's standard input.
	Stdin []byte
	// Timeout ends the wait for the command and kills it.
	Timeout time.Duration
	// MaxOutput bounds what is kept of each of stdout and stderr.
	MaxOutput int
}

// ExecResult is how a command ended and what it printed.
type ExecResult struct {
	ExitCode int `json:"exit_code"`
	// Signal is the signal that ended the command, if any.
	Signal int    `json:"signal,omitempty"`
	Stdout []byte `json:"-"`
	Stderr []byte `json:"-"`
	// Truncated says that the guest agent or MaxOutput cut the output.
	Truncated bool `json:"truncated,omitempty"`
	// TimedOut says that the command ran past the timeout and was killed.
	TimedOut bool `json:"timed_out,omitempty"`
}

type execStatus struct {
	Exited       bool   `json:"exited"`
	ExitCode     int    `json:"exitcode"`
	Signal       int    `json:"signal"`
	OutData      string `json:"out-data"`
	ErrData      string `json:"err-data"`
	OutTruncated bool   `json:"out-truncated"`
	ErrTruncated bool   `json:"err-truncated"`
}

// Exec runs a command in the guest and waits for it to end, for
// r.Timeout or until ctx ends. On a timeout or a cancelled ctx it kills the
// command, best effort, with the guest's kill program.
func Exec(ctx context.Context, a Agent, r ExecRequest) (ExecResult, error) {
	if len(r.Argv) == 0 || r.Argv[0] == "" {
		return ExecResult{}, errors.New("exec: no command")
	}
	args := map[string]any{"path": r.Argv[0], "capture-output": true}
	if len(r.Argv) > 1 {
		args["arg"] = r.Argv[1:]
	}
	if len(r.Env) > 0 {
		args["env"] = r.Env
	}
	if len(r.Stdin) > 0 {
		args["input-data"] = base64.StdEncoding.EncodeToString(r.Stdin)
	}
	var started struct {
		PID int `json:"pid"`
	}
	if err := call(a, callTimeout, "guest-exec", args, &started); err != nil {
		return ExecResult{}, err
	}
	deadline := time.NewTimer(r.Timeout)
	defer deadline.Stop()
	wait := 50 * time.Millisecond
	for {
		var st execStatus
		if err := call(a, callTimeout, "guest-exec-status", map[string]int{"pid": started.PID}, &st); err != nil {
			return ExecResult{}, err
		}
		if st.Exited {
			return result(st, r.MaxOutput)
		}
		select {
		case <-ctx.Done():
			kill(a, started.PID)
			return ExecResult{}, ctx.Err()
		case <-deadline.C:
			kill(a, started.PID)
			return ExecResult{ExitCode: -1, TimedOut: true}, nil
		case <-time.After(wait):
		}
		wait = min(2*wait, time.Second)
	}
}

func result(st execStatus, maxOutput int) (ExecResult, error) {
	res := ExecResult{ExitCode: st.ExitCode, Signal: st.Signal, Truncated: st.OutTruncated || st.ErrTruncated}
	var err error
	var cut bool
	if res.Stdout, cut, err = decode(st.OutData, maxOutput); err != nil {
		return ExecResult{}, fmt.Errorf("guest-exec-status: stdout: %w", err)
	}
	res.Truncated = res.Truncated || cut
	if res.Stderr, cut, err = decode(st.ErrData, maxOutput); err != nil {
		return ExecResult{}, fmt.Errorf("guest-exec-status: stderr: %w", err)
	}
	res.Truncated = res.Truncated || cut
	if st.Signal != 0 {
		res.ExitCode = -1
	}
	return res, nil
}

// decode decodes base64 data and keeps at most limit bytes of it.
func decode(data string, limit int) ([]byte, bool, error) {
	b, err := base64.StdEncoding.DecodeString(data)
	if err != nil {
		return nil, false, err
	}
	if limit > 0 && len(b) > limit {
		return b[:limit], true, nil
	}
	return b, false, nil
}

func kill(a Agent, pid int) {
	_ = call(a, callTimeout, "guest-exec", map[string]any{
		"path": "kill", "arg": []string{"-KILL", fmt.Sprint(pid)},
	}, nil)
}

// WriteFile writes data to the guest file path, replacing it or, with
// appendTo, appending to it. The agent creates a missing file with mode
// 0666 less the agent's umask, owned by root.
func WriteFile(a Agent, path string, data []byte, appendTo bool) error {
	mode := "w"
	if appendTo {
		mode = "a"
	}
	h, err := open(a, path, mode)
	if err != nil {
		return err
	}
	for len(data) > 0 {
		n := min(len(data), Chunk)
		var w struct {
			Count int `json:"count"`
		}
		args := map[string]any{"handle": h, "buf-b64": base64.StdEncoding.EncodeToString(data[:n])}
		if err := call(a, time.Minute, "guest-file-write", args, &w); err != nil {
			_ = closeFile(a, h)
			return fmt.Errorf("write %s: %w", path, err)
		}
		if w.Count <= 0 || w.Count > n {
			_ = closeFile(a, h)
			return fmt.Errorf("write %s: the guest agent wrote %d of %d bytes", path, w.Count, n)
		}
		data = data[w.Count:]
	}
	if err := closeFile(a, h); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}

// ReadFile reads at most limit bytes of the guest file path from offset on.
// It reports whether it reached the end of the file.
func ReadFile(a Agent, path string, offset int64, limit int) ([]byte, bool, error) {
	if offset < 0 || limit <= 0 {
		return nil, false, fmt.Errorf("read %s: invalid offset %d or limit %d", path, offset, limit)
	}
	h, err := open(a, path, "r")
	if err != nil {
		return nil, false, err
	}
	defer func() { _ = closeFile(a, h) }()
	if offset > 0 {
		var pos struct {
			Position int64 `json:"position"`
		}
		if err := call(a, callTimeout, "guest-file-seek", map[string]any{"handle": h, "offset": offset, "whence": "set"}, &pos); err != nil {
			return nil, false, fmt.Errorf("read %s: %w", path, err)
		}
	}
	var out []byte
	for len(out) < limit {
		want := min(limit-len(out), Chunk)
		var r struct {
			Count int    `json:"count"`
			Buf   string `json:"buf-b64"`
			EOF   bool   `json:"eof"`
		}
		if err := call(a, time.Minute, "guest-file-read", map[string]any{"handle": h, "count": want}, &r); err != nil {
			return nil, false, fmt.Errorf("read %s: %w", path, err)
		}
		b, err := base64.StdEncoding.DecodeString(r.Buf)
		if err != nil {
			return nil, false, fmt.Errorf("read %s: %w", path, err)
		}
		if len(b) > want {
			return nil, false, fmt.Errorf("read %s: the guest agent sent %d bytes, asked for %d", path, len(b), want)
		}
		out = append(out, b...)
		if r.EOF {
			return out, true, nil
		}
		if len(b) == 0 {
			// No data and no EOF: the agent would spin forever.
			return out, false, nil
		}
	}
	return out, false, nil
}

func open(a Agent, path, mode string) (int, error) {
	if path == "" {
		return 0, errors.New("no file path")
	}
	var h int
	if err := call(a, callTimeout, "guest-file-open", map[string]string{"path": path, "mode": mode}, &h); err != nil {
		return 0, fmt.Errorf("open %s: %w", path, err)
	}
	return h, nil
}

func closeFile(a Agent, h int) error {
	return call(a, callTimeout, "guest-file-close", map[string]int{"handle": h}, nil)
}
