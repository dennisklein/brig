// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

package vmnet

import (
	"fmt"
	"net"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

// journalStdout is where journald takes the output streams of processes.
var journalStdout = "/run/systemd/journal/stdout"

// StderrToJournal keeps the stderr of a process in the namespace in the
// network unit's journal. systemd connects the unit's stderr to the journal
// and says so in JOURNAL_STREAM, but SELinux takes the stream away from
// pasta, which may not write to it, and hands the namespace a null device
// instead. A process that finds its stderr is not the stream it was given
// opens a stream of its own, which its children inherit. Outside a unit, it
// leaves stderr as it is. restore gives the process its stderr back.
func StderrToJournal() (restore func()) {
	restore = func() {}
	given := os.Getenv("JOURNAL_STREAM")
	if given == "" || streamOf(unix.Stderr) == given {
		return restore
	}
	conn, err := net.DialUnix("unix", nil, &net.UnixAddr{Name: journalStdout, Net: "unix"})
	if err != nil {
		return restore
	}
	defer func() { _ = conn.Close() }()
	// The identifier, no unit, priority info, no level prefixes, and no
	// copies to syslog, the kernel's log or the console.
	header := fmt.Sprintf("%s\n\n6\n0\n0\n0\n0\n", filepath.Base(os.Args[0]))
	if _, err := conn.Write([]byte(header)); err != nil {
		return restore
	}
	_ = conn.CloseRead()
	f, err := conn.File()
	if err != nil {
		return restore
	}
	defer func() { _ = f.Close() }()
	old, err := unix.FcntlInt(uintptr(unix.Stderr), unix.F_DUPFD_CLOEXEC, 3)
	if err != nil {
		return restore
	}
	if err := unix.Dup3(int(f.Fd()), unix.Stderr, 0); err != nil {
		_ = unix.Close(old)
		return restore
	}
	_ = os.Setenv("JOURNAL_STREAM", streamOf(unix.Stderr))
	return func() {
		_ = unix.Dup3(old, unix.Stderr, 0)
		_ = unix.Close(old)
		_ = os.Setenv("JOURNAL_STREAM", given)
	}
}

// streamOf is the device and inode of fd as JOURNAL_STREAM gives them.
func streamOf(fd int) string {
	var st unix.Stat_t
	if unix.Fstat(fd, &st) != nil {
		return ""
	}
	return fmt.Sprintf("%d:%d", st.Dev, st.Ino)
}
