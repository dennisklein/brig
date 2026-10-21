// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

package qga

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeAgent is a guest agent with an in-memory file system and scripted
// commands.
type fakeAgent struct {
	mu    sync.Mutex
	calls []string
	files map[string][]byte
	// handles maps open handles to path, mode and offset.
	handles map[int]*handle
	next    int
	// polls is the number of guest-exec-status calls before the command
	// exits; negative never exits.
	polls  int
	status execStatus
	killed []string
	// down makes every call fail, as when the agent is not running yet.
	down int
	// short makes reads return no data without EOF.
	short bool
}

type handle struct {
	path, mode string
	off        int
}

func b64(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }

func (f *fakeAgent) AgentCommand(cmd string, _ time.Duration) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var req struct {
		Execute   string          `json:"execute"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if err := json.Unmarshal([]byte(cmd), &req); err != nil {
		return "", err
	}
	f.calls = append(f.calls, req.Execute)
	if f.down > 0 {
		f.down--
		return "", errors.New("guest agent is not connected")
	}
	var a map[string]any
	_ = json.Unmarshal(req.Arguments, &a)
	ret := func(v any) (string, error) {
		b, _ := json.Marshal(map[string]any{"return": v})
		return string(b), nil
	}
	switch req.Execute {
	case "guest-ping":
		return ret(struct{}{})
	case "guest-exec":
		if a["path"] == "kill" {
			f.killed = append(f.killed, fmt.Sprint(a["arg"]))
			return ret(map[string]int{"pid": 2})
		}
		if a["path"] == "missing" {
			return `{"error":{"class":"GenericError","desc":"Failed to execute child process"}}`, nil
		}
		return ret(map[string]int{"pid": 1})
	case "guest-exec-status":
		if f.polls == 0 {
			st := f.status
			st.Exited = true
			return ret(st)
		}
		f.polls--
		return ret(map[string]bool{"exited": false})
	case "guest-file-open":
		path, mode := a["path"].(string), a["mode"].(string)
		if mode == "r" {
			if _, ok := f.files[path]; !ok {
				return `{"error":{"class":"GenericError","desc":"No such file or directory"}}`, nil
			}
		}
		if mode == "w" {
			f.files[path] = nil
		}
		f.next++
		f.handles[f.next] = &handle{path: path, mode: mode}
		return ret(f.next)
	case "guest-file-write":
		h := f.handles[int(a["handle"].(float64))]
		b, _ := base64.StdEncoding.DecodeString(a["buf-b64"].(string))
		f.files[h.path] = append(f.files[h.path], b...)
		return ret(map[string]any{"count": len(b), "eof": false})
	case "guest-file-seek":
		h := f.handles[int(a["handle"].(float64))]
		h.off = int(a["offset"].(float64))
		return ret(map[string]any{"position": h.off, "eof": false})
	case "guest-file-read":
		h := f.handles[int(a["handle"].(float64))]
		if f.short {
			return ret(map[string]any{"count": 0, "buf-b64": "", "eof": false})
		}
		data := f.files[h.path]
		end := min(h.off+int(a["count"].(float64)), len(data))
		chunk := data[min(h.off, len(data)):end]
		h.off = end
		return ret(map[string]any{"count": len(chunk), "buf-b64": base64.StdEncoding.EncodeToString(chunk), "eof": end == len(data)})
	case "guest-file-close":
		delete(f.handles, int(a["handle"].(float64)))
		return ret(struct{}{})
	}
	return "", fmt.Errorf("unknown command %s", req.Execute)
}

func newAgent() *fakeAgent {
	return &fakeAgent{files: map[string][]byte{}, handles: map[int]*handle{}}
}

func TestPingAndWaitReady(t *testing.T) {
	a := newAgent()
	a.down = 2
	if err := Ping(a); err == nil {
		t.Error("Ping succeeded while the agent is down")
	}
	if err := WaitReady(context.Background(), a, time.Millisecond); err != nil {
		t.Errorf("WaitReady = %v", err)
	}
	a.down = 1000
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := WaitReady(ctx, a, time.Millisecond); err == nil || !strings.Contains(err.Error(), "did not answer") {
		t.Errorf("WaitReady = %v, want a timeout", err)
	}
}

func TestExec(t *testing.T) {
	a := newAgent()
	a.polls = 2
	a.status = execStatus{ExitCode: 3, OutData: b64("hello\n"), ErrData: b64("oops\n")}
	res, err := Exec(context.Background(), a, ExecRequest{Argv: []string{"echo", "hello"}, Stdin: []byte("in"), Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if res.ExitCode != 3 || string(res.Stdout) != "hello\n" || string(res.Stderr) != "oops\n" || res.Truncated || res.TimedOut {
		t.Errorf("result = %+v %q %q", res, res.Stdout, res.Stderr)
	}
	if got := strings.Join(a.calls, " "); got != "guest-exec guest-exec-status guest-exec-status guest-exec-status" {
		t.Errorf("calls = %s", got)
	}
}

func TestExecTruncatesAndSignals(t *testing.T) {
	a := newAgent()
	a.status = execStatus{Signal: 9, OutData: b64("0123456789"), OutTruncated: false}
	res, err := Exec(context.Background(), a, ExecRequest{Argv: []string{"x"}, Timeout: time.Second, MaxOutput: 4})
	if err != nil {
		t.Fatal(err)
	}
	if string(res.Stdout) != "0123" || !res.Truncated || res.ExitCode != -1 || res.Signal != 9 {
		t.Errorf("result = %+v %q", res, res.Stdout)
	}
}

func TestExecTimeoutKills(t *testing.T) {
	a := newAgent()
	a.polls = -1
	res, err := Exec(context.Background(), a, ExecRequest{Argv: []string{"sleep", "100"}, Timeout: 30 * time.Millisecond})
	if err != nil || !res.TimedOut {
		t.Fatalf("Exec = %+v, %v; want a timeout", res, err)
	}
	if len(a.killed) != 1 || a.killed[0] != "[-KILL 1]" {
		t.Errorf("killed = %v", a.killed)
	}
}

func TestExecErrors(t *testing.T) {
	a := newAgent()
	if _, err := Exec(context.Background(), a, ExecRequest{Timeout: time.Second}); err == nil {
		t.Error("Exec without a command succeeded")
	}
	if _, err := Exec(context.Background(), a, ExecRequest{Argv: []string{"missing"}, Timeout: time.Second}); err == nil ||
		!strings.Contains(err.Error(), "Failed to execute") {
		t.Errorf("Exec(missing) = %v", err)
	}
	a.status = execStatus{OutData: "%%%"}
	if _, err := Exec(context.Background(), a, ExecRequest{Argv: []string{"x"}, Timeout: time.Second}); err == nil {
		t.Error("Exec accepted bad base64 output")
	}
}

func TestFiles(t *testing.T) {
	a := newAgent()
	big := bytes.Repeat([]byte("0123456789abcdef"), (Chunk/16)*2+100)
	if err := WriteFile(a, "/tmp/app.rpm", big[:Chunk+5], false); err != nil {
		t.Fatal(err)
	}
	if err := WriteFile(a, "/tmp/app.rpm", big[Chunk+5:], true); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(a.files["/tmp/app.rpm"], big) {
		t.Fatalf("file has %d bytes, want %d", len(a.files["/tmp/app.rpm"]), len(big))
	}
	got, eof, err := ReadFile(a, "/tmp/app.rpm", 0, len(big)+10)
	if err != nil || !eof || !bytes.Equal(got, big) {
		t.Errorf("ReadFile = %d bytes, eof %v, %v", len(got), eof, err)
	}
	got, eof, err = ReadFile(a, "/tmp/app.rpm", 10, 6)
	if err != nil || eof || string(got) != "abcdef" {
		t.Errorf("ReadFile at 10 = %q, eof %v, %v", got, eof, err)
	}
	if len(a.handles) != 0 {
		t.Errorf("%d handles left open", len(a.handles))
	}
	if _, _, err := ReadFile(a, "/nope", 0, 10); err == nil {
		t.Error("ReadFile of a missing file succeeded")
	}
	if _, _, err := ReadFile(a, "/tmp/app.rpm", -1, 10); err == nil {
		t.Error("ReadFile at a negative offset succeeded")
	}
	if err := WriteFile(a, "", nil, false); err == nil {
		t.Error("WriteFile without a path succeeded")
	}
}

func TestReadFileStopsOnEmptyRead(t *testing.T) {
	a := newAgent()
	a.files["/f"] = []byte("data")
	a.short = true
	got, eof, err := ReadFile(a, "/f", 0, 10)
	if err != nil || eof || len(got) != 0 {
		t.Errorf("ReadFile = %q, %v, %v", got, eof, err)
	}
}
