// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

package packaging

import (
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

const dummySpec = `Name: dummy
Version: %{ver}
Release: %{rel}
Summary: dummy
License: Apache-2.0
BuildArch: noarch

%description
dummy

%files
`

// pythonWithRPM returns an interpreter that can import the rpm module.
func pythonWithRPM(t *testing.T) string {
	t.Helper()
	for _, py := range []string{"python3", "python3.12"} {
		if exec.Command(py, "-I", "-c", "import rpm").Run() == nil {
			return py
		}
	}
	t.Skip("no python with the rpm module")
	return ""
}

func buildDummy(t *testing.T, top, dir, ver, rel string) {
	t.Helper()
	spec := filepath.Join(top, "dummy.spec")
	if err := os.WriteFile(spec, []byte(dummySpec), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("rpmbuild", "-bb", "--define", "_topdir "+top,
		"--define", "ver "+ver, "--define", "rel "+rel, spec)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("rpmbuild %s-%s: %v\n%s", ver, rel, err, out)
	}
	rpm := filepath.Join(top, "RPMS", "noarch", "dummy-"+ver+"-"+rel+".noarch.rpm")
	if err := os.Rename(rpm, filepath.Join(dir, filepath.Base(rpm))); err != nil {
		t.Fatal(err)
	}
}

func TestPruneRepo(t *testing.T) {
	if _, err := exec.LookPath("rpmbuild"); err != nil {
		t.Skip("rpmbuild not installed")
	}
	py := pythonWithRPM(t)

	top, dir := t.TempDir(), t.TempDir()
	for _, evr := range [][2]string{{"0.1.1", "1"}, {"0.1.2", "1"}, {"0.1.3", "1"}, {"0.1.3", "2"}} {
		buildDummy(t, top, dir, evr[0], evr[1])
	}

	cmd := exec.Command(py, "-I", "prune-repo.py", "--keep", "2", dir)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("prune-repo.py: %v\n%s", err, out)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, e := range entries {
		got = append(got, e.Name())
	}
	sort.Strings(got)
	// The release bump 0.1.3-2 replaces 0.1.3-1 instead of pushing 0.1.2 out.
	want := "dummy-0.1.2-1.noarch.rpm dummy-0.1.3-2.noarch.rpm"
	if strings.Join(got, " ") != want {
		t.Errorf("kept %q, want %q", got, want)
	}
}
