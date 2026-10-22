// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

package ui

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
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

// fakeProc writes a /proc whose net/tcp lists a socket at port in state,
// owned by uid and with an inode.
func fakeProc(t *testing.T, port, uid int) string {
	t.Helper()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "net"), 0o750); err != nil {
		t.Fatal(err)
	}
	table := "  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode\n" +
		fmt.Sprintf("   0: 0100007F:%04X 00000000:0000 0A 00000000:00000000 00:00000000 00000000 %5d        0 101 1 0000000000000000 100 0 0 10 0\n", port, uid)
	if err := os.WriteFile(filepath.Join(root, "net", "tcp"), []byte(table), 0o600); err != nil {
		t.Fatal(err)
	}
	return root
}

func TestRunning(t *testing.T) {
	port := listener(t)
	good := "http://127.0.0.1:" + strconv.Itoa(port) + "/?token=abc"
	self := os.Getpid()
	newGuard := func() *instance {
		t.Helper()
		return &instance{dir: t.TempDir(), procRoot: fakeProc(t, port, os.Getuid())}
	}
	write := func(i *instance, r runtimeRecord, mode os.FileMode) {
		t.Helper()
		data, _ := json.Marshal(r)
		if err := os.WriteFile(i.recordPath(), data, mode); err != nil {
			t.Fatal(err)
		}
	}
	i := newGuard()
	write(i, runtimeRecord{URL: good, PID: self}, 0o600)
	if r, ok := i.running(); !ok || r.URL != good {
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
		i := newGuard()
		write(i, c.rec, c.mode)
		if _, ok := i.running(); ok {
			t.Errorf("%s: record accepted", c.name)
		}
	}

	// A listener of another user, or none in the table, is not a brig ui of
	// the user, whatever the record says.
	i = newGuard()
	write(i, runtimeRecord{URL: good, PID: self}, 0o600)
	i.procRoot = fakeProc(t, port, os.Getuid()+1)
	if _, ok := i.running(); ok {
		t.Error("a listener of another user was accepted")
	}
	i.procRoot = fakeProc(t, port+1, os.Getuid())
	if _, ok := i.running(); ok {
		t.Error("a port without a listener in the table was accepted")
	}
	i.procRoot = filepath.Join(t.TempDir(), "missing")
	if _, ok := i.running(); ok {
		t.Error("a missing table was accepted")
	}

	// A link to a record, and a record that is not a file, do not count.
	i = newGuard()
	target := filepath.Join(t.TempDir(), "target.json")
	data, _ := json.Marshal(runtimeRecord{URL: good, PID: self})
	if err := os.WriteFile(target, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, i.recordPath()); err != nil {
		t.Fatal(err)
	}
	if _, ok := i.running(); ok {
		t.Error("a symlink to a record was accepted")
	}
	i = newGuard()
	if err := syscallMkfifo(i.recordPath()); err != nil {
		t.Fatal(err)
	}
	if _, ok := i.running(); ok {
		t.Error("a FIFO was accepted")
	}
}

