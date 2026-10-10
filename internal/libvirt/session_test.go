// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

package libvirt

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"
)

func env(vars map[string]string) func(string) string {
	return func(k string) string { return vars[k] }
}

func TestNewSession(t *testing.T) {
	for _, tc := range []struct {
		name string
		euid int
		env  map[string]string
		want string
	}{
		{"root", 0, map[string]string{"XDG_RUNTIME_DIR": "/run/user/0"}, "refusing to run as root"},
		{"no runtime dir", 1000, nil, "XDG_RUNTIME_DIR"},
		{"relative runtime dir", 1000, map[string]string{"XDG_RUNTIME_DIR": "run"}, "XDG_RUNTIME_DIR"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := newSession(tc.euid, env(tc.env)); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("newSession() = %v, want error with %q", err, tc.want)
			}
		})
	}

	s, err := newSession(1000, env(map[string]string{"XDG_RUNTIME_DIR": "/run/user/1000"}))
	if err != nil || s.dir != "/run/user/1000/libvirt" || !s.autostart {
		t.Errorf("newSession() = %+v, %v", s, err)
	}
	s, err = newSession(1000, env(map[string]string{"XDG_RUNTIME_DIR": "/run/user/1000", "LIBVIRT_AUTOSTART": "0"}))
	if err != nil || s.autostart {
		t.Errorf("LIBVIRT_AUTOSTART=0: autostart = %v, %v", s.autostart, err)
	}
}

// fakeDaemon answers dials for the sockets in listening and records the
// order of events.
type fakeDaemon struct {
	t         *testing.T
	dir       string
	listening map[string]bool
	// dialErr, if set, is returned for sockets that do not listen.
	dialErr error
	// startAfter is the number of dials after the spawn until virtqemud
	// listens; negative if it never does.
	startAfter int
	spawnErr   error
	spawned    bool
	events     []string
}

func (d *fakeDaemon) dial(path string) (net.Conn, error) {
	name := filepath.Base(path)
	d.events = append(d.events, "dial "+name)
	if d.spawned && name == "virtqemud-sock" && d.startAfter >= 0 {
		if d.startAfter == 0 {
			d.listening[name] = true
		}
		d.startAfter--
	}
	if d.listening[name] {
		c, _ := net.Pipe()
		return c, nil
	}
	if d.dialErr != nil {
		return nil, d.dialErr
	}
	return nil, &net.OpError{Op: "dial", Net: "unix", Err: os.NewSyscallError("connect", syscall.ECONNREFUSED)}
}

func (d *fakeDaemon) spawn(context.Context) error {
	d.events = append(d.events, "spawn")
	if _, err := os.Stat(filepath.Join(d.dir, "virtqemud.lock")); err != nil {
		d.t.Errorf("spawn without lock file: %v", err)
	}
	d.spawned = true
	return d.spawnErr
}

func (d *fakeDaemon) session(autostart bool) *session {
	return &session{dir: d.dir, autostart: autostart, dial: d.dial, spawn: d.spawn, retry: time.Millisecond, attempts: 5}
}

func TestConnect(t *testing.T) {
	probe := []string{"dial virtqemud-sock", "dial libvirt-sock"}
	for _, tc := range []struct {
		name       string
		listening  map[string]bool
		dialErr    error
		autostart  bool
		startAfter int
		spawnErr   error
		wantErr    string
		wantEvents []string
	}{
		{
			name: "virtqemud", listening: map[string]bool{"virtqemud-sock": true, "libvirt-sock": true},
			autostart: true, wantEvents: probe[:1],
		},
		{
			name: "libvirtd", listening: map[string]bool{"libvirt-sock": true},
			autostart: true, wantEvents: probe,
		},
		{
			name: "spawn", autostart: true, startAfter: 2,
			wantEvents: append(slices.Clone(probe), "spawn", "dial virtqemud-sock", "dial virtqemud-sock", "dial virtqemud-sock"),
		},
		{
			name: "no autostart", wantErr: "LIBVIRT_AUTOSTART=0", wantEvents: probe,
		},
		{
			name: "spawn fails", autostart: true, spawnErr: errors.New("boom"),
			wantErr: "start virtqemud: boom", wantEvents: append(slices.Clone(probe), "spawn"),
		},
		{
			name: "never listens", autostart: true, startAfter: -1, wantErr: "did not start listening",
			wantEvents: append(slices.Clone(probe), "spawn", "dial virtqemud-sock", "dial virtqemud-sock",
				"dial virtqemud-sock", "dial virtqemud-sock", "dial virtqemud-sock"),
		},
		{
			name: "permission denied", autostart: true, dialErr: syscall.EACCES,
			wantErr: "permission denied", wantEvents: probe[:1],
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			listening := map[string]bool{}
			for k, v := range tc.listening {
				listening[k] = v
			}
			d := &fakeDaemon{
				t: t, dir: filepath.Join(t.TempDir(), "libvirt"), listening: listening,
				dialErr: tc.dialErr, startAfter: tc.startAfter, spawnErr: tc.spawnErr,
			}
			conn, err := d.session(tc.autostart).connect(context.Background())
			if tc.wantErr == "" && err != nil {
				t.Fatal(err)
			}
			if tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)) {
				t.Errorf("connect() = %v, want error with %q", err, tc.wantErr)
			}
			if (err == nil) != (conn != nil) {
				t.Errorf("connect() = %v, %v", conn, err)
			}
			if !slices.Equal(d.events, tc.wantEvents) {
				t.Errorf("events = %q, want %q", d.events, tc.wantEvents)
			}
			if _, err := os.Stat(filepath.Join(d.dir, "virtqemud.lock")); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("lock file left behind: %v", err)
			}
		})
	}
}

