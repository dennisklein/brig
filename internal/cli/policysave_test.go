// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// pipRule is a rule that no policy file of policyFixture has, in audit mode.
const pipRule = `"pip": {"endpoints": [{"host": "pypi.org", "ports": [443], "protocol": "rest", "enforcement": "audit"}], "binaries": [{"path": "/usr/bin/pip"}]}`

func readPolicy(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestPolicySaveIntoTheSourceFile(t *testing.T) {
	// webapp came from webapp.yaml and got the pip rule since.
	_, dir := policyFixture(t, `{"filesystem_policy": {"include_workdir": true, "read_only": ["/usr"]},
  "landlock": {"compatibility": "best_effort"},
  "network_policies": {"npm": {"endpoints": [{"host": "registry.npmjs.org", "port": 443, "protocol": "rest", "enforcement": "enforce"}]}, `+pipRule+`}}`)
	file := filepath.Join(dir, "webapp.yaml")

	out, _, _, err := runSplit(t, "policy", "save", "dev", "webapp", "--dry-run")
	if err != nil {
		t.Fatalf("exit = %v\n%s", err, out)
	}
	for _, want := range []string{"Would save rules pip to ", "+  pip:\n", "+        enforcement: audit\n"} {
		if !strings.Contains(out, want) {
			t.Errorf("dry run output lacks %q:\n%s", want, out)
		}
	}
	if readPolicy(t, file) != testPolicyBase+testPolicyNPM {
		t.Fatalf("dry run wrote the file")
	}

	out, _, _, err = runSplit(t, "policy", "save", "dev", "webapp")
	if err != nil {
		t.Fatalf("exit = %v\n%s", err, out)
	}
	for _, want := range []string{"Saved rules pip to " + file, "Rule pip only logs", "openshell policy set NAME --policy " + file} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	want := testPolicyBase + testPolicyNPM + "  pip:\n    endpoints:\n      - host: pypi.org\n        port: 443\n        protocol: rest\n        enforcement: audit\n    binaries:\n      - path: /usr/bin/pip\n"
	if got := readPolicy(t, file); got != want {
		t.Errorf("file =\n%s\nwant\n%s", got, want)
	}
	if out, _, _, err := runSplit(t, "policy", "diff", "dev", "webapp"); err != nil {
		t.Errorf("diff after save: exit = %v\n%s", err, out)
	}
	out, _, _, err = runSplit(t, "policy", "save", "dev", "webapp")
	if err != nil || !strings.Contains(out, "Nothing to save") {
		t.Errorf("second save: exit = %v\n%s", err, out)
	}
}

func TestPolicySaveFromTheDefault(t *testing.T) {
	// other came from default.yaml and got the pip rule since.
	_, dir := policyFixture(t, `{"filesystem_policy": {"include_workdir": true, "read_only": ["/usr"]},
  "landlock": {"compatibility": "best_effort"}, "network_policies": {`+pipRule+`}}`)

	_, _, _, err := runSplit(t, "policy", "save", "dev", "other")
	if err == nil || !strings.Contains(err.Error(), "--to other") {
		t.Fatalf("saved into the default: %v", err)
	}
	if readPolicy(t, filepath.Join(dir, "default.yaml")) != testPolicyBase {
		t.Fatal("default.yaml changed")
	}

	created := filepath.Join(dir, "other.yaml")
	out, _, _, err := runSplit(t, "policy", "save", "dev", "other", "--to", "other", "--dry-run")
	if err != nil || !strings.Contains(out, "Would create "+created+" as a copy of "+filepath.Join(dir, "default.yaml")) {
		t.Fatalf("exit = %v\n%s", err, out)
	}
	if _, err := os.Stat(created); err == nil {
		t.Fatal("dry run created the file")
	}

	if out, _, _, err := runSplit(t, "policy", "save", "dev", "other", "--to", "other"); err != nil {
		t.Fatalf("exit = %v\n%s", err, out)
	}
	if got := readPolicy(t, created); !strings.HasPrefix(got, testPolicyBase+"network_policies:\n  pip:\n") {
		t.Errorf("other.yaml =\n%s", got)
	}
	// The new file is the sandbox's from now on.
	out, _, _, err = runSplit(t, "policy", "diff", "dev", "other", "-o", "json")
	if err != nil {
		t.Fatalf("exit = %v\n%s", err, out)
	}
	if rep := decodeReport(t, out); rep.Source == nil || rep.Source.How != "recorded" || rep.Source.Path != created {
		t.Errorf("report:\n%s", out)
	}
}

func TestPolicySaveRules(t *testing.T) {
	// webapp has npm with another binary than webapp.yaml's, and pip.
	_, dir := policyFixture(t, `{"filesystem_policy": {"include_workdir": true, "read_only": ["/usr"]},
  "landlock": {"compatibility": "best_effort"},
  "network_policies": {"npm": {"endpoints": [{"host": "registry.npmjs.org", "port": 443, "protocol": "rest", "enforcement": "enforce"}], "binaries": [{"path": "/usr/bin/node"}]}, `+pipRule+`}}`)
	file := filepath.Join(dir, "webapp.yaml")
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"--rule", "nope"}, "rule nope of sandbox webapp does not differ"},
		{[]string{"--to", "default", "--rule", "npm"}, ""},
	} {
		_, _, _, err := runSplit(t, append([]string{"policy", "save", "dev", "webapp", "--dry-run"}, tc.args...)...)
		if tc.want == "" {
			if err != nil {
				t.Errorf("%v: %v", tc.args, err)
			}
			continue
		}
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%v: error = %v, want %q", tc.args, err, tc.want)
		}
	}

	// Without --rule, the changed rule stays as the file has it.
	out, _, _, err := runSplit(t, "policy", "save", "dev", "webapp")
	if err != nil || !strings.Contains(out, "Saved rules pip ") {
		t.Fatalf("exit = %v\n%s", err, out)
	}
	if !strings.Contains(readPolicy(t, file), testPolicyNPM) {
		t.Fatal("npm changed without --rule")
	}
	out, _, _, err = runSplit(t, "policy", "save", "dev", "webapp")
	if err != nil || !strings.Contains(out, "These rules differ from the file's: npm") {
		t.Errorf("exit = %v\n%s", err, out)
	}
	if out, _, _, err := runSplit(t, "policy", "save", "dev", "webapp", "--rule", "npm"); err != nil {
		t.Fatalf("exit = %v\n%s", err, out)
	}
	if !strings.Contains(readPolicy(t, file), "    binaries:\n      - path: /usr/bin/node\n  pip:\n") {
		t.Errorf("npm not replaced in place:\n%s", readPolicy(t, file))
	}
}

