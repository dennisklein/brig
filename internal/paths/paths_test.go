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

	d, err := Default()
	if err != nil {
		t.Fatal(err)
	}
	want := Dirs{Config: "/x/config/brig", Data: "/home/u/.local/share/brig", Cache: "/x/cache/brig"}
	if d != want {
		t.Fatalf("Default() = %+v, want %+v", d, want)
	}
	if got := d.ConfigFile(); got != "/x/config/brig/config.yaml" {
		t.Errorf("ConfigFile() = %q", got)
	}
}

func TestDefaultRejectsRelativeXDG(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", "relative")
	if _, err := Default(); err == nil {
		t.Fatal("expected an error for a relative XDG_DATA_HOME")
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
