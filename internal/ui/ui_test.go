// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

package ui

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"
	"time"
)

// Run returns when it fails after it started its goroutines. It used to
// wait for goroutines that only its own cancel could end.
func TestRunReturnsOnError(t *testing.T) {
	done := make(chan error, 1)
	go func() {
		var out bytes.Buffer
		done <- Run(context.Background(), Options{
			Demo: true, NoOpen: true, DevAssets: filepath.Join(t.TempDir(), "missing"),
			Stdout: &out, Stderr: &out,
		})
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Error("Run succeeded with assets that are missing")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not return after an error")
	}
}

// listener serves on a free port of 127.0.0.1 until the test ends, and
// returns that port.
func listener(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.Close() })
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			_ = c.Close()
		}
	}()
	return l.Addr().(*net.TCPAddr).Port
}

func TestRunning(t *testing.T) {
	port := listener(t)
	good := "http://127.0.0.1:" + strconv.Itoa(port) + "/?token=abc"
	write := func(name string, r runtimeRecord, mode os.FileMode) string {
		t.Helper()
		path := filepath.Join(t.TempDir(), name)
		data, _ := json.Marshal(r)
		if err := os.WriteFile(path, data, mode); err != nil {
			t.Fatal(err)
		}
		return path
	}
	self := os.Getpid()
	if r, ok := running(write("ui.json", runtimeRecord{URL: good, PID: self}, 0o600)); !ok || r.URL != good {
		t.Errorf("a good record: %+v, %v", r, ok)
	}

	for _, c := range []struct {
		name string
		rec  runtimeRecord
		mode os.FileMode
	}{
		{"readable by others", runtimeRecord{URL: good, PID: self}, 0o644},
		{"no process", runtimeRecord{URL: good, PID: 0}, 0o600},
		{"another host", runtimeRecord{URL: "http://evil.example:" + strconv.Itoa(port) + "/?token=abc", PID: self}, 0o600},
		{"another scheme", runtimeRecord{URL: "file:///etc/passwd", PID: self}, 0o600},
		{"credentials", runtimeRecord{URL: "http://127.0.0.1:" + strconv.Itoa(port) + "@evil.example/?token=abc", PID: self}, 0o600},
		{"another path", runtimeRecord{URL: "http://127.0.0.1:" + strconv.Itoa(port) + "/x?token=abc", PID: self}, 0o600},
		{"no token", runtimeRecord{URL: "http://127.0.0.1:" + strconv.Itoa(port) + "/", PID: self}, 0o600},
		{"more query", runtimeRecord{URL: good + "&x=1", PID: self}, 0o600},
	} {
		if _, ok := running(write("ui.json", c.rec, c.mode)); ok {
			t.Errorf("%s: record accepted", c.name)
		}
	}

	// A link to a record, and a record that is not a file, do not count.
	dir := t.TempDir()
	target := write("target.json", runtimeRecord{URL: good, PID: self}, 0o600)
	link := filepath.Join(dir, "ui.json")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if _, ok := running(link); ok {
		t.Error("a symlink to a record was accepted")
	}
	fifo := filepath.Join(dir, "fifo.json")
	if err := syscallMkfifo(fifo); err != nil {
		t.Fatal(err)
	}
	if _, ok := running(fifo); ok {
		t.Error("a FIFO was accepted")
	}
}

func syscallMkfifo(path string) error { return syscall.Mkfifo(path, 0o600) }
