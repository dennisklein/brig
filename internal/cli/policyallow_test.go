// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// allowFixture is policyFixture with an openshell that logs the policy
// update and rule approve commands it gets into the returned file. Its
// sandbox has the policy base from the start, which tests give the rule that
// the commands add.
func allowFixture(t *testing.T, base string) (log, policies string) {
	t.Helper()
	log = filepath.Join(t.TempDir(), "openshell.log")
	_, policies = policyFixtureWith(t, base, `
"-g brig-dev --workspace "*" policy update "*|"-g brig-dev --workspace "*" rule approve "*) echo "$*" >>`+log+`; echo "✓ done";;`)
	return log, policies
}

func openshellLog(t *testing.T, log string) string {
	t.Helper()
	data, err := os.ReadFile(log)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	return string(data)
}

const allowBase = `{"filesystem_policy": {"include_workdir": true, "read_only": ["/usr"]},
  "landlock": {"compatibility": "best_effort"},
  "network_policies": {"npm": {"endpoints": [{"host": "registry.npmjs.org", "port": 443, "protocol": "rest", "enforcement": "enforce"}]}, `

func TestPolicyAllowAddsTheRule(t *testing.T) {
	log, _ := allowFixture(t, allowBase+pipRule+`}}`)
	out, _, _, err := runSplit(t, "policy", "allow", "dev", "webapp", "pypi.org", "--binary", "/usr/bin/pip", "--rule", "pip", "--audit")
	if err != nil {
		t.Fatalf("exit = %v\n%s", err, out)
	}
	want := "-g brig-dev --workspace default policy update webapp --rule-name pip --binary /usr/bin/pip --add-endpoint pypi.org:443:read-only:rest:audit --wait"
	if got := strings.TrimSpace(openshellLog(t, log)); got != want {
		t.Errorf("openshell ran\n%s\nwant\n%s", got, want)
	}
	for _, want := range []string{"✓ done", "Only webapp has the rule.", "brig policy save dev webapp --rule pip keeps it for new sandboxes."} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
}

func TestPolicyAllowArguments(t *testing.T) {
	log, _ := allowFixture(t, allowBase+pipRule+`}}`)
	t.Setenv("OPENSHELL_GATEWAY", "brig-dev")
	for _, c := range []struct {
		name string
		args []string
		ran  string // the openshell arguments after the workspace, or "" if it must not run
		err  string
	}{
		{
			"the VM of brig use, a port and a name",
			[]string{"webapp", "Api.Example.com:8443", "--binary", "/usr/bin/curl", "--access", "read-write", "--workspace", "team"},
			"policy update webapp --rule-name allow_api_example_com_8443 --binary /usr/bin/curl --add-endpoint api.example.com:8443:read-write:rest:enforce --wait", "",
		},
		{
			"whole connection",
			[]string{"dev", "webapp", "10.0.2.2", "--binary", "/usr/bin/curl", "--l4", "--broad"},
			"policy update webapp --rule-name allow_10_0_2_2_443 --binary /usr/bin/curl --add-endpoint 10.0.2.2:443 --wait", "",
		},
		{"whole connection unconfirmed", []string{"dev", "webapp", "10.0.2.2", "--binary", "/usr/bin/curl", "--l4"}, "", "this rule is broad: the whole connection is allowed"},
		{"full access unconfirmed", []string{"dev", "webapp", "github.com", "--binary", "/usr/bin/git", "--access", "full"}, "", "full access allows every HTTP method"},
		{"l4 with an access level", []string{"dev", "webapp", "github.com", "--binary", "/usr/bin/git", "--l4", "--access", "read-only"}, "", "--l4 allows the whole connection"},
		{"program wildcard", []string{"dev", "webapp", "github.com", "--binary", "/usr/bin/*"}, "", "the program path has a wildcard"},
		{"any host", []string{"dev", "webapp", "**", "--binary", "/usr/bin/curl"}, "", "a wildcard needs a domain"},
		{"a top-level domain", []string{"dev", "webapp", "*.com:1", "--binary", "/bin/sh", "--l4", "--broad"}, "", "a wildcard needs a domain"},
		{
			"a wildcard below a domain",
			[]string{"dev", "webapp", "*.npmjs.org", "--binary", "/usr/bin/node"},
			"policy update webapp --rule-name allow_wildcard_npmjs_org_443 --binary /usr/bin/node --add-endpoint *.npmjs.org:443:read-only:rest:enforce --wait", "",
		},
		{"no binary", []string{"dev", "webapp", "pypi.org"}, "", "--binary"},
		{"relative binary", []string{"dev", "webapp", "pypi.org", "--binary", "pip"}, "", "full path"},
		{"bad port", []string{"dev", "webapp", "pypi.org:0", "--binary", "/usr/bin/pip"}, "", "write a number from 1 to 65535"},
		{"bad access", []string{"dev", "webapp", "pypi.org", "--binary", "/usr/bin/pip", "--access", "write"}, "", "choose read-only, read-write or full"},
		{"bad rule", []string{"dev", "webapp", "pypi.org", "--binary", "/usr/bin/pip", "--rule", "a b"}, "", "--rule"},
		{"provider rule", []string{"dev", "webapp", "pypi.org", "--binary", "/usr/bin/pip", "--rule", "_provider_x"}, "", "providers"},
		{"to without save", []string{"dev", "webapp", "pypi.org", "--binary", "/usr/bin/pip", "--to", "x"}, "", "--to needs --save"},
		{"sandbox missing", []string{"pypi.org", "--binary", "/usr/bin/pip"}, "", "accepts between 2 and 3 arg(s)"},
		{"workspace", []string{"dev", "webapp", "pypi.org", "--binary", "/usr/bin/pip", "--workspace", "-x"}, "", "workspace"},
		{"draft", []string{"dev", "webapp", "--draft", "ch-1"}, "rule approve webapp --chunk-id ch-1", ""},
		{"draft with a rule of your own", []string{"dev", "webapp", "--draft", "ch-1", "--binary", "/usr/bin/pip"}, "", "--binary describes a rule of your own"},
		{"draft saved without a rule", []string{"dev", "webapp", "--draft", "ch-1", "--save"}, "", "--save with --draft needs --rule"},
	} {
		t.Run(c.name, func(t *testing.T) {
			_ = os.Remove(log)
			_, _, _, err := runSplit(t, append([]string{"policy", "allow"}, c.args...)...)
			if c.err != "" {
				if err == nil || !strings.Contains(err.Error(), c.err) {
					t.Errorf("error = %v, want %q", err, c.err)
				}
			} else if err != nil {
				t.Fatal(err)
			}
			ran := strings.TrimSpace(openshellLog(t, log))
			if ws := "default"; c.ran != "" {
				if strings.Contains(strings.Join(c.args, " "), "--workspace team") {
					ws = "team"
				}
				if want := "-g brig-dev --workspace " + ws + " " + c.ran; ran != want {
					t.Errorf("openshell ran\n%s\nwant\n%s", ran, want)
				}
			} else if ran != "" {
				t.Errorf("openshell ran: %s", ran)
			}
		})
	}
}

