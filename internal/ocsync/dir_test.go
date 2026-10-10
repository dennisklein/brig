// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

package ocsync

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/dennisklein/brig/internal/openshell"
)

func writeFile(t *testing.T, path, data string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
}

const githubProfile = `id: github
display_name: GitHub
endpoints:
  - host: api.github.com
    port: 443
  - host: github.com
    port: 443
`

const githubProvider = `name: github
type: github
credentials:
  GH_TOKEN:
    secret_tool:
      lookup: [service, github.com, user, alice]
`

func TestLoad(t *testing.T) {
	team, personal := t.TempDir(), t.TempDir()
	writeFile(t, filepath.Join(team, "profiles", "github.yaml"), githubProfile)
	writeFile(t, filepath.Join(team, "profiles", "corp.json"), `{"id": "corp", "endpoints": [{"host": "git.corp", "ports": [443, 8443]}]}`)
	writeFile(t, filepath.Join(team, "profiles", "README.md"), "not a profile")
	writeFile(t, filepath.Join(team, "policies", "default.yaml"), "version: 1\n")
	writeFile(t, filepath.Join(personal, "providers", "github.yaml"), githubProvider)
	set, err := Load([]string{team, personal})
	if err != nil {
		t.Fatal(err)
	}
	if len(set.Profiles) != 2 || set.Profiles[0].ID != "corp" || set.Profiles[1].ID != "github" {
		t.Fatalf("profiles: %+v", set.Profiles)
	}
	if want := []openshell.Endpoint{{Host: "git.corp", Ports: []uint32{443, 8443}}}; !reflect.DeepEqual(set.Profiles[0].Endpoints, want) {
		t.Errorf("corp endpoints = %+v", set.Profiles[0].Endpoints)
	}
	want := Provider{
		Name: "github", Type: "github", Path: filepath.Join(personal, "providers", "github.yaml"),
		Credentials: map[string]Credential{"GH_TOKEN": {SecretTool: &SecretToolRef{Lookup: []string{"service", "github.com", "user", "alice"}}}},
	}
	if len(set.Providers) != 1 || !reflect.DeepEqual(set.Providers[0], want) {
		t.Errorf("providers: %+v", set.Providers)
	}
	if set.DefaultPolicy != filepath.Join(team, "policies", "default.yaml") {
		t.Errorf("default policy %q", set.DefaultPolicy)
	}
	if set, err := Load([]string{t.TempDir()}); err != nil || len(set.Profiles)+len(set.Providers) != 0 || set.DefaultPolicy != "" {
		t.Errorf("empty directory: %+v, %v", set, err)
	}
}

func TestLoadRejects(t *testing.T) {
	provider := func(body string) func(dir string) {
		return func(dir string) { writeFile(t, filepath.Join(dir, "providers", "p.yaml"), body) }
	}
	for name, tc := range map[string]struct {
		setup func(dir string)
		want  string
	}{
		"plaintext value": {provider("name: p\ntype: github\ncredentials:\n  GH_TOKEN:\n    value: s3cret\n"), "never values"},
		"no credentials":  {provider("name: p\ntype: github\n"), "at least one credential"},
		"odd lookup":      {provider("name: p\ntype: github\ncredentials:\n  T:\n    secret_tool: {lookup: [service]}\n"), "attribute/value pairs"},
		"empty attribute": {provider("name: p\ntype: github\ncredentials:\n  T:\n    secret_tool: {lookup: [service, '']}\n"), "attribute/value pairs"},
		"reserved key":    {provider("name: p\ntype: github\ncredentials:\n  PATH:\n    secret_tool: {lookup: [a, b]}\n"), "cannot pass"},
		"openshell key":   {provider("name: p\ntype: github\ncredentials:\n  openshell_x:\n    secret_tool: {lookup: [a, b]}\n"), "cannot pass"},
		"revision key":    {provider("name: p\ntype: github\ncredentials:\n  v10_TOKEN:\n    secret_tool: {lookup: [a, b]}\n"), "cannot pass"},
		"bad key":         {provider("name: p\ntype: github\ncredentials:\n  A-B:\n    secret_tool: {lookup: [a, b]}\n"), "environment variable"},
		"bad name":        {provider("name: ../p\ntype: github\ncredentials:\n  T:\n    secret_tool: {lookup: [a, b]}\n"), "provider name"},
		"missing type":    {provider("name: p\ncredentials:\n  T:\n    secret_tool: {lookup: [a, b]}\n"), "provider type"},
		"profile id": {func(dir string) {
			writeFile(t, filepath.Join(dir, "profiles", "x.yaml"), "display_name: X\n")
		}, "profile id"},
		"octal port": {func(dir string) {
			writeFile(t, filepath.Join(dir, "profiles", "x.yaml"), "id: x\nendpoints:\n  - host: github.com\n    port: 0673\n")
		}, "leading zero"},
		"octal ports": {func(dir string) {
			writeFile(t, filepath.Join(dir, "profiles", "x.yaml"), "id: x\nendpoints:\n  - host: github.com\n    ports: [443, 0673]\n")
		}, "leading zero"},
		"aliased port": {func(dir string) {
			writeFile(t, filepath.Join(dir, "profiles", "x.yaml"), "id: x\nextra: &p 0673\nendpoints:\n  - host: github.com\n    port: *p\n")
		}, "leading zero"},
		"aliased ports entry": {func(dir string) {
			writeFile(t, filepath.Join(dir, "profiles", "x.yaml"), "id: x\nextra: &p 0673\nendpoints:\n  - host: github.com\n    ports: [443, *p]\n")
		}, "leading zero"},
		"aliased ports list": {func(dir string) {
			writeFile(t, filepath.Join(dir, "profiles", "x.yaml"), "id: x\nextra: &p [0673]\nendpoints:\n  - host: github.com\n    ports: *p\n")
		}, "leading zero"},
		"duplicate profile": {func(dir string) {
			writeFile(t, filepath.Join(dir, "profiles", "a.yaml"), githubProfile)
			writeFile(t, filepath.Join(dir, "profiles", "b.yaml"), githubProfile)
		}, "defined in"},
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			tc.setup(dir)
			if _, err := Load([]string{dir}); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("Load() = %v, want an error containing %q", err, tc.want)
			}
		})
	}
}

