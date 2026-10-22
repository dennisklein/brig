// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

package ui

import (
	"encoding/json"
	"fmt"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	"github.com/dennisklein/brig/internal/vm"
)

// instance is the guard that makes one brig ui of a user run at a time: a
// directory in $XDG_RUNTIME_DIR that holds the lock and the record of the
// brig ui that runs.
type instance struct {
	dir string
	// procRoot is the /proc that names the owner of a listening socket.
	procRoot string
	// flock takes or drops the lock on a file descriptor.
	flock func(fd, how int) error
	lockf *os.File
}

// newInstance returns the guard that lives in runtimeDir, and creates the
// directory it uses, for the user alone. The files in it are private too.
// A directory that is there already must be private: the user's own, mode
// 0700, and no symbolic link, as another user could read or replace what
// is in it otherwise.
func newInstance(runtimeDir string) (*instance, error) {
	dir := filepath.Join(runtimeDir, "brig")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	if err := checkPrivateDir(dir, os.Getuid()); err != nil {
		return nil, err
	}
	return &instance{dir: dir, procRoot: "/proc", flock: unix.Flock}, nil
}

// checkPrivateDir returns an error unless dir is a directory, not a link to
// one, of the user uid, that only that user can use.
func checkPrivateDir(dir string, uid int) error {
	info, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	switch {
	case !info.IsDir():
		return fmt.Errorf("%s is no directory", dir)
	case !ok || int(st.Uid) != uid:
		return fmt.Errorf("%s belongs to another user", dir)
	case info.Mode().Perm() != 0o700:
		return fmt.Errorf("%s has mode %04o; it must be 0700", dir, info.Mode().Perm())
	}
	return nil
}

func (i *instance) recordPath() string { return filepath.Join(i.dir, "ui.json") }

// lock waits for the lock that two starts of brig ui take in turn, so that
// they cannot both find no brig ui running and both start one.
func (i *instance) lock() error {
	f, err := os.OpenFile(filepath.Join(i.dir, "ui.lock"), os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return err
	}
	if err := i.flock(int(f.Fd()), unix.LOCK_EX); err != nil {
		_ = f.Close()
		return err
	}
	i.lockf = f
	return nil
}

// unlock drops the lock; closing the file does. It does nothing without a
// lock.
func (i *instance) unlock() {
	if i.lockf != nil {
		_ = i.lockf.Close()
		i.lockf = nil
	}
}

// running returns the record of a brig ui that still runs: its process is
// there, and a socket of the user listens at its URL.
func (i *instance) running() (runtimeRecord, bool) {
	r, ok := readRecord(i.recordPath())
	if !ok {
		return r, false
	}
	// Signal 0 checks that the process exists and is ours; a PID used
	// again by another process does not listen at the URL.
	if syscall.Kill(r.PID, 0) != nil {
		return r, false
	}
	u, ok := recordURL(r.URL)
	if !ok {
		return r, false
	}
	addr, err := netip.ParseAddrPort(u.Host)
	if err != nil {
		return r, false
	}
	c, err := net.DialTimeout("tcp4", u.Host, time.Second)
	if err != nil {
		return r, false
	}
	_ = c.Close()
	// Another user may listen on the port of a brig ui that has gone; the
	// browser must not take the URL there.
	uid, err := procNetTCPListener(i.procRoot, addr)
	return r, err == nil && uid == os.Getuid()
}

// publish writes the record of the brig ui that runs, readable only by the
// user.
func (i *instance) publish(r runtimeRecord) error {
	data, err := json.Marshal(r)
	if err != nil {
		return err
	}
	return vm.WriteFileAtomic(i.recordPath(), append(data, '\n'), 0o600)
}

// retire removes the record.
func (i *instance) retire() { _ = os.Remove(i.recordPath()) }
