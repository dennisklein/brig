// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

const testPolicyBase = `version: 1
filesystem_policy:
  include_workdir: true
  read_only: [/usr]
landlock:
  compatibility: best_effort
`

const testPolicyNPM = `network_policies:
  npm:
    endpoints:
      - host: registry.npmjs.org
        port: 443
        protocol: rest
        enforcement: enforce
`

// sandboxBase is a base policy as a gateway might return it: default.yaml
// plus the npm rule, with a port list and zero values spelled out.
const sandboxBase = `{"filesystem_policy": {"include_workdir": true, "read_only": ["/usr"]},
  "landlock": {"compatibility": "best_effort"}, "process": {"run_as_user": ""},
  "network_policies": {"npm": {"endpoints": [{"host": "registry.npmjs.org", "ports": [443], "protocol": "rest", "enforcement": "enforce"}]}}}`

// policyFixture sets up VM dev with a config directory holding default.yaml
// and webapp.yaml, and an openshell that prints sandboxPolicy (JSON) as the
// base policy of any sandbox. It returns the VM's state directory file for
// the sandbox choices and the config directory's policies directory.
func policyFixture(t *testing.T, sandboxPolicy string) (choices, policies string) {
	t.Helper()
	a := testApp(t)
	v := testVM(t, a, "dev")
	dir := t.TempDir()
	writeTestFile(t, filepath.Join(dir, "policies", "default.yaml"), testPolicyBase, 0o600)
	writeTestFile(t, filepath.Join(dir, "policies", "webapp.yaml"), testPolicyBase+testPolicyNPM, 0o600)
	v.OpenShellConfigs = []string{dir}
	if err := a.vms.Save(v); err != nil {
		t.Fatal(err)
	}
	t.Setenv("OPENSHELL_GATEWAY", "")
	fakeCommands(t, map[string]string{"openshell": `
case "$*" in
"-g brig-dev --workspace default policy get "*" --base -o json"|"-g brig-dev --workspace team policy get "*" --base -o json") cat <<'JSON'
{"version": 4, "policy": ` + sandboxPolicy + `}
JSON
;;
*) echo "unexpected: $*" >&2; exit 3;;
esac`})
	return a.vmFile(v, policySourcesFile), filepath.Join(dir, "policies")
}

func exitCode(err error) int {
	var e exitError
	if errors.As(err, &e) {
		return e.code
	}
	if err != nil {
		return -1
	}
	return 0
}

func decodeReport(t *testing.T, out string) policyReport {
	t.Helper()
	var rep policyReport
	if err := json.Unmarshal([]byte(out), &rep); err != nil {
		t.Fatalf("%v:\n%s", err, out)
	}
	return rep
}

func TestPolicyDiffInfersTheSource(t *testing.T) {
	// The sandbox came from webapp.yaml, and got another rule since, in
	// audit mode.
	policyFixture(t, `{"filesystem_policy": {"include_workdir": true, "read_only": ["/usr"]},
  "landlock": {"compatibility": "best_effort"},
  "network_policies": {"npm": {"endpoints": [{"host": "registry.npmjs.org", "port": 443, "protocol": "rest", "enforcement": "enforce"}]},
                       "extra": {"endpoints": [{"host": "example.org", "enforcement": "audit"}]}}}`)
	out, _, _, err := runSplit(t, "policy", "diff", "dev", "webapp", "-o", "json")
	if exitCode(err) != exitDrift {
		t.Fatalf("exit = %v, want drift", err)
	}
	rep := decodeReport(t, out)
	if rep.Source == nil || filepath.Base(rep.Source.Path) != "webapp.yaml" || rep.Source.How != "inferred" {
		t.Errorf("source = %+v\n%s", rep.Source, out)
	}
	if len(rep.Rules) != 1 || rep.Rules[0].Key != "extra" || rep.Rules[0].State != "unsaved" || !rep.Rules[0].Audit || !rep.Drift || len(rep.Fixed) != 0 {
		t.Errorf("report:\n%s", out)
	}

	text, _, _, err := runSplit(t, "policy", "diff", "dev", "webapp")
	if exitCode(err) != exitDrift {
		t.Fatalf("exit = %v", err)
	}
	for _, want := range []string{"compared with ", "webapp.yaml (inferred)", "unsaved", "extra", "audit mode"} {
		if !strings.Contains(text, want) {
			t.Errorf("output lacks %q:\n%s", want, text)
		}
	}
}

func TestPolicyDiffInSync(t *testing.T) {
	policyFixture(t, sandboxBase)
	out, _, _, err := runSplit(t, "policy", "diff", "dev", "webapp")
	if err != nil {
		t.Fatalf("exit = %v\n%s", err, out)
	}
	if !strings.Contains(out, "No difference.") {
		t.Errorf("output:\n%s", out)
	}
}

func TestPolicyDiffYAMLText(t *testing.T) {
	// A gateway may return the policy as YAML text.
	policyFixture(t, `"version: 1\nfilesystem_policy:\n  include_workdir: true\n  read_only: [/usr]\nlandlock:\n  compatibility: best_effort\n"`)
	if out, _, _, err := runSplit(t, "policy", "diff", "dev", "other"); err != nil {
		t.Errorf("exit = %v, want no difference with default.yaml\n%s", err, out)
	}
}

