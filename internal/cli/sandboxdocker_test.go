// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/dennisklein/brig/internal/guest"
	"github.com/dennisklein/brig/internal/policy"
	"github.com/dennisklein/brig/internal/vm"
)

// execPolicy returns the policy that the exec'd openshell argv reads with
// --policy, and argv with the path in place of the file.
func execPolicy(t *testing.T, argv []string) (*policy.Doc, string, []string) {
	t.Helper()
	i := slices.Index(argv, "--policy")
	if i < 0 || i+1 == len(argv) {
		t.Fatalf("no --policy in %q", argv)
	}
	if !strings.HasPrefix(argv[i+1], "/proc/self/fd/") {
		t.Errorf("--policy %s is not a memfd", argv[i+1])
	}
	data, err := os.ReadFile(argv[i+1])
	if err != nil {
		t.Fatal(err)
	}
	doc, err := policy.Parse(data)
	if err != nil {
		t.Fatalf("%v:\n%s", err, data)
	}
	return doc, string(data), slices.Replace(slices.Clone(argv), i+1, i+2, "FILE")
}

func TestSandboxCreateDocker(t *testing.T) {
	a := testApp(t)
	testVM(t, a, "dev")
	got := captureExec(t)
	if out, err := run(t, "sandbox", "create", "dev", "--docker", "--", "--name", "tests", "--from", "img", "--", "pytest"); err != nil {
		t.Fatalf("brig sandbox create: %v: %s", err, out)
	}
	doc, _, argv := execPolicy(t, *got)
	want := []string{
		"openshell", "openshell", "-g", "brig-dev", "--workspace", "default", "sandbox", "create",
		"--policy", "FILE", "--label", "brig.dev/docker=true",
		"--env", "DOCKER_HOST=tcp://host.openshell.internal:2375",
		"--env", "TESTCONTAINERS_HOST_OVERRIDE=host.openshell.internal",
		"--env", "TESTCONTAINERS_DOCKER_SOCKET_OVERRIDE=/run/user/1001/docker.sock",
		"--no-auto-providers", "--name", "tests", "--from", "img", "--", "pytest",
	}
	if !slices.Equal(argv, want) {
		t.Errorf("exec %q\nwant %q", argv, want)
	}

	// Without a policy file, OpenShell's built-in default and the rule.
	if fs, ok := doc.Fixed["filesystem_policy"].(map[string]any); !ok || fs["include_workdir"] != true {
		t.Errorf("filesystem_policy = %v", doc.Fixed["filesystem_policy"])
	}
	if len(doc.Network) != 1 {
		t.Errorf("network rules = %v", doc.Network)
	}
	rule := doc.Network[dockerRuleKey]
	eps, _ := rule["endpoints"].([]any)
	if len(eps) != 1 {
		t.Fatalf("rule %s = %v", dockerRuleKey, rule)
	}
	ep := eps[0].(map[string]any)
	ports, _ := ep["ports"].([]any)
	if ep["host"] != "host.openshell.internal" || ep["protocol"] != "tcp" || len(ports) != 129 ||
		ports[0] != int64(guest.DockerPort) || ports[1] != int64(guest.FirstPublishedPort) || ports[128] != int64(guest.LastPublishedPort) {
		t.Errorf("endpoint = %v", ep)
	}
	bins, _ := rule["binaries"].([]any)
	if len(bins) != len(dockerBinaries) {
		t.Errorf("binaries = %v", bins)
	}
}

