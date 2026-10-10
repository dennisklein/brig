// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

package libvirt

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// session finds the socket of the user's libvirt session daemon and starts
// virtqemud when nothing listens there, mirroring what libvirt's own
// clients do in virNetSocketNewConnectUNIX (src/rpc/virnetsocket.c).
type session struct {
	// dir is $XDG_RUNTIME_DIR/libvirt.
	dir string
	// autostart is false when LIBVIRT_AUTOSTART=0.
	autostart bool
	dial      func(path string) (net.Conn, error)
	spawn     func(context.Context) error
	// After a spawn, connect retries every retry, at most attempts times.
	retry    time.Duration
	attempts int
}

const virtqemudTimeout = "--timeout=120" // idle timeout, as libvirt's clients use

func newSession(euid int, getenv func(string) string) (*session, error) {
	if euid == 0 {
		return nil, errors.New("refusing to run as root: brig manages VMs in your user's libvirt session (qemu:///session)")
	}
	rt := getenv("XDG_RUNTIME_DIR")
	if !filepath.IsAbs(rt) {
		return nil, errors.New("XDG_RUNTIME_DIR must be set to an absolute path to reach the libvirt session daemon")
	}
	return &session{
		dir:       filepath.Join(rt, "libvirt"),
		autostart: getenv("LIBVIRT_AUTOSTART") != "0",
		dial: func(path string) (net.Conn, error) {
			return net.DialTimeout("unix", path, 5*time.Second)
		},
		spawn: func(ctx context.Context) error {
			daemon, err := findVirtqemud()
			if err != nil {
				return err
			}
			return spawnVirtqemud(ctx, daemon, getenv)
		},
		retry:    10 * time.Millisecond,
		attempts: 500,
	}, nil
}

// connect returns a connection to the session daemon. It probes the
// modular daemon's socket first, then the monolithic libvirtd's. Probing
// connects rather than checks for the file, so a stale socket left by a
// crashed daemon still leads to a spawn. Like libvirt, it holds the lock
// $XDG_RUNTIME_DIR/libvirt/virtqemud.lock across probe, spawn and the
// connect loop, so concurrent clients start only one daemon.
func (s *session) connect(ctx context.Context) (net.Conn, error) {
	if err := os.MkdirAll(s.dir, 0o700); err != nil {
		return nil, err
	}
	unlock, err := lock(ctx, filepath.Join(s.dir, "virtqemud.lock"))
	if err != nil {
		return nil, err
	}
	defer unlock()

	for _, name := range []string{"virtqemud-sock", "libvirt-sock"} {
		conn, err := s.dial(filepath.Join(s.dir, name))
		if err == nil {
			return conn, nil
		}
		if !notListening(err) {
			return nil, err
		}
	}
	if !s.autostart {
		return nil, fmt.Errorf("no libvirt session daemon listens in %s, and LIBVIRT_AUTOSTART=0 forbids starting virtqemud", s.dir)
	}
	if err := s.spawn(ctx); err != nil {
		return nil, fmt.Errorf("start virtqemud: %w", err)
	}
	sock := filepath.Join(s.dir, "virtqemud-sock")
	for range s.attempts {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(s.retry):
		}
		var conn net.Conn
		if conn, err = s.dial(sock); err == nil || !notListening(err) {
			return conn, err
		}
	}
	return nil, fmt.Errorf("virtqemud did not start listening: %w", err)
}

// notListening reports whether a connect error means that no daemon
// listens on the socket, the errors after which libvirt spawns one.
func notListening(err error) bool {
	return errors.Is(err, syscall.ENOENT) || errors.Is(err, syscall.ECONNREFUSED)
}

// lock takes the lock libvirt's clients take before spawning a daemon: an
// fcntl write lock on the first byte of path (virFileLock), not flock(2),
// which Linux keeps separate. Like libvirt, unlock removes the file while
// still holding the lock.
func lock(ctx context.Context, path string) (unlock func(), err error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600) //nolint:gosec // G703: path is in the user's own runtime directory
	if err != nil {
		return nil, err
	}
	lk := unix.Flock_t{Type: unix.F_WRLCK, Whence: io.SeekStart, Start: 0, Len: 1}
	for {
		err := unix.FcntlFlock(f.Fd(), unix.F_SETLK, &lk)
		if err == nil {
			break
		}
		if !errors.Is(err, unix.EAGAIN) && !errors.Is(err, unix.EACCES) {
			_ = f.Close()
			return nil, fmt.Errorf("lock %s: %w", path, err)
		}
		select {
		case <-ctx.Done():
			_ = f.Close()
			return nil, ctx.Err()
		case <-time.After(10 * time.Millisecond):
		}
	}
	return func() {
		_ = os.Remove(path) //nolint:gosec // G703: path is in the user's own runtime directory
		_ = f.Close()
	}, nil
}

// spawnVirtqemud starts the virtqemud binary daemon in the background. It
// prefers a transient systemd user service: a daemon started directly would
// stay in the cgroup of brig's terminal and die with it, taking the VMs
// along. KillMode=process, as in libvirt's own units, keeps the VMs running
// when the daemon's unit stops. In session mode libvirt leaves QEMU in the
// daemon's cgroup, so after the daemon dies with VMs running, systemd keeps
// its unit loaded and refuses to reuse the name. Each start therefore gets a
// unit name of its own.
func spawnVirtqemud(ctx context.Context, daemon string, getenv func(string) string) error {
	var runErr error
	if run, err := exec.LookPath("systemd-run"); err == nil {
		out, err := exec.CommandContext(ctx, run, systemdRunArgs(daemon, virtqemudUnit(time.Now()), getenv)...).CombinedOutput()
		if err == nil {
			return nil
		}
		runErr = fmt.Errorf("systemd-run: %w: %s", err, out)
	}
	cmd := exec.Command(daemon, virtqemudTimeout) // not bound to ctx: the daemon must outlive brig
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return errors.Join(runErr, err)
	}
	go func() { _ = cmd.Wait() }() // reap it should it exit while brig runs
	return nil
}

func findVirtqemud() (string, error) {
	for _, name := range []string{"/usr/sbin/virtqemud", "virtqemud"} {
		if path, err := exec.LookPath(name); err == nil {
			return path, nil
		}
	}
	return "", errors.New("virtqemud not found in /usr/sbin or $PATH: install libvirt-daemon-driver-qemu")
}

// virtqemudUnit returns the name of the transient unit that runs a daemon
// started at now.
func virtqemudUnit(now time.Time) string {
	return fmt.Sprintf("brig-virtqemud-%d", now.Unix())
}

// systemdRunArgs returns the systemd-run arguments that start daemon as the
// transient unit unit. The service gets the XDG directories that libvirt
// passes to the daemons it spawns, since the user manager's environment may
// differ from brig's.
func systemdRunArgs(daemon, unit string, getenv func(string) string) []string {
	args := []string{
		"--user", "--collect", "--quiet",
		"--unit=" + unit,
		"--description=libvirt QEMU session daemon started by brig",
		"--property=KillMode=process",
	}
	for _, k := range []string{"XDG_CACHE_HOME", "XDG_CONFIG_HOME", "XDG_RUNTIME_DIR"} {
		if v := getenv(k); v != "" {
			args = append(args, "--setenv="+k+"="+v)
		}
	}
	return append(args, daemon, virtqemudTimeout)
}
