// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"bytes"
	"strings"
	"testing"
)

func run(t *testing.T, args ...string) (string, error) {
	t.Helper()
	cmd := newRootCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs(args)
	err := cmd.Execute()
	return out.String(), err
}

func TestPrintFedoraDeps(t *testing.T) {
	out, err := run(t, "print-fedora-deps")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(out, "\n") != 1 || !strings.Contains(out, "passt") || strings.Contains(out, "virtiofsd") {
		t.Fatalf("output = %q", out)
	}
	out, err = run(t, "print-fedora-deps", "--with", "mounts", "--explain")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(out, "GROUP") || !strings.Contains(out, "virtiofsd") {
		t.Fatalf("output = %q", out)
	}
	if _, err := run(t, "print-fedora-deps", "--with", "nope"); err == nil {
		t.Fatal("unknown group accepted")
	}
}
