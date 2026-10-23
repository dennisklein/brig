// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

package paths

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDefaultHonoursXDG(t *testing.T) {
	t.Setenv("HOME", "/home/u")
	t.Setenv("XDG_CONFIG_HOME", "/x/config")
	t.Setenv("XDG_DATA_HOME", "")
	t.Setenv("XDG_CACHE_HOME", "/x/cache")
	t.Setenv("XDG_STATE_HOME", "")

	d, err := Default()
	if err != nil {
		t.Fatal(err)
	}
	want := Dirs{Config: "/x/config/brig", Data: "/home/u/.local/share/brig", Cache: "/x/cache/brig", State: "/home/u/.local/state/brig"}
	if d != want {
		t.Fatalf("Default() = %+v, want %+v", d, want)
	}
	if got := d.ConfigFile(); got != "/x/config/brig/config.yaml" {
		t.Errorf("ConfigFile() = %q", got)
	}
	if got := d.ActivityLog(); got != "/home/u/.local/state/brig/activity.jsonl" {
		t.Errorf("ActivityLog() = %q", got)
	}
}

func TestDefaultRejectsRelativeXDG(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", "relative")
	if _, err := Default(); err == nil {
		t.Fatal("expected an error for a relative XDG_DATA_HOME")
	}
}

func TestDefaultStateHome(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", "/x/state")
	d, err := Default()
	if err != nil {
		t.Fatal(err)
	}
	if d.State != "/x/state/brig" {
		t.Errorf("State = %q", d.State)
	}
	t.Setenv("XDG_STATE_HOME", "relative")
	if _, err := Default(); err == nil {
		t.Error("Default() accepted a relative XDG_STATE_HOME")
	}
}

func TestDefaultRejectsDataDirSSHExpands(t *testing.T) {
	for _, dir := range []string{"/x/100%", "/x/%h", "/x/${USER}"} {
		t.Setenv("XDG_DATA_HOME", dir)
		if _, err := Default(); err == nil {
			t.Errorf("Default() accepted data directory %q", dir)
		}
	}
	// ssh expands only ${NAME}, so a bare $ is fine.
	t.Setenv("XDG_DATA_HOME", "/x/$USER")
	if _, err := Default(); err != nil {
		t.Errorf("Default() rejected a bare $: %v", err)
	}
}

func TestEnsurePrivate(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "a", "b")
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := EnsurePrivate(dir); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o700 {
		t.Fatalf("mode = %v, want 0700", fi.Mode().Perm())
	}
}

func TestWriteFileGivesExactlyPerm(t *testing.T) {
	path := filepath.Join(t.TempDir(), "key")
	if err := os.WriteFile(path, []byte("old"), 0o644); err != nil { //nolint:gosec // G306: a too-open file that WriteFile must tighten
		t.Fatal(err)
	}
	if err := WriteFile(path, []byte("new"), 0o600); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil || string(data) != "new" {
		t.Fatalf("content = %q, %v; want new", data, err)
	}
	if fi, err := os.Stat(path); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %v, %v; want 0600", fi.Mode().Perm(), err)
	}
}

func TestWriteFileMakesTheFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "a", "file")
	if err := WriteFile(path, []byte("x"), 0o640); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Stat(path); err != nil || fi.Mode().Perm() != 0o640 {
		t.Fatalf("mode = %v, %v; want 0640", fi.Mode().Perm(), err)
	}
}

func TestWriteFileLeavesNothingOnAnError(t *testing.T) {
	dir := t.TempDir()
	// A directory at the path: the rename of the new file over it fails.
	target := filepath.Join(dir, "target")
	if err := os.Mkdir(target, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := WriteFile(target, []byte("new"), 0o700); err == nil {
		t.Fatal("WriteFile over a directory succeeded")
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 1 || entries[0].Name() != "target" {
		t.Fatalf("directory holds %v, %v; want target alone", entries, err)
	}
}