func TestConnectCanceled(t *testing.T) {
	d := &fakeDaemon{t: t, dir: t.TempDir(), listening: map[string]bool{}, startAfter: -1}
	s := d.session(true)
	s.attempts = 1 << 20
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := s.connect(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("connect() = %v, want deadline exceeded", err)
	}
}

// TestConnectStaleSocket uses real sockets: a socket file without a
// listener must lead to a spawn.
func TestConnectStaleSocket(t *testing.T) {
	// Keep socket paths well below the 108-byte limit of sun_path.
	dir, err := os.MkdirTemp("", "brig")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	sock := filepath.Join(dir, "virtqemud-sock")
	stale, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	stale.(*net.UnixListener).SetUnlinkOnClose(false)
	_ = stale.Close()

	s, err := newSession(1000, env(map[string]string{"XDG_RUNTIME_DIR": dir}))
	if err != nil {
		t.Fatal(err)
	}
	s.dir, s.retry = dir, time.Millisecond
	s.spawn = func(context.Context) error {
		_ = os.Remove(sock)
		l, err := net.Listen("unix", sock)
		if err != nil {
			return err
		}
		t.Cleanup(func() { _ = l.Close() })
		go func() {
			if c, err := l.Accept(); err == nil {
				_ = c.Close()
			}
		}()
		return nil
	}
	conn, err := s.connect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	_ = conn.Close()
}

func TestSystemdRunArgs(t *testing.T) {
	got := systemdRunArgs("/usr/sbin/virtqemud", "brig-virtqemud-1", env(map[string]string{
		"XDG_RUNTIME_DIR": "/run/user/1000",
		"XDG_CONFIG_HOME": "/home/u/.config",
	}))
	want := []string{
		"--user", "--collect", "--quiet", "--unit=brig-virtqemud-1",
		"--description=libvirt QEMU session daemon started by brig",
		"--property=KillMode=process",
		"--setenv=XDG_CONFIG_HOME=/home/u/.config",
		"--setenv=XDG_RUNTIME_DIR=/run/user/1000",
		"/usr/sbin/virtqemud", "--timeout=120",
	}
	if !slices.Equal(got, want) {
		t.Errorf("systemdRunArgs() = %q\nwant %q", got, want)
	}
}

func TestVirtqemudUnit(t *testing.T) {
	start := time.Unix(1_700_000_000, 0)
	if got, want := virtqemudUnit(start), "brig-virtqemud-1700000000"; got != want {
		t.Errorf("virtqemudUnit() = %q, want %q", got, want)
	}
	if virtqemudUnit(start) == virtqemudUnit(start.Add(time.Second)) {
		t.Error("starts a second apart share a unit name")
	}
}

