// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

package llmproxy

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestRequirementsPinned checks that the lock file installs the LiteLLM
// version that brig names, and every package at one version with hashes.
func TestRequirementsPinned(t *testing.T) {
	data, err := imageFS.ReadFile("image/requirements.txt")
	if err != nil {
		t.Fatal(err)
	}
	pkgRE := regexp.MustCompile(`^([A-Za-z0-9._-]+(?:\[[a-z,]+\])?)==([^ ;\\]+)`)
	var pkg string
	hashes := map[string]int{}
	for _, line := range strings.Split(string(data), "\n") {
		switch {
		case pkgRE.MatchString(line):
			m := pkgRE.FindStringSubmatch(line)
			pkg = m[1] + "==" + m[2]
			hashes[pkg] = 0
		case strings.HasPrefix(strings.TrimSpace(line), "--hash=sha256:"):
			hashes[pkg]++
		case strings.TrimSpace(line) == "" || strings.HasPrefix(strings.TrimSpace(line), "#"):
		default:
			t.Errorf("unexpected line %q", line)
		}
	}
	if _, ok := hashes["litellm=="+LiteLLMVersion]; !ok {
		t.Errorf("requirements.txt does not pin litellm==%s", LiteLLMVersion)
	}
	for p, n := range hashes {
		if n == 0 {
			t.Errorf("%s has no hash", p)
		}
	}
}

func TestContainerfilePinsBases(t *testing.T) {
	data, err := imageFS.ReadFile("image/Containerfile")
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, "FROM ") && !regexp.MustCompile(`^FROM \S+@sha256:[0-9a-f]{64}( AS \w+)?$`).MatchString(line) {
			t.Errorf("base image not pinned by digest: %s", line)
		}
	}
	if !strings.Contains(string(data), "--require-hashes") {
		t.Error("pip install does not require hashes")
	}
}

func TestWriteImageContext(t *testing.T) {
	dir := t.TempDir()
	if err := WriteImageContext(dir); err != nil {
		t.Fatal(err)
	}
	for name, mode := range imageFiles {
		fi, err := os.Stat(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		if fi.Mode().Perm() != mode {
			t.Errorf("%s has mode %v, want %v", name, fi.Mode().Perm(), mode)
		}
	}
	ref := ImageRef()
	if !regexp.MustCompile(`^localhost/brig-llm-proxy:`+regexp.QuoteMeta(LiteLLMVersion)+`-[0-9a-f]{12}$`).MatchString(ref) || ref != ImageRef() {
		t.Errorf("ImageRef() = %q", ref)
	}
}