func TestPolicySaveNotApplied(t *testing.T) {
	// The sandbox lacks webapp.yaml's npm rule.
	policyFixture(t, `{"filesystem_policy": {"include_workdir": true, "read_only": ["/usr"]},
  "landlock": {"compatibility": "best_effort"}, "network_policies": {`+pipRule+`}}`)
	_, _, _, err := runSplit(t, "policy", "save", "dev", "webapp", "--to", "webapp", "--rule", "npm")
	if err == nil || !strings.Contains(err.Error(), "openshell policy set applies the file") {
		t.Errorf("error = %v", err)
	}
}

func TestPolicySaveThroughASymlink(t *testing.T) {
	_, dir := policyFixture(t, `{"filesystem_policy": {"include_workdir": true, "read_only": ["/usr"]},
  "landlock": {"compatibility": "best_effort"},
  "network_policies": {"npm": {"endpoints": [{"host": "registry.npmjs.org", "port": 443, "protocol": "rest", "enforcement": "enforce"}]}, `+pipRule+`}}`)
	file := filepath.Join(dir, "webapp.yaml")
	target := filepath.Join(t.TempDir(), "webapp.yaml")
	if err := os.Rename(file, target); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, file); err != nil {
		t.Fatal(err)
	}
	if out, _, _, err := runSplit(t, "policy", "save", "dev", "webapp"); err != nil {
		t.Fatalf("exit = %v\n%s", err, out)
	}
	if fi, err := os.Lstat(file); err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Errorf("the link was replaced: %v", err)
	}
	if !strings.Contains(readPolicy(t, target), "  pip:\n") {
		t.Error("the link's target was not written")
	}
}

func TestPolicySaveInAWorkspace(t *testing.T) {
	// other, in workspace team, came from default.yaml and got the pip rule.
	choices, dir := policyFixture(t, `{"filesystem_policy": {"include_workdir": true, "read_only": ["/usr"]},
  "landlock": {"compatibility": "best_effort"}, "network_policies": {`+pipRule+`}}`)
	if out, _, _, err := runSplit(t, "policy", "save", "dev", "other", "--workspace", "team", "--to", "other"); err != nil {
		t.Fatalf("exit = %v\n%s", err, out)
	}
	if got := readPolicy(t, choices); !strings.Contains(got, `"team/other": "`+filepath.Join(dir, "other.yaml")+`"`) {
		t.Errorf("record:\n%s", got)
	}
	// default/other has no record, so its source is inferred.
	out, _, _, _ := runSplit(t, "policy", "diff", "dev", "other", "-o", "json")
	if rep := decodeReport(t, out); rep.Workspace != "default" || rep.Source == nil || rep.Source.How == "recorded" {
		t.Errorf("report:\n%s", out)
	}
}
