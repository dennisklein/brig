// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

package vmnet

import (
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
)

// TestStderrToJournal runs itself as a process whose stderr systemd gave
// the journal but SELinux replaced with a null device, as pasta's
// namespace finds it.
func TestStderrToJournal(t *testing.T) {
	if path := os.Getenv("VMNET_TEST_JOURNAL"); path != "" {
		journalStdout = path
		restore := StderrToJournal()
		if got, want := os.Getenv("JOURNAL_STREAM"), streamOf(unix.Stderr); got != want {
			fmt.Printf("JOURNAL_STREAM %q, stderr %q\n", got, want)
		}
		fmt.Fprintln(os.Stderr, "brig: the firewall follows a change of the host's network")
		restore()
		if got := os.Getenv("JOURNAL_STREAM"); got != "1:2" {
			fmt.Printf("JOURNAL_STREAM %q after restore\n", got)
		}
		fmt.Fprintln(os.Stderr, "brig: after restore")
		return
	}
	path := filepath.Join(t.TempDir(), "stdout")
	l, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Close() }()
	cmd := exec.Command(os.Args[0], "-test.run=^TestStderrToJournal$") //nolint:gosec // G702: the test binary itself
	cmd.Env = append(os.Environ(), "VMNET_TEST_JOURNAL="+path, "JOURNAL_STREAM=1:2")
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	conn, err := l.Accept()
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(conn)
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Base(os.Args[0]) + "\n\n6\n0\n0\n0\n0\n" +
		"brig: the firewall follows a change of the host's network\n"
	if string(got) != want {
		t.Errorf("journal got %q, want %q", got, want)
	}
	if string(out) != "PASS\n" {
		t.Errorf("child: %s", out)
	}
}

func TestStderrToJournalLeavesStderr(t *testing.T) {
	journalStdout = filepath.Join(t.TempDir(), "none")
	t.Cleanup(func() { journalStdout = "/run/systemd/journal/stdout" })
	before := streamOf(unix.Stderr)
	for _, env := range []string{"", before} {
		t.Setenv("JOURNAL_STREAM", env)
		StderrToJournal()()
		if got := streamOf(unix.Stderr); got != before {
			t.Errorf("JOURNAL_STREAM %q: stderr is %q, was %q", env, got, before)
		}
	}
}