func TestNewInstance(t *testing.T) {
	runtime := t.TempDir()
	i, err := newInstance(runtime)
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(runtime, "brig"); i.dir != want {
		t.Errorf("dir = %s, want %s", i.dir, want)
	}
	if err := os.Chmod(i.dir, 0o750); err != nil { //nolint:gosec // G302 targets files; directories need the execute bit
		t.Fatal(err)
	}
	if _, err := newInstance(runtime); err == nil || !strings.Contains(err.Error(), "must be 0700") {
		t.Errorf("a directory with mode 0750: %v", err)
	}

	// Somebody else's directory, or a link to one, is refused.
	other := t.TempDir()
	if err := os.Mkdir(filepath.Join(other, "brig"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := checkPrivateDir(filepath.Join(other, "brig"), os.Getuid()+1); err == nil || !strings.Contains(err.Error(), "another user") {
		t.Errorf("a directory of another user: %v", err)
	}
	linked := t.TempDir()
	if err := os.Symlink(filepath.Join(other, "brig"), filepath.Join(linked, "brig")); err != nil {
		t.Fatal(err)
	}
	if _, err := newInstance(linked); err == nil || !strings.Contains(err.Error(), "no directory") {
		t.Errorf("a link to a directory: %v", err)
	}
	file := t.TempDir()
	if err := os.WriteFile(filepath.Join(file, "brig"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := newInstance(file); err == nil {
		t.Error("a file in place of the directory")
	}
	if _, err := newInstance(filepath.Join(file, "brig", "x")); err == nil {
		t.Error("a runtime directory below a file")
	}
}

func TestLock(t *testing.T) {
	dir := t.TempDir()
	first := &instance{dir: dir, flock: unix.Flock}
	if err := first.lock(); err != nil {
		t.Fatal(err)
	}
	second := &instance{dir: dir, flock: unix.Flock}
	locked := make(chan struct{})
	go func() {
		if err := second.lock(); err != nil {
			t.Error(err)
		}
		close(locked)
	}()
	select {
	case <-locked:
		t.Fatal("two starts held the lock at once")
	case <-time.After(100 * time.Millisecond):
	}
	first.unlock()
	select {
	case <-locked:
	case <-time.After(5 * time.Second):
		t.Fatal("the second start never got the lock")
	}
	second.unlock()
	second.unlock() // dropping a lock twice does nothing

	boom := errors.New("boom")
	failing := &instance{dir: dir, flock: func(int, int) error { return boom }}
	if err := failing.lock(); !errors.Is(err, boom) {
		t.Errorf("a failing flock: %v", err)
	}

	// The lock file is not followed to somewhere else.
	linked := t.TempDir()
	if err := os.Symlink(filepath.Join(dir, "elsewhere"), filepath.Join(linked, "ui.lock")); err != nil {
		t.Fatal(err)
	}
	if err := (&instance{dir: linked, flock: unix.Flock}).lock(); err == nil {
		t.Error("locked through a symbolic link")
	}
}

func TestPublishAndRetire(t *testing.T) {
	i := &instance{dir: t.TempDir()}
	if err := i.publish(runtimeRecord{URL: "http://127.0.0.1:1/?token=a", PID: 7}); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(i.recordPath())
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("record: %v, %v; want mode 0600", fi, err)
	}
	i.retire()
	if _, err := os.Stat(i.recordPath()); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("record after retire: %v", err)
	}
	if err := (&instance{dir: filepath.Join(i.dir, "missing")}).publish(runtimeRecord{}); err == nil {
		t.Error("published into a directory that is not there")
	}
}

func TestOpenBrowserWritesAPrivatePage(t *testing.T) {
	t.Setenv("PATH", t.TempDir()) // no xdg-open: the page is all that is left
	dir := t.TempDir()
	var logs bytes.Buffer
	openBrowser(dir, "http://127.0.0.1:1/?token=secret", slog.New(slog.NewTextHandler(&logs, nil)))
	fi, err := os.Stat(filepath.Join(dir, "ui-open.html"))
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("page: %v, %v; want mode 0600", fi, err)
	}
	if !strings.Contains(logs.String(), "could not open a browser") {
		t.Errorf("log: %q", logs.String())
	}

	// Without a place for the page no browser starts: the token would stand
	// in xdg-open's command line.
	logs.Reset()
	bin := t.TempDir()
	marker := filepath.Join(bin, "ran")
	script := "#!/bin/sh\ntouch " + marker + "\n"
	if err := os.WriteFile(filepath.Join(bin, "xdg-open"), []byte(script), 0o755); err != nil { //nolint:gosec // G306: a script to run
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	openBrowser(filepath.Join(dir, "missing"), "http://127.0.0.1:1/?token=secret", slog.New(slog.NewTextHandler(&logs, nil)))
	time.Sleep(100 * time.Millisecond)
	if _, err := os.Stat(marker); err == nil {
		t.Error("xdg-open ran without a page")
	}
	if !strings.Contains(logs.String(), "could not write the page") {
		t.Errorf("log: %q", logs.String())
	}
}

func syscallMkfifo(path string) error { return syscall.Mkfifo(path, 0o600) }