func TestSandboxCreateDockerAddsToThePolicyFile(t *testing.T) {
	a := testApp(t)
	v := testVM(t, a, "dev")
	dir := t.TempDir()
	writeTestFile(t, filepath.Join(dir, "policies", "default.yaml"), "version: 1\n# the VM's\nnetwork_policies:\n  pypi:\n    endpoints: [{host: pypi.org, port: 443}]\n", 0o600)
	v.OpenShellConfigs = []string{dir}
	if err := a.vms.Save(v); err != nil {
		t.Fatal(err)
	}
	own := filepath.Join(t.TempDir(), "webapp.yaml")
	writeTestFile(t, own, "version: 1\n# mine\nlandlock:\n  compatibility: hard_requirement\n", 0o600)
	// A file with a brig_docker rule of its own keeps it.
	mine := filepath.Join(t.TempDir(), "dotnet.yaml")
	writeTestFile(t, mine, "version: 1\nnetwork_policies:\n  brig_docker:\n    endpoints: [{host: host.openshell.internal, port: 2375, protocol: tcp}]\n"+
		"    binaries: [{path: /usr/bin/dotnet}]\n", 0o600)

	for _, tc := range []struct {
		name  string
		pass  []string
		rules []string
		text  string
	}{
		{"default policy", []string{"--name", "tests"}, []string{dockerRuleKey, "pypi"}, "# the VM's\n"},
		{"--policy", []string{"--policy", own, "--name", "tests"}, []string{dockerRuleKey}, "# mine\n"},
		{"--policy=", []string{"--name", "tests", "--policy=" + own}, []string{dockerRuleKey}, "# mine\n"},
		{"own rule", []string{"--policy", mine}, []string{dockerRuleKey}, "/usr/bin/dotnet"},
	} {
		got := captureExec(t)
		if out, err := run(t, append([]string{"sandbox", "create", "dev", "--docker", "--"}, tc.pass...)...); err != nil {
			t.Fatalf("%s: brig sandbox create: %v: %s", tc.name, err, out)
		}
		doc, text, argv := execPolicy(t, *got)
		var keys []string
		for k := range doc.Network {
			keys = append(keys, k)
		}
		slices.Sort(keys)
		if !slices.Equal(keys, tc.rules) || !strings.Contains(text, tc.text) {
			t.Errorf("%s: rules %q in\n%s", tc.name, keys, text)
		}
		if n := strings.Count(strings.Join(argv, " "), "--policy"); n != 1 {
			t.Errorf("%s: %d --policy in %q", tc.name, n, argv)
		}
	}
}

func TestSandboxCreateDockerWithATemplate(t *testing.T) {
	a := testApp(t)
	testVM(t, a, "dev")
	got := captureExec(t)
	out, err := run(t, "sandbox", "create", "dev", "--docker", "--", "--name", "tests", "--template", "pi")
	if err != nil {
		t.Fatalf("brig sandbox create: %v: %s", err, out)
	}
	if slices.Contains(*got, "--env") {
		t.Errorf("exec %q has --env, which openshell refuses with --template", *got)
	}
	if !strings.Contains(out, "export DOCKER_HOST=tcp://host.openshell.internal:2375\n") {
		t.Errorf("output does not say how to set DOCKER_HOST:\n%s", out)
	}
	execPolicy(t, *got)
}

func TestSandboxCreateDockerRejects(t *testing.T) {
	a := testApp(t)
	testVM(t, a, "dev")
	captureExec(t)
	for _, tc := range []struct {
		args    []string
		wantErr string
	}{
		{[]string{"dev", "--docker", "--", "--name", "x", "--policy"}, "--policy needs a file"},
		{[]string{"dev", "--docker", "--", "--policy", "/no/such/policy.yaml"}, "no such file"},
	} {
		if _, err := run(t, append([]string{"sandbox", "create"}, tc.args...)...); err == nil || !strings.Contains(err.Error(), tc.wantErr) {
			t.Errorf("sandbox create %q: %v, want an error containing %q", tc.args, err, tc.wantErr)
		}
	}
}

func TestCutPolicyArg(t *testing.T) {
	for _, tc := range []struct {
		args, rest []string
		file       string
	}{
		{[]string{"--name", "x"}, []string{"--name", "x"}, ""},
		{[]string{"--policy", "p.yaml", "--name", "x"}, []string{"--name", "x"}, "p.yaml"},
		{[]string{"--policy=p.yaml"}, nil, "p.yaml"},
		// The sandbox's command may take the flag for itself.
		{[]string{"--name", "x", "--", "opa", "--policy", "q"}, []string{"--name", "x", "--", "opa", "--policy", "q"}, ""},
	} {
		file, rest, err := cutPolicyArg(tc.args)
		if err != nil || file != tc.file || !slices.Equal(rest, tc.rest) {
			t.Errorf("cutPolicyArg(%q) = %q, %q, %v; want %q, %q", tc.args, file, rest, err, tc.file, tc.rest)
		}
	}
}

