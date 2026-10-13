// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"

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