func TestPolicyAllowSavesTheRule(t *testing.T) {
	// webapp came from webapp.yaml and has the pip rule after the update.
	log, dir := allowFixture(t, allowBase+pipRule+`}}`)
	file := filepath.Join(dir, "webapp.yaml")
	out, _, _, err := runSplit(t, "policy", "allow", "dev", "webapp", "pypi.org", "--binary", "/usr/bin/pip", "--rule", "pip", "--audit", "--save")
	if err != nil {
		t.Fatalf("exit = %v\n%s", err, out)
	}
	if !strings.Contains(openshellLog(t, log), "policy update webapp --rule-name pip ") {
		t.Error("openshell did not run")
	}
	// The change is shown, then written, without asking.
	for _, want := range []string{"Saving rules pip to " + file, "+  pip:\n", "pip: pypi.org:443 (audit)", "Rule pip only logs", "Saved rules pip to " + file} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	if !strings.Contains(readPolicy(t, file), "  pip:\n") {
		t.Errorf("the file lacks the rule:\n%s", readPolicy(t, file))
	}

	// A second time, the file has the rule already.
	out, _, _, err = runSplit(t, "policy", "allow", "dev", "webapp", "pypi.org", "--binary", "/usr/bin/pip", "--rule", "pip", "--audit", "--save")
	if err != nil || !strings.Contains(out, "Nothing to save: "+file+" has rule pip already.") {
		t.Errorf("exit = %v\n%s", err, out)
	}
}

func TestPolicyAllowSaveRefusesBeforeItChangesTheSandbox(t *testing.T) {
	// other came from default.yaml, which --save does not write into.
	log, dir := allowFixture(t, `{"filesystem_policy": {"include_workdir": true, "read_only": ["/usr"]},
  "landlock": {"compatibility": "best_effort"}, "network_policies": {`+pipRule+`}}`)
	_, _, _, err := runSplit(t, "policy", "allow", "dev", "other", "pypi.org", "--binary", "/usr/bin/pip", "--rule", "pip", "--save")
	if err == nil || !strings.Contains(err.Error(), "--to other") {
		t.Fatalf("error = %v", err)
	}
	if got := openshellLog(t, log); got != "" {
		t.Errorf("openshell ran: %s", got)
	}

	out, _, _, err := runSplit(t, "policy", "allow", "dev", "other", "pypi.org", "--binary", "/usr/bin/pip", "--rule", "pip", "--save", "--to", "other")
	if err != nil || !strings.Contains(out, "Saved rules pip to "+filepath.Join(dir, "other.yaml")) {
		t.Fatalf("exit = %v\n%s", err, out)
	}
	if !strings.HasPrefix(readPolicy(t, filepath.Join(dir, "other.yaml")), testPolicyBase+"network_policies:\n  pip:\n") {
		t.Errorf("other.yaml =\n%s", readPolicy(t, filepath.Join(dir, "other.yaml")))
	}
}

func TestPolicyAllowApprovesAndSavesADraft(t *testing.T) {
	log, dir := allowFixture(t, allowBase+pipRule+`}}`)
	out, _, _, err := runSplit(t, "policy", "allow", "dev", "webapp", "--draft", "ch-9", "--rule", "pip", "--save")
	if err != nil {
		t.Fatalf("exit = %v\n%s", err, out)
	}
	if got := strings.TrimSpace(openshellLog(t, log)); got != "-g brig-dev --workspace default rule approve webapp --chunk-id ch-9" {
		t.Errorf("openshell ran %q", got)
	}
	if !strings.Contains(readPolicy(t, filepath.Join(dir, "webapp.yaml")), "  pip:\n") {
		t.Error("the file lacks the rule")
	}
}
