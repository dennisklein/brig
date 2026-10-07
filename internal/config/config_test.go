// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

package config

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/dennisklein/brig/internal/bytesize"
)

func write(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoadMissingFileIsBuiltin(t *testing.T) {
	cfg, err := Load(filepath.Join(t.TempDir(), "absent.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(cfg, Builtin()) {
		t.Fatalf("Load(absent) = %+v, want built-in config", cfg)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("built-in config is invalid: %v", err)
	}
}

func TestLoadEmptyFile(t *testing.T) {
	if _, err := Load(write(t, "")); err != nil {
		t.Fatal(err)
	}
}

func TestLoadOverlay(t *testing.T) {
	cfg, err := Load(write(t, `
defaults:
  memory: 16GiB
  network_profile: ollama
network_profiles:
  ollama:
    internet: true
    host_ports: [11434]
  open:
    internet: true
`))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Defaults.Memory != 16*bytesize.GiB || cfg.Defaults.CPUs != 4 {
		t.Errorf("defaults = %+v", cfg.Defaults)
	}
	want := NetworkProfile{Internet: true, HostPorts: []uint16{11434}}
	if got, _ := cfg.Profile("ollama"); !reflect.DeepEqual(got, want) {
		t.Errorf("ollama = %+v, want %+v", got, want)
	}
	// A profile in the file replaces the built-in one as a whole.
	if got, _ := cfg.Profile("open"); got.LAN || got.Host {
		t.Errorf("open = %+v, want built-in fields dropped", got)
	}
	if _, err := cfg.Profile("isolated"); err != nil {
		t.Errorf("built-in profiles must survive an overlay: %v", err)
	}
}

func TestLoadErrors(t *testing.T) {
	for name, tc := range map[string]struct{ yaml, want string }{
		"unknown key":     {"defaults:\n  cpu: 2\n", "field cpu not found"},
		"bad size":        {"defaults:\n  memory: lots\n", "invalid size"},
		"unknown profile": {"defaults:\n  network_profile: nope\n", `unknown network profile "nope"`},
		"tiny memory":     {"defaults:\n  memory: 1MiB\n", "at least 512MiB"},
		"port zero":       {"network_profiles:\n  x:\n    host_ports: [0]\n", "must not contain 0"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := Load(write(t, tc.yaml))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Load() error = %v, want it to contain %q", err, tc.want)
			}
		})
	}
}

func TestProfileNamesSorted(t *testing.T) {
	got := Builtin().ProfileNames()
	want := []string{"default", "isolated", "open"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ProfileNames() = %v, want %v", got, want)
	}
}
