// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

package ocsync

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeSecretTool writes an executable that runs body, after logging its
// arguments.
func fakeSecretTool(t *testing.T, body string) (path, log string) {
	t.Helper()
	dir := t.TempDir()
	path, log = filepath.Join(dir, "secret-tool"), filepath.Join(dir, "log")
	script := "#!/bin/sh\necho \"$*\" >>'" + log + "'\n" + body + "\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil { //nolint:gosec // G306: the fake must be executable
		t.Fatal(err)
	}
	return path, log
}

func TestSecretToolLookup(t *testing.T) {
	path, log := fakeSecretTool(t, `printf 's3cret\n'`)
	got, err := SecretTool{Path: path}.Lookup(context.Background(), []string{"service", "github.com"})
	if err != nil || got != "s3cret" {
		t.Fatalf("Lookup() = %q, %v", got, err)
	}
	if data, _ := os.ReadFile(log); string(data) != "lookup application brig service github.com\n" {
		t.Errorf("argv: %q", data)
	}

	// Only one trailing newline belongs to the output.
	path, _ = fakeSecretTool(t, `printf 'two\n\n'`)
	if got, _ := (SecretTool{Path: path}).Lookup(context.Background(), []string{"a", "b"}); got != "two\n" {
		t.Errorf("Lookup() = %q", got)
	}
}

func TestSecretToolLookupFailures(t *testing.T) {
	for name, tc := range map[string]struct {
		body     string
		notFound bool
		want     string
	}{
		"not found":    {"exit 1", true, "secret-tool store --label=LABEL application brig service x"},
		"empty":        {"exit 0", true, "no such secret"},
		"error":        {"echo 'Cannot autolaunch D-Bus without X11' >&2; echo leaked; exit 1", false, "D-Bus"},
		"silent crash": {"echo leaked; exit 3", false, "exit status 3"},
	} {
		t.Run(name, func(t *testing.T) {
			path, _ := fakeSecretTool(t, tc.body)
			_, err := SecretTool{Path: path}.Lookup(context.Background(), []string{"service", "x"})
			if err == nil || !strings.Contains(err.Error(), tc.want) || errors.Is(err, ErrSecretNotFound) != tc.notFound {
				t.Errorf("Lookup() error = %v", err)
			}
			if err != nil && strings.Contains(err.Error(), "leaked") {
				t.Errorf("error shows secret-tool's standard output: %v", err)
			}
		})
	}

	path, _ := fakeSecretTool(t, "sleep 5")
	start := time.Now()
	if _, err := (SecretTool{Path: path, Timeout: 100 * time.Millisecond}).Lookup(context.Background(), []string{"a", "b"}); err == nil || time.Since(start) > 3*time.Second {
		t.Errorf("timeout: %v after %s", err, time.Since(start))
	}

	if _, err := (SecretTool{Path: "brig-no-such-secret-tool"}).Lookup(context.Background(), []string{"a", "b"}); err == nil || !strings.Contains(err.Error(), "openshell.secret_tool") {
		t.Errorf("missing tool: %v", err)
	}
}

func TestSecretToolStoreHint(t *testing.T) {
	// Attributes that are not plain words are quoted, so pasting the hint
	// stores the secret under the same attributes.
	path, _ := fakeSecretTool(t, "exit 1")
	_, err := SecretTool{Path: path}.Lookup(context.Background(), []string{"service", "GitHub token", "user", "alice's", "x", "$(echo hi)"})
	want := `store it with: secret-tool store --label=LABEL application brig service 'GitHub token' user 'alice'\''s' x '$(echo hi)'`
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Errorf("Lookup() error = %v, want %q", err, want)
	}

	// Another program does not store into libsecret, so the hint names it.
	path, _ = fakeSecretTool(t, "exit 1")
	other := filepath.Join(t.TempDir(), "pass-lookup")
	if err := os.Rename(path, other); err != nil {
		t.Fatal(err)
	}
	_, err = SecretTool{Path: other}.Lookup(context.Background(), []string{"service", "x"})
	if err == nil || !strings.Contains(err.Error(), "store it where "+other+" looks") || strings.Contains(err.Error(), "secret-tool") {
		t.Errorf("Lookup() error = %v", err)
	}
}