// fakeBinaries puts fake systemd-run (unless runExit is negative) and
// virtqemud scripts into an otherwise empty PATH. Both record their
// arguments in files in the returned directory; virtqemud also records
// /proc/self/stat.
func fakeBinaries(t *testing.T, runExit int) (dir, daemon string) {
	t.Helper()
	dir = t.TempDir()
	write := func(name, script string) string {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte("#!/bin/sh\n"+script), 0o700); err != nil { //nolint:gosec // G306: the scripts must be executable
			t.Fatal(err)
		}
		return path
	}
	if runExit >= 0 {
		write("systemd-run", fmt.Sprintf("printf '%%s\\n' \"$@\" > %q\necho 'Unit brig-virtqemud.service was already loaded'\nexit %d\n",
			filepath.Join(dir, "systemd-run.args"), runExit))
	}
	daemon = write("virtqemud", fmt.Sprintf("read -r stat < /proc/$$/stat\nprintf '%%s\\n%%s\\n' \"$*\" \"$stat\" > %q\n",
		filepath.Join(dir, "virtqemud.out")))
	t.Setenv("PATH", dir)
	return dir, daemon
}

func TestSpawnVirtqemudSystemdRun(t *testing.T) {
	dir, daemon := fakeBinaries(t, 0)
	getenv := env(map[string]string{"XDG_RUNTIME_DIR": "/run/user/1000"})
	if err := spawnVirtqemud(context.Background(), daemon, getenv); err != nil {
		t.Fatal(err)
	}
	args, err := os.ReadFile(filepath.Join(dir, "systemd-run.args"))
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Split(strings.TrimSuffix(string(args), "\n"), "\n")
	unit := ""
	for _, a := range got {
		if u, ok := strings.CutPrefix(a, "--unit="); ok {
			unit = u
		}
	}
	if !strings.HasPrefix(unit, "brig-virtqemud-") {
		t.Errorf("systemd-run unit %q, want brig-virtqemud-<time>", unit)
	}
	if want := systemdRunArgs(daemon, unit, getenv); !slices.Equal(got, want) {
		t.Errorf("systemd-run %q, want %q", got, want)
	}
	if _, err := os.Stat(filepath.Join(dir, "virtqemud.out")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("virtqemud started directly although systemd-run succeeded: %v", err)
	}
}

// TestSpawnVirtqemudDirect checks the fallback for hosts where no systemd
// user manager can start the daemon: virtqemud runs in a session of its
// own, detached from brig's terminal.
func TestSpawnVirtqemudDirect(t *testing.T) {
	for name, runExit := range map[string]int{"systemd-run fails": 1, "no systemd-run": -1} {
		t.Run(name, func(t *testing.T) {
			dir, daemon := fakeBinaries(t, runExit)
			if err := spawnVirtqemud(context.Background(), daemon, env(nil)); err != nil {
				t.Fatal(err)
			}
			var out []string
			for range 500 {
				data, _ := os.ReadFile(filepath.Join(dir, "virtqemud.out"))
				if out = strings.Split(strings.TrimSpace(string(data)), "\n"); len(out) == 2 {
					break
				}
				time.Sleep(10 * time.Millisecond)
			}
			if len(out) != 2 {
				t.Fatalf("virtqemud did not start: %q", out)
			}
			if out[0] != virtqemudTimeout {
				t.Errorf("virtqemud args = %q, want %q", out[0], virtqemudTimeout)
			}
			// Fields of /proc/PID/stat: pid (comm) state ppid pgrp session.
			if f := strings.Fields(out[1]); len(f) < 6 || f[0] != f[5] {
				t.Errorf("virtqemud is not a session leader: stat %q", out[1])
			}
		})
	}
}

// TestLockHelper holds the lock in a separate process for
// TestLockExcludesOtherProcesses: fcntl locks never conflict within one
// process.
func TestLockHelper(t *testing.T) {
	path := os.Getenv("BRIG_TEST_LOCK")
	if path == "" {
		t.Skip("helper process for TestLockExcludesOtherProcesses")
	}
	unlock, err := lock(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = os.Stdout.WriteString("locked\n")
	_, _ = io.Copy(io.Discard, os.Stdin)
	unlock()
}

func TestLockExcludesOtherProcesses(t *testing.T) {
	path := filepath.Join(t.TempDir(), "virtqemud.lock")
	cmd := exec.Command(os.Args[0], "-test.run=^TestLockHelper$") //nolint:gosec // G702: re-runs this test binary
	cmd.Env = append(os.Environ(), "BRIG_TEST_LOCK="+path)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	if line, err := bufio.NewReader(stdout).ReadString('\n'); line != "locked\n" {
		t.Fatalf("helper said %q, %v", line, err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := lock(ctx, path); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("lock() while held elsewhere = %v, want deadline exceeded", err)
	}

	_ = stdin.Close()
	if err := cmd.Wait(); err != nil {
		t.Fatal(err)
	}
	unlock, err := lock(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	unlock()
}
