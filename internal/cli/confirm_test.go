// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/GSI-HPC/go-clikit/progress/cliprogress"

	"github.com/dennisklein/brig/internal/libvirt"
)

func TestConfirmer(t *testing.T) {
	onTerminal(t)
	for _, tc := range []struct {
		in   string
		want bool
	}{
		{"y\n", true},
		{"YES\n", true},
		{" yes \n", true},
		{"y", true},
		{"\n", false},
		{"n\n", false},
		{"no\n", false},
		{"yep\n", false},
		{"", false},
	} {
		var out bytes.Buffer
		ask := confirmer(t.Context(), strings.NewReader(tc.in), &out, false)
		got, err := ask("Go on? [y/N] ")
		if err != nil || got != tc.want || !strings.HasPrefix(out.String(), "Go on? [y/N] ") {
			t.Errorf("answer %q = %v, %v, %q; want %v", tc.in, got, err, out.String(), tc.want)
		}
	}
}

func TestConfirmerEndsWithTheContext(t *testing.T) {
	onTerminal(t)
	ctx, cancel := context.WithCancel(t.Context())
	pr, pw := io.Pipe()
	defer func() { _ = pw.Close() }()
	cancel()
	if got, err := confirmer(ctx, pr, io.Discard, false)("Go on? "); got || err == nil {
		t.Errorf("answer after cancel = %v, %v", got, err)
	}
}

func TestConfirmerStaysOutOfTheWay(t *testing.T) {
	// Without a terminal, and with --yes, nothing is asked.
	if confirmer(t.Context(), strings.NewReader("y\n"), io.Discard, false) != nil {
		t.Error("confirmer asks without a terminal")
	}
	onTerminal(t)
	if confirmer(t.Context(), strings.NewReader("y\n"), io.Discard, true) != nil {
		t.Error("confirmer asks despite --yes")
	}
}

func TestRemoveAsksBeforeDeleting(t *testing.T) {
	a := testApp(t)
	v := testVM(t, a, "dev")
	t.Setenv("PATH", t.TempDir())
	var stdout, stderr bytes.Buffer
	var asked libvirt.State
	refuse := func(s libvirt.State) error { asked = s; return io.ErrClosedPipe }
	conn := &fakeDomains{state: libvirt.StateRunning}
	if err := a.remove(t.Context(), conn, v, true, refuse, &stdout, &stderr); !errors.Is(err, io.ErrClosedPipe) {
		t.Fatalf("remove: %v", err)
	}
	if asked != libvirt.StateRunning || !a.vms.Exists("dev") || stdout.Len() != 0 {
		t.Errorf("asked about %v, VM exists %v, stdout %q", asked, a.vms.Exists("dev"), &stdout)
	}
	// A VM that cannot be deleted anyway is not asked about.
	asked = libvirt.StateShutoff
	if err := a.remove(t.Context(), conn, v, false, refuse, &stdout, &stderr); err == nil || !strings.Contains(err.Error(), "stop it first") || asked != libvirt.StateShutoff {
		t.Errorf("remove of a running VM without --force: %v, asked %v", err, asked)
	}
}

func TestConfirmDeleteNamesWhatGoes(t *testing.T) {
	a := testApp(t)
	v := testVM(t, a, "dev")
	var errOut bytes.Buffer
	answer := func(reply string) func(string) (bool, error) {
		return func(prompt string) (bool, error) {
			errOut.WriteString(prompt)
			return reply == "y", nil
		}
	}
	err := confirmDelete(&errOut, answer("n"), v, libvirt.StateRunning)
	if err == nil || !strings.Contains(err.Error(), "VM dev not deleted") {
		t.Errorf("answer n: %v", err)
	}
	for _, want := range []string{"root disk (20GiB)", "data disk (10GiB)", "secrets", "dev is running", "Delete VM dev? [y/N]"} {
		if !strings.Contains(errOut.String(), want) {
			t.Errorf("the question lacks %q:\n%s", want, &errOut)
		}
	}
	if err := confirmDelete(&errOut, answer("y"), v, libvirt.StateShutoff); err != nil {
		t.Errorf("answer y: %v", err)
	}
}

func TestDeleteHasYes(t *testing.T) {
	a := testApp(t)
	testVM(t, a, "dev")
	if out, err := run(t, "delete", "--help"); err != nil || !strings.Contains(out, "-y, --yes") {
		t.Errorf("delete --help: %v\n%s", err, out)
	}
}

// TestAuditPruneAsksOnAPseudoTerminal runs brig audit prune through the root
// command on a pseudo-terminal, with brig's own terminal check: the writer
// of the progress display around standard error must not keep brig from
// asking.
func TestAuditPruneAsksOnAPseudoTerminal(t *testing.T) {
	const prompt = "Delete it? [y/N]"
	for _, tc := range []struct {
		mode, answer string
		deleted      bool
	}{
		{"tty", "y", true},
		{"tty", "n", false},
		{"plain", "y", true},
		{"none", "n", false},
	} {
		t.Run(tc.mode+" "+tc.answer, func(t *testing.T) {
			a := auditFixture(t)
			master, slave := openPTY(t)
			var mu sync.Mutex
			var screen bytes.Buffer
			go func() {
				buf := make([]byte, 4096)
				for {
					n, err := master.Read(buf)
					mu.Lock()
					screen.Write(buf[:n])
					mu.Unlock()
					if err != nil {
						return
					}
				}
			}()
			shown := func() string {
				mu.Lock()
				defer mu.Unlock()
				return screen.String()
			}
			p := &progressRun{options: func(o *cliprogress.Options) {
				o.Stderr, o.OnTerminal = slave, true
				o.Size = func() (int, int, error) { return 80, 24, nil }
				o.Foreground = func() bool { return true }
			}}
			cmd := newRootCmd(p)
			var stdout bytes.Buffer
			cmd.SetOut(&stdout)
			cmd.SetErr(slave)
			cmd.SetIn(slave)
			cmd.SetArgs([]string{"audit", "prune", "--deleted-vms", "--progress", tc.mode, "--progress-log", ""})
			done := make(chan error, 1)
			go func() {
				err := cmd.Execute()
				p.end(err)
				done <- err
			}()
			var err error
			asked := false
		wait:
			for deadline := time.After(10 * time.Second); ; {
				select {
				case err = <-done:
					break wait
				case <-deadline:
					t.Fatalf("brig did not end:\n%q", shown())
				case <-time.After(20 * time.Millisecond):
					if !asked && strings.Contains(shown(), prompt) {
						asked = true
						_, _ = master.WriteString(tc.answer + "\n")
					}
				}
			}
			vms, _ := a.auditStore().VMs()
			if !asked || (err == nil) != tc.deleted || (len(vms) == 1) != tc.deleted {
				t.Errorf("asked %v, %v, VMs left %v\n%q", asked, err, vms, shown())
			}
		})
	}
}
