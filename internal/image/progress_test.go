// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

package image

import (
	"testing"

	"github.com/GSI-HPC/go-clikit/progress/progresstest"
)

func TestBuildProgress(t *testing.T) {
	f := newFixture(t, "mkosi 26", "echo 'Installing bash'\necho 'Installing openshell-gateway' >&2\n"+buildOK)
	ctx, w := progresstest.Watch(t.Context(), t)
	if _, _, err := f.s.Build(ctx, t.TempDir(), testOptions); err != nil {
		t.Fatal(err)
	}
	t.Log(w.Tree())
	want := "step build with mkosi [show-lines]: ok\n  target Fedora 44 [show-lines]: ok\nstep install image: ok\n"
	if got := w.Finish(); got != want {
		t.Errorf("progress\n%s\nwant\n%s", got, want)
	}
}

func TestBuildFailureProgress(t *testing.T) {
	f := newFixture(t, "mkosi 26", "echo 'Installing bash'\nexit 1")
	ctx, w := progresstest.Watch(t.Context(), t)
	if _, _, err := f.s.Build(ctx, t.TempDir(), testOptions); err == nil {
		t.Fatal("Build succeeded")
	}
	t.Log(w.Tree())
	want := "step build with mkosi [show-lines]: failed (target): mkosi build: exit status 1\n  target Fedora 44 [show-lines]: failed (target): mkosi build: exit status 1\n"
	if got := w.Finish(); got != want {
		t.Errorf("progress\n%s\nwant\n%s", got, want)
	}
}
