// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeCommands puts shell scripts named after the keys of scripts first in
// PATH.
func fakeCommands(t *testing.T, scripts map[string]string) {
	t.Helper()
	dir := t.TempDir()
	for name, script := range scripts {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/sh\n"+script+"\n"), 0o700); err != nil { //nolint:gosec // G306: scripts must be executable
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func TestCompletePushImage(t *testing.T) {
	a := testApp(t)
	testVM(t, a, "dev")
	fakeCommands(t, map[string]string{"podman": `printf 'localhost/agent:latest\n<none>:<none>\n'`})
	out, err := run(t, "__complete", "image", "push", "dev", "")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(out, "localhost/agent:latest\n:4\n") {
		t.Fatalf("completion = %q, want the host's tagged images", out)
	}
}

func TestPushImage(t *testing.T) {
	// podman save writes more than a pipe holds.
	const save = "exec head -c 4000000 /dev/zero"
	for _, tc := range []struct {
		name, podman, ssh, wantErr string
	}{
		{"ok", save, "exec cat >/dev/null", ""},
		{"VM down", save, "echo 'ssh: connect to host 127.0.0.1: Connection refused' >&2; exit 255", "loading localhost/agent:latest into dev"},
		{"load fails", save, "head -c 1000 >/dev/null; exit 125", "loading localhost/agent:latest into dev"},
		{"save fails", "exit 125", "exec cat >/dev/null", "podman save localhost/agent:latest failed"},
		// Real podman load rejects the empty stream a failed save leaves.
		{"save fails, load rejects empty", "echo 'Error: image not known' >&2; exit 125", `[ "$(wc -c)" -gt 0 ] || exit 125`, "podman save localhost/agent:latest failed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := testApp(t)
			v := testVM(t, a, "dev")
			fakeCommands(t, map[string]string{"podman": tc.podman, "ssh": tc.ssh})
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			cmd := newImagePushCmd()
			cmd.SetOut(io.Discard)
			cmd.SetErr(io.Discard)
			err := a.pushImage(ctx, v, "localhost/agent:latest", cmd)
			if ctx.Err() != nil {
				t.Fatalf("pushImage() hung: %v", err)
			}
			switch {
			case tc.wantErr == "" && err != nil:
				t.Fatalf("pushImage() = %v", err)
			case tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)):
				t.Fatalf("pushImage() = %v, want an error containing %q", err, tc.wantErr)
			}
		})
	}
}