func TestBuiltinPolicyIsOpenShellsDefault(t *testing.T) {
	doc, err := policy.Parse(builtinPolicy)
	if err != nil {
		t.Fatal(err)
	}
	// The default policy that brig agent init writes spells out the same.
	want, err := policy.Parse([]byte("version: 1\nfilesystem_policy:\n  include_workdir: true\n" +
		"  read_only: [/bin, /usr, /lib, /etc, /proc, /dev/urandom, /var/log]\n  read_write: [/tmp, /dev/null]\n" +
		"landlock:\n  compatibility: best_effort\n"))
	if err != nil {
		t.Fatal(err)
	}
	if d := policy.Diff(doc, want); d.Drift() || len(doc.Network) > 0 {
		t.Errorf("built-in policy differs: %+v", d)
	}
}

func TestWithoutDockerRule(t *testing.T) {
	sandbox := &policy.Doc{Network: map[string]policy.Rule{dockerRuleKey: {"name": "x"}, "pypi": {}}}
	if got := withoutDockerRule(sandbox, &policy.Doc{}); len(got.Network) != 1 || len(sandbox.Network) != 2 {
		t.Errorf("without the rule in the file: %v, sandbox %v", got.Network, sandbox.Network)
	}
	if got := withoutDockerRule(sandbox, &policy.Doc{Network: map[string]policy.Rule{dockerRuleKey: {}}}); len(got.Network) != 2 {
		t.Errorf("with the rule in the file: %v", got.Network)
	}
}

func TestPolicyDiffIgnoresTheDockerRule(t *testing.T) {
	// webapp.yaml plus the rule of --docker.
	policyFixture(t, `{"filesystem_policy": {"include_workdir": true, "read_only": ["/usr"]},
  "landlock": {"compatibility": "best_effort"},
  "network_policies": {"npm": {"endpoints": [{"host": "registry.npmjs.org", "port": 443, "protocol": "rest", "enforcement": "enforce"}]},
                       "brig_docker": {"endpoints": [{"host": "host.openshell.internal", "ports": [2375, 61000], "protocol": "tcp"}]}}}`)
	out, _, _, err := runSplit(t, "policy", "diff", "dev", "webapp", "-o", "json")
	if err != nil {
		t.Fatalf("exit = %v\n%s", err, out)
	}
	if rep := decodeReport(t, out); rep.Source == nil || filepath.Base(rep.Source.Path) != "webapp.yaml" || rep.Drift {
		t.Errorf("report:\n%s", out)
	}
}

func TestRecreateWithThePolicyFileKeepsDocker(t *testing.T) {
	g := newFakeSnapGW()
	g.add("webapp", "Stopped", "localhost/claude-agent:1", "work")
	g.base = json.RawMessage(`{"networkPolicies":{"npm":{"name":"npm"},"brig_docker":{"name":"brig_docker","endpoints":[{"host":"host.openshell.internal","port":2375}]}}}`)
	s, _, _ := recreateEngine(t, g)
	orig := filePolicy
	filePolicy = func(context.Context, *app, *vm.VM, string, string, string) (string, json.RawMessage, error) {
		return "policies/webapp.yaml", json.RawMessage(`{"version":1}`), nil
	}
	t.Cleanup(func() { filePolicy = orig })
	ctx := context.Background()
	p, err := s.planRecreate(ctx, "default", "webapp", recreateTarget{image: "localhost/claude-agent:2"}, planOpts{policy: "file"})
	if err != nil {
		t.Fatal(err)
	}
	if p.Policy != "policy file policies/webapp.yaml, with rule brig_docker of the sandbox" {
		t.Errorf("plan %+v", p)
	}
	if _, err := s.recreate(ctx, p); err != nil {
		t.Fatal(err)
	}
	want := `{"version":1,"networkPolicies":{"brig_docker":{"name":"brig_docker","endpoints":[{"host":"host.openshell.internal","port":2375}]}}}`
	var got, w any
	_ = json.Unmarshal(g.created[0].Policy, &got)
	_ = json.Unmarshal([]byte(want), &w)
	if !reflect.DeepEqual(got, w) {
		t.Errorf("policy %s\nwant %s", g.created[0].Policy, want)
	}
}
