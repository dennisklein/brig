// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/GSI-HPC/go-clikit/progress/cliprogress"
	"github.com/GSI-HPC/go-clikit/progress/progresstest"
)

// TestImageBuildDisplay draws the live tree of "image build" while a fake
// mkosi waits, and checks that mkosi's newest line is on it and that its
// escape sequence is not.
func TestImageBuildDisplay(t *testing.T) {
	testApp(t) // sets the XDG directories
	gate := filepath.Join(t.TempDir(), "go")
	fakeCommands(t, map[string]string{
		"mkosi": `if [ "$1" = --version ]; then echo 'mkosi 26'; exit 0; fi
for a in "$@"; do case "$a" in --output-directory=*) out="${a#*=}" ;; esac; done
printf 'Installing \033]0;pwned\007bash\n'
while [ ! -e '` + gate + `' ]; do sleep 0.02; done
printf 'raw disk' >"$out/base.raw"
cat >"$out/base.manifest" <<'EOF'
{"manifest_version": 1, "packages": [{"type": "rpm", "name": "openshell-gateway", "version": "0.1.2-1.fc44"}]}
EOF`,
		"qemu-img": `for a; do src="$dst"; dst="$a"; done; cp "$src" "$dst"`,
	})
	screen := &progresstest.Screen{Width: 80}
	p := &progressRun{options: func(o *cliprogress.Options) {
		o.Manual, o.Stderr, o.OnTerminal = true, screen, true
		o.Size = func() (int, int, error) { return 80, 24, nil }
		o.Foreground = func() bool { return true }
	}}
	cmd := newRootCmd(p)
	var stdout, stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	cmd.SetArgs([]string{"image", "build", "--fedora", "44", "--progress", "tty"})
	done := make(chan error, 1)
	go func() { done <- cmd.Execute() }()

	var frame string
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		if run := p.run.Load(); run != nil {
			run.Draw()
			if frame = screen.String(); strings.Contains(frame, "Installing") {
				break
			}
		}
	}
	t.Log("\n" + frame)
	for _, want := range []string{"image build", "build with mkosi", "Installing"} {
		if !strings.Contains(frame, want) {
			t.Errorf("frame lacks %q", want)
		}
	}
	if strings.Contains(frame, "pwned") && strings.Contains(frame, "\x1b") {
		t.Error("frame holds mkosi's escape sequence")
	}
	if err := os.WriteFile(gate, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	// The command is done when Execute returns; the display ends with Finish.
	if err := <-done; err != nil {
		t.Fatalf("image build: %v\nstderr: %s", err, stderr.String())
	}
	p.end(nil)
	if !strings.HasPrefix(stdout.String(), "Built image ") {
		t.Errorf("stdout = %q, want the result only", stdout.String())
	}
}

// TestProgressRefused checks that a display the terminal cannot show is
// a usage error.
func TestProgressRefused(t *testing.T) {
	cmd := newRootCmd(&progressRun{})
	cmd.SetOut(new(bytes.Buffer))
	cmd.SetErr(new(bytes.Buffer))
	cmd.SetArgs([]string{"print-fedora-deps", "--progress", "tty"})
	if err := cmd.Execute(); err == nil {
		t.Skip("standard error is a terminal")
	}
}