func TestLoadRejectsDefinitionsInTwoDirectories(t *testing.T) {
	a, b := t.TempDir(), t.TempDir()
	for _, dir := range []string{a, b} {
		writeFile(t, filepath.Join(dir, "providers", "github.yaml"), githubProvider)
	}
	if _, err := Load([]string{a, b}); err == nil || !strings.Contains(err.Error(), "provider github is defined in") {
		t.Errorf("duplicate provider: %v", err)
	}
	c, d := t.TempDir(), t.TempDir()
	for _, dir := range []string{c, d} {
		writeFile(t, filepath.Join(dir, "policies", "default.yaml"), "")
	}
	if _, err := Load([]string{c, d}); err == nil || !strings.Contains(err.Error(), "default policy is defined in") {
		t.Errorf("duplicate default policy: %v", err)
	}
	if _, err := Load([]string{"relative"}); err == nil {
		t.Error("relative directory accepted")
	}
	if _, err := Load([]string{filepath.Join(a, "missing")}); err == nil {
		t.Error("missing directory accepted")
	}
}

// TestLoadDoesNotEchoPlaintextSecrets checks that a secret written where a
// secret_tool reference belongs stays out of error messages.
func TestLoadDoesNotEchoPlaintextSecrets(t *testing.T) {
	for _, body := range []string{
		"name: p\ntype: github\ncredentials:\n  GH_TOKEN: hunter2pw\n",
		"name: p\ntype: github\ncredentials:\n  GH_TOKEN: ghp_R3allyL0ngT0kenValue\n",
		"name: p\ntype: github\ncredentials:\n  GH_TOKEN:\n    secret_tool: hunter2pw\n",
		"name: p\ntype: github\ncredentials:\n  GH_TOKEN:\n    secret_tool:\n      lookup: hunter2pw\n",
	} {
		dir := t.TempDir()
		writeFile(t, filepath.Join(dir, "providers", "p.yaml"), body)
		_, err := Load([]string{dir})
		if err == nil {
			t.Errorf("Load() accepted:\n%s", body)
			continue
		}
		for _, secret := range []string{"hunter2", "ghp_R3a"} {
			if strings.Contains(err.Error(), secret) {
				t.Errorf("error shows the secret: %v", err)
			}
		}
	}
}

// TestLoadSkipsHiddenFiles checks that an editor's lock file, a dangling
// symbolic link, and hidden copies of a config file do not make Load fail.
func TestLoadSkipsHiddenFiles(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "profiles", "github.yaml"), githubProfile)
	writeFile(t, filepath.Join(dir, "providers", "github.yaml"), githubProvider)
	writeFile(t, filepath.Join(dir, "providers", ".github.yaml"), githubProvider)
	if err := os.Symlink("alice@laptop.12345:1700000000", filepath.Join(dir, "providers", ".#github.yaml")); err != nil {
		t.Fatal(err)
	}
	set, err := Load([]string{dir})
	if err != nil {
		t.Fatalf("Load() with hidden files: %v", err)
	}
	if len(set.Profiles) != 1 || len(set.Providers) != 1 {
		t.Errorf("profiles %d, providers %d; want 1 and 1", len(set.Profiles), len(set.Providers))
	}
	if want := filepath.Join(dir, "providers", "github.yaml"); set.Providers[0].Path != want {
		t.Errorf("provider path %q, want %q", set.Providers[0].Path, want)
	}
}

func TestLoadReadsOnlyRegularFiles(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "profiles"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/dev/zero", filepath.Join(dir, "profiles", "zero.yaml")); err != nil {
		t.Fatal(err)
	}
	if _, err := Load([]string{dir}); err == nil || !strings.Contains(err.Error(), "not a regular file") {
		t.Errorf("symlink to /dev/zero: %v", err)
	}
	fifo := t.TempDir()
	if err := os.MkdirAll(filepath.Join(fifo, "profiles"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(filepath.Join(fifo, "fifo"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(fifo, "fifo"), filepath.Join(fifo, "profiles", "fifo.yaml")); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := Load([]string{fifo})
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "not a regular file") {
			t.Errorf("symlink to a FIFO: %v", err)
		}
	case <-time.After(10 * time.Second):
		// The goroutine stays blocked in open(2); the test binary exits anyway.
		t.Fatal("Load blocked on a symlink to a FIFO")
	}
	big := t.TempDir()
	writeFile(t, filepath.Join(big, "profiles", "big.yaml"), "id: big\n#"+strings.Repeat("x", maxConfigFileSize)+"\n")
	if _, err := Load([]string{big}); err == nil || !strings.Contains(err.Error(), "larger than") {
		t.Errorf("oversized file: %v", err)
	}
}