func TestSecretToolStore(t *testing.T) {
	dir := t.TempDir()
	in := filepath.Join(dir, "stdin")
	path, log := fakeSecretTool(t, "cat >'"+in+"'")
	secret := "s3cret\nline two"
	err := SecretTool{Path: path}.Store(context.Background(), "brig: github GH_TOKEN", []string{"service", "github.com", "user", "alice smith"}, secret)
	if err != nil {
		t.Fatal(err)
	}
	// The label and attributes are arguments; the secret is not.
	if data, _ := os.ReadFile(log); string(data) != "store --label=brig: github GH_TOKEN application brig service github.com user alice smith\n" {
		t.Errorf("argv: %q", data)
	}
	if data, _ := os.ReadFile(in); string(data) != secret {
		t.Errorf("stdin: %q", data)
	}
}

func TestSecretToolStoreFailures(t *testing.T) {
	attrs := []string{"service", "GitHub token", "user", "alice's"}
	for name, tc := range map[string]struct {
		body string
		want string
	}{
		"no store verb": {"echo \"unknown verb: $1\" >&2; exit 2", "unknown verb: store"},
		"echoes secret": {"echo \"cannot store $(cat)\" >&2; exit 1", "cannot store "},
		"silent":        {"echo leaked; exit 3", "exit status 3"},
	} {
		t.Run(name, func(t *testing.T) {
			path, _ := fakeSecretTool(t, tc.body)
			err := SecretTool{Path: path}.Store(context.Background(), "L", attrs, "hunter2")
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Store() error = %v, want %q", err, tc.want)
			}
			// The attributes come quoted, to paste into a shell.
			if !strings.Contains(err.Error(), `service 'GitHub token' user 'alice'\''s'`) {
				t.Errorf("error lacks the attributes: %v", err)
			}
			for _, leak := range []string{"hunter2", "leaked"} {
				if strings.Contains(err.Error(), leak) {
					t.Errorf("error shows %q: %v", leak, err)
				}
			}
		})
	}

	path, _ := fakeSecretTool(t, "sleep 5")
	start := time.Now()
	if err := (SecretTool{Path: path, Timeout: 100 * time.Millisecond}).Store(context.Background(), "L", attrs, "x"); err == nil || time.Since(start) > 3*time.Second {
		t.Errorf("timeout: %v after %s", err, time.Since(start))
	}
	if err := (SecretTool{Path: "brig-no-such-secret-tool"}).Store(context.Background(), "L", attrs, "x"); err == nil || !strings.Contains(err.Error(), "openshell.secret_tool") {
		t.Errorf("missing tool: %v", err)
	}
}

func TestLookupEnv(t *testing.T) {
	t.Setenv("BRIG_SECRET_X", "v\n")
	if got, err := LookupEnv("BRIG_SECRET_X"); err != nil || got != "v\n" {
		t.Errorf("LookupEnv() = %q, %v", got, err)
	}
	t.Setenv("BRIG_SECRET_EMPTY", "")
	if _, err := LookupEnv("BRIG_SECRET_EMPTY"); !errors.Is(err, ErrSecretNotFound) {
		t.Errorf("empty: %v", err)
	}
	t.Setenv("AWS_SECRET_ACCESS_KEY", "k")
	if _, err := LookupEnv("AWS_SECRET_ACCESS_KEY"); err == nil || !strings.Contains(err.Error(), "must start with BRIG_SECRET_") {
		t.Errorf("unrelated: %v", err)
	}
	t.Setenv("BRIG_SECRET_BIG", strings.Repeat("x", maxSecretSize+1))
	if _, err := LookupEnv("BRIG_SECRET_BIG"); err == nil || !strings.Contains(err.Error(), "larger than") {
		t.Errorf("big: %v", err)
	}
}