func TestPolicyDiffSourceUnknownThenChosen(t *testing.T) {
	// Neither file has the sandbox's filesystem section.
	policyFixture(t, `{"filesystem_policy": {"read_only": ["/"]}, "network_policies": {}}`)
	out, _, _, err := runSplit(t, "policy", "diff", "dev", "webapp", "-o", "json")
	if exitCode(err) != exitSourceUnknown {
		t.Fatalf("exit = %v, want source unknown\n%s", err, out)
	}
	if rep := decodeReport(t, out); rep.Source != nil || len(rep.Candidates) != 2 || rep.Drift {
		t.Errorf("report:\n%s", out)
	}
	text, _, _, _ := runSplit(t, "policy", "diff", "dev", "webapp")
	if !strings.Contains(text, "Choose one with --source") {
		t.Errorf("output:\n%s", text)
	}

	// Choosing a file compares with it; its fixed sections differ.
	out, _, _, err = runSplit(t, "policy", "diff", "dev", "webapp", "--source", "default", "--remember", "-o", "json")
	if exitCode(err) != exitDrift {
		t.Fatalf("exit = %v\n%s", err, out)
	}
	if rep := decodeReport(t, out); rep.Source == nil || rep.Source.How != "given" || len(rep.Fixed) == 0 {
		t.Errorf("report:\n%s", out)
	}
	// The choice is kept for that sandbox only, and wins over inference.
	out, _, _, _ = runSplit(t, "policy", "diff", "dev", "webapp", "-o", "json")
	if rep := decodeReport(t, out); rep.Source == nil || rep.Source.How != "recorded" || filepath.Base(rep.Source.Path) != "default.yaml" {
		t.Errorf("recorded choice not used:\n%s", out)
	}
	out, _, _, _ = runSplit(t, "policy", "diff", "dev", "other", "-o", "json")
	if rep := decodeReport(t, out); rep.Source != nil {
		t.Errorf("the choice for webapp applies to another sandbox:\n%s", out)
	}
}

func TestPolicyDiffGivesUpAGoneRecordedFile(t *testing.T) {
	choices, _ := policyFixture(t, sandboxBase)
	writeTestFile(t, choices, `{"default/webapp": "/nonexistent/webapp.yaml"}`, 0o600)
	out, stderr, _, err := runSplit(t, "policy", "diff", "dev", "webapp", "-o", "json")
	if err != nil {
		t.Fatalf("exit = %v\n%s", err, out)
	}
	if rep := decodeReport(t, out); rep.Source == nil || rep.Source.How != "inferred" {
		t.Errorf("a recorded file that is gone was not given up:\n%s", out)
	}
	if !strings.Contains(stderr, "is gone") {
		t.Errorf("no warning:\n%s", stderr)
	}
}

func TestPolicyDiffIgnoresRecordsFromBeforeWorkspaces(t *testing.T) {
	choices, dir := policyFixture(t, sandboxBase)
	writeTestFile(t, choices, `{"webapp": "`+filepath.Join(dir, "default.yaml")+`"}`, 0o600)
	out, _, _, err := runSplit(t, "policy", "diff", "dev", "webapp", "-o", "json")
	if err != nil {
		t.Fatalf("exit = %v\n%s", err, out)
	}
	if rep := decodeReport(t, out); rep.Source == nil || rep.Source.How != "inferred" {
		t.Errorf("an old record was used:\n%s", out)
	}
}

func TestPolicyDiffKeysChoicesByWorkspace(t *testing.T) {
	choices, dir := policyFixture(t, `{"filesystem_policy": {"read_only": ["/"]}, "network_policies": {}}`)
	out, _, _, err := runSplit(t, "policy", "diff", "dev", "webapp", "--workspace", "team", "--source", "default", "--remember", "-o", "json")
	if exitCode(err) != exitDrift {
		t.Fatalf("exit = %v\n%s", err, out)
	}
	if rep := decodeReport(t, out); rep.Workspace != "team" || rep.Sandbox != "webapp" {
		t.Errorf("report:\n%s", out)
	}
	if got := readPolicy(t, choices); !strings.Contains(got, `"team/webapp": "`+filepath.Join(dir, "default.yaml")+`"`) {
		t.Errorf("record:\n%s", got)
	}
	// The same name in another workspace has no record.
	out, _, _, _ = runSplit(t, "policy", "diff", "dev", "webapp", "-o", "json")
	if rep := decodeReport(t, out); rep.Workspace != "default" || rep.Source != nil {
		t.Errorf("the choice for team applies to default:\n%s", out)
	}
	out, _, _, _ = runSplit(t, "policy", "diff", "dev", "webapp", "--workspace", "team", "-o", "json")
	if rep := decodeReport(t, out); rep.Source == nil || rep.Source.How != "recorded" {
		t.Errorf("the choice for team is gone:\n%s", out)
	}
	if _, _, _, err := runSplit(t, "policy", "diff", "dev", "webapp", "--workspace", "--rule"); err == nil || !strings.Contains(err.Error(), "invalid workspace name") {
		t.Errorf("workspace named like an option: %v", err)
	}
}

func TestPolicyDiffArguments(t *testing.T) {
	policyFixture(t, `{}`)
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"policy", "diff"}, "accepts between 1 and 2"},
		{[]string{"policy", "diff", "webapp"}, "name the VM"},
		{[]string{"policy", "diff", "dev", "webapp", "--remember"}, "--remember needs --source"},
		{[]string{"policy", "diff", "dev", "--", "-bad"}, "invalid sandbox name"},
		{[]string{"policy", "diff", "dev", "webapp", "--source", "nope"}, `no policy file "nope"`},
		{[]string{"policy", "diff", "nodev", "webapp"}, "nodev"},
	} {
		_, _, _, err := runSplit(t, tc.args...)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("brig %s: error = %v, want %q", strings.Join(tc.args, " "), err, tc.want)
		}
	}
}

func TestPolicyDiffUsesTheSelectedVM(t *testing.T) {
	policyFixture(t, sandboxBase)
	t.Setenv("OPENSHELL_GATEWAY", "brig-dev")
	if out, _, _, err := runSplit(t, "policy", "diff", "webapp"); err != nil {
		t.Fatalf("exit = %v\n%s", err, out)
	}
}
