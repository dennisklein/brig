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
	if data, _ := os.ReadFile(log); string(data) != "lookup service github.com\n" {
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
		"not found":    {"exit 1", true, "secret-tool store --label=LABEL service x"},
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
