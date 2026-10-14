// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

package config

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

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
		"unknown key":       {"defaults:\n  cpu: 2\n", "field cpu not found"},
		"bad size":          {"defaults:\n  memory: lots\n", "invalid size"},
		"unknown profile":   {"defaults:\n  network_profile: nope\n", `unknown network profile "nope"`},
		"tiny memory":       {"defaults:\n  memory: 1MiB\n", "at least 512MiB"},
		"fractional memory": {"defaults:\n  memory: 1000000K\n", "whole number of MiB"},
		"port zero":         {"network_profiles:\n  x:\n    host_ports: [0]\n", "must not contain 0"},
		"relative tool":     {"openshell:\n  secret_tool: bin/secret-tool\n", "absolute path"},
		"relative config":   {"openshell:\n  configs: [team]\n", "not an absolute path"},
		"journal mode":      {"audit:\n  journal: some\n", "must be changes, all or off"},
		"bad retention":     {"audit:\n  retention: forever\n", "invalid duration"},
		"short retention":   {"audit:\n  retention: 1h\n", "at least a day"},
		"second document":   {"defaults:\n  cpus: 2\n---\nnetwork_profiles:\n  default: {}\n", "more than one YAML document"},
		"empty document":    {"---\n---\ndefaults:\n  cpus: 2\n", "more than one YAML document"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := Load(write(t, tc.yaml))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Load() error = %v, want it to contain %q", err, tc.want)
			}
		})
	}
}

func TestLoadAudit(t *testing.T) {
	if b := Builtin().Audit; b.Journal != "changes" || b.Retention != 0 {
		t.Errorf("built-in audit section: %+v", b)
	}
	cfg, err := Load(write(t, "audit:\n  journal: all\n  retention: 90d\n"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Audit.Journal != "all" || time.Duration(cfg.Audit.Retention) != 90*24*time.Hour {
		t.Errorf("audit section = %+v", cfg.Audit)
	}
	for in, want := range map[string]time.Duration{"2w": 14 * 24 * time.Hour, "36h": 36 * time.Hour, "0d": 0} {
		if got, err := ParseDuration(in); err != nil || got != want {
			t.Errorf("ParseDuration(%q) = %v, %v", in, got, err)
		}
	}
}

func TestProfileNamesSorted(t *testing.T) {
	got := Builtin().ProfileNames()
	want := []string{"default", "isolated", "open"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ProfileNames() = %v, want %v", got, want)
	}
}

func TestLoadOpenShell(t *testing.T) {
	if b := Builtin().OpenShell; b.SecretTool != "secret-tool" || b.Configs != nil {
		t.Errorf("built-in openshell section: %+v", b)
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	cfg, err := Load(write(t, "openshell:\n  secret_tool: ~/bin/my-secret-tool\n  configs: [~/team, /etc/brig/openshell]\n"))
	if err != nil {
		t.Fatal(err)
	}
	want := OpenShell{
		SecretTool: filepath.Join(home, "bin", "my-secret-tool"),
		Configs:    []string{filepath.Join(home, "team"), "/etc/brig/openshell"},
	}
	if !reflect.DeepEqual(cfg.OpenShell, want) {
		t.Errorf("openshell section = %+v, want %+v", cfg.OpenShell, want)
	}
}

func TestNetworkProfileAllows(t *testing.T) {
	for name, want := range map[string]string{
		"default":  "internet",
		"open":     "internet, LAN, all host services, IPv6",
		"isolated": "nothing outside the VM",
	} {
		p, err := Builtin().Profile(name)
		if err != nil || p.Allows() != want {
			t.Errorf("profile %s allows %q, %v; want %q", name, p.Allows(), err, want)
		}
	}
	if got := (NetworkProfile{HostPorts: []uint16{11434, 8080}}).Allows(); got != "host ports 11434, 8080" {
		t.Errorf("host ports: %q", got)
	}
}

func TestUnknownProfileListsTheKnownOnes(t *testing.T) {
	_, err := Builtin().Profile("nope")
	if err == nil || !strings.Contains(err.Error(), "(have default, isolated, open)") {
		t.Errorf("Profile(nope) = %v", err)
	}
}

func TestExampleConfigSetsEveryKey(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	cfg, err := Load("../../config.example.yaml")
	if err != nil {
		t.Fatal(err)
	}
	set := map[string]bool{}
	setKeys(reflect.ValueOf(*cfg), "", set)
	for _, key := range allKeys(reflect.TypeFor[Config](), "") {
		if !set[key] {
			t.Errorf("config.example.yaml does not set %s; add it, so that the example stays complete", key)
		}
	}
	if len(cfg.NetworkProfiles) <= len(Builtin().NetworkProfiles) {
		t.Error("config.example.yaml defines no network profile of its own")
	}
}

// allKeys lists the yaml keys below t, such as audit.retention. NAME stands
// for the key of a map.
func allKeys(t reflect.Type, prefix string) []string {
	switch t.Kind() {
	case reflect.Map:
		return allKeys(t.Elem(), prefix+"NAME.")
	case reflect.Struct:
		var out []string
		for i := range t.NumField() {
			tag, _, _ := strings.Cut(t.Field(i).Tag.Get("yaml"), ",")
			out = append(out, allKeys(t.Field(i).Type, prefix+tag+".")...)
		}
		return out
	}
	return []string{strings.TrimSuffix(prefix, ".")}
}

// setKeys records the keys below v that hold a value other than zero; a key
// of a map is set if any entry sets it.
func setKeys(v reflect.Value, prefix string, set map[string]bool) {
	switch v.Kind() {
	case reflect.Map:
		for _, k := range v.MapKeys() {
			setKeys(v.MapIndex(k), prefix+"NAME.", set)
		}
	case reflect.Struct:
		for i := range v.NumField() {
			tag, _, _ := strings.Cut(v.Type().Field(i).Tag.Get("yaml"), ",")
			setKeys(v.Field(i), prefix+tag+".", set)
		}
	default:
		if !v.IsZero() {
			set[strings.TrimSuffix(prefix, ".")] = true
		}
	}
}
