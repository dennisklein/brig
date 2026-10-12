// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// stateHome points the activity log at a new directory, and returns its path.
func stateHome(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("XDG_STATE_HOME", dir)
	return filepath.Join(dir, "brig", "activity.jsonl")
}

func readActivity(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return ""
	}
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestActivityLogIsTheDefault(t *testing.T) {
	log := stateHome(t)
	t.Setenv(progressLogVar, "")
	if err := os.Unsetenv(progressLogVar); err != nil {
		t.Fatal(err)
	}
	if _, _, started, err := runSplit(t, "print-fedora-deps"); err != nil || !started {
		t.Fatalf("started = %v, err = %v", started, err)
	}
	got := readActivity(t, log)
	if !strings.Contains(got, `"name":"print-fedora-deps"`) {
		t.Errorf("activity log lacks the command:\n%s", got)
	}
	fi, err := os.Stat(log)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("activity log mode = %o, want 600", fi.Mode().Perm())
	}
	if fi, err := os.Stat(filepath.Dir(log)); err != nil || fi.Mode().Perm() != 0o700 {
		t.Errorf("state directory = %v, %v; want mode 700", fi, err)
	}
}

func TestActivityLogYieldsToTheFlagAndTheVariable(t *testing.T) {
	log := stateHome(t)
	named := filepath.Join(t.TempDir(), "named.jsonl")

	t.Setenv(progressLogVar, named)
	if _, _, _, err := runSplit(t, "print-fedora-deps"); err != nil {
		t.Fatal(err)
	}
	if readActivity(t, named) == "" || readActivity(t, log) != "" {
		t.Errorf("variable: named log = %q, activity log = %q", readActivity(t, named), readActivity(t, log))
	}

	flagged := filepath.Join(t.TempDir(), "flagged.jsonl")
	if _, _, _, err := runSplit(t, "--progress-log", flagged, "print-fedora-deps"); err != nil {
		t.Fatal(err)
	}
	if readActivity(t, flagged) == "" || readActivity(t, log) != "" {
		t.Errorf("flag: flagged log = %q, activity log = %q", readActivity(t, flagged), readActivity(t, log))
	}
}

func TestEmptyFlagOrVariableTurnsTheLogOff(t *testing.T) {
	log := stateHome(t)
	t.Setenv(progressLogVar, "")
	if _, _, _, err := runSplit(t, "print-fedora-deps"); err != nil {
		t.Fatal(err)
	}
	if got := readActivity(t, log); got != "" {
		t.Errorf("empty variable: activity log = %q", got)
	}
	if err := os.Unsetenv(progressLogVar); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := runSplit(t, "--progress-log=", "print-fedora-deps"); err != nil {
		t.Fatal(err)
	}
	if got := readActivity(t, log); got != "" {
		t.Errorf("empty flag: activity log = %q", got)
	}
}

func TestUnusableStateDirectoryWritesNoLogAndNoNote(t *testing.T) {
	file := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	// A state directory below a file cannot be made.
	t.Setenv("XDG_STATE_HOME", file)
	if err := os.Unsetenv(progressLogVar); err != nil {
		t.Fatal(err)
	}
	_, stderr, _, err := runSplit(t, "print-fedora-deps")
	if err != nil {
		t.Fatal(err)
	}
	if stderr != "" {
		t.Errorf("standard error = %q, want none", stderr)
	}
}

func TestRotateLog(t *testing.T) {
	dir := t.TempDir()
	log := filepath.Join(dir, "activity.jsonl")
	write := func(path, s string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(s), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	if err := rotateLog(log, 10); err != nil {
		t.Errorf("rotating a missing log: %v", err)
	}
	write(log, "0123456789")
	if err := rotateLog(log, 10); err != nil {
		t.Fatal(err)
	}
	if readActivity(t, log) != "0123456789" || readActivity(t, log+".1") != "" {
		t.Error("a log at the limit was rotated")
	}

	write(log, "0123456789a")
	write(log+".1", "older")
	if err := rotateLog(log, 10); err != nil {
		t.Fatal(err)
	}
	if got := readActivity(t, log); got != "" {
		t.Errorf("log after rotation = %q", got)
	}
	if got := readActivity(t, log+".1"); got != "0123456789a" {
		t.Errorf("old copy = %q, want it replaced", got)
	}
}

func TestDefaultActivityLogRotates(t *testing.T) {
	log := stateHome(t)
	if err := os.Unsetenv(progressLogVar); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(log), 0o700); err != nil {
		t.Fatal(err)
	}
	big := strings.Repeat("x", activityLogLimit+1)
	if err := os.WriteFile(log, []byte(big), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := runSplit(t, "print-fedora-deps"); err != nil {
		t.Fatal(err)
	}
	if got := readActivity(t, log+".1"); got != big {
		t.Errorf("rotated copy has %d bytes, want %d", len(got), len(big))
	}
	if got := readActivity(t, log); !strings.Contains(got, "print-fedora-deps") || strings.Contains(got, "xxx") {
		t.Errorf("new log:\n%.200s", got)
	}
}
