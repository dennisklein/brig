// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

package policy

import (
	"strings"
	"testing"
)

// gateway is a base policy as openshell might print it: JSON, a port list,
// zero values spelled out, keys in no particular order.
const gateway = `{"network_policies": {
  "npm": {"binaries": [{"path": "/usr/bin/node"}], "endpoints": [{"protocol": "rest", "host": "registry.npmjs.org", "ports": [443], "enforcement": "enforce", "tls": ""}]},
  "push": {"name": "github-push", "endpoints": [{"host": "github.com", "port": 443, "protocol": "rest", "enforcement": "enforce",
    "rules": [{"allow": {"method": "POST", "path": "/alice/webapp.git/git-receive-pack"}}]}], "binaries": [{"path": "/usr/bin/git"}]}
}}`

func gatewayRules(t *testing.T, keys ...string) []NamedRule {
	t.Helper()
	raw, err := RawRules([]byte(gateway))
	if err != nil {
		t.Fatal(err)
	}
	var out []NamedRule
	for _, k := range keys {
		out = append(out, NamedRule{Key: k, Rule: raw[k]})
	}
	return out
}

const npmRule = `  npm:
    endpoints:
      - host: registry.npmjs.org
        port: 443
        protocol: rest
        enforcement: enforce
    binaries:
      - path: /usr/bin/node
`

func TestSave(t *testing.T) {
	for _, tc := range []struct {
		name, file string
		keys       []string
		want       string
	}{
		{
			name: "new section, comments and blank lines kept",
			file: "# default policy\nversion: 1\n\nfilesystem_policy:\n  include_workdir: true   # /sandbox\n  read_only: [/usr, \"/etc\"]\n",
			keys: []string{"npm"},
			want: "# default policy\nversion: 1\n\nfilesystem_policy:\n  include_workdir: true   # /sandbox\n  read_only: [/usr, \"/etc\"]\nnetwork_policies:\n" + npmRule,
		},
		{
			name: "no newline at the end",
			file: "version: 1",
			keys: []string{"npm"},
			want: "version: 1\nnetwork_policies:\n" + npmRule,
		},
		{
			name: "empty file",
			file: "",
			keys: []string{"npm"},
			want: "network_policies:\n" + npmRule,
		},
		{
			name: "empty flow section",
			file: "version: 1\nnetwork_policies: {}  # none yet\nlandlock:\n  compatibility: best_effort\n",
			keys: []string{"npm"},
			want: "version: 1\nnetwork_policies: # none yet\n" + npmRule + "landlock:\n  compatibility: best_effort\n",
		},
		{
			name: "null section",
			file: "network_policies:\nversion: 1\n",
			keys: []string{"npm"},
			want: "network_policies:\n" + npmRule + "version: 1\n",
		},
		{
			name: "appended after the last rule, before the next section's comment",
			file: "network_policies:\n    # the registry\n    old:\n        endpoints:\n            - host: example.org\n        # trailing, inside old\n\n# fixed sections\nlandlock:\n  compatibility: best_effort\n",
			keys: []string{"push"},
			want: "network_policies:\n    # the registry\n    old:\n        endpoints:\n            - host: example.org\n        # trailing, inside old\n" +
				"    push:\n      name: github-push\n      endpoints:\n        - host: github.com\n          port: 443\n          protocol: rest\n          enforcement: enforce\n" +
				"          rules:\n            - allow: {method: POST, path: /alice/webapp.git/git-receive-pack}\n      binaries:\n        - path: /usr/bin/git\n" +
				"\n# fixed sections\nlandlock:\n  compatibility: best_effort\n",
		},
		{
			name: "replaced in place, neighbours kept",
			file: "network_policies:\n  a:\n    endpoints: [{host: a.example}]\n\n  # npm, by hand\n  npm:\n    endpoints:\n      - host: registry.npmjs.org\n        enforcement: audit\n\n  z:\n    endpoints: [{host: z.example}]\n",
			keys: []string{"npm"},
			want: "network_policies:\n  a:\n    endpoints: [{host: a.example}]\n\n  # npm, by hand\n" + npmRule + "\n  z:\n    endpoints: [{host: z.example}]\n",
		},
		{
			name: "one replaced, one added",
			file: "network_policies:\n  npm:\n    endpoints: [{host: registry.npmjs.org}]\n",
			keys: []string{"npm", "push"},
			want: "network_policies:\n" + npmRule +
				"  push:\n    name: github-push\n    endpoints:\n      - host: github.com\n        port: 443\n        protocol: rest\n        enforcement: enforce\n" +
				"        rules:\n          - allow: {method: POST, path: /alice/webapp.git/git-receive-pack}\n    binaries:\n      - path: /usr/bin/git\n",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Save([]byte(tc.file), gatewayRules(t, tc.keys...))
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != tc.want {
				t.Errorf("Save() =\n%s\nwant\n%s", got, tc.want)
			}
		})
	}
}

func TestSaveRefuses(t *testing.T) {
	for _, tc := range []struct{ name, file, want string }{
		{"flow section", "network_policies: {a: {endpoints: [{host: a.example}]}}\n", "not written as a block mapping"},
		{"flow document", "{version: 1}\n", "not a block mapping"},
		{"list", "- a\n", "parse policy"},
		{"bad YAML", "version: [\n", "parse policy"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := Save([]byte(tc.file), gatewayRules(t, "npm")); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("Save() error = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestRawRules(t *testing.T) {
	raw, err := RawRules([]byte(gateway))
	if err != nil {
		t.Fatal(err)
	}
	ep := raw["npm"]["endpoints"].([]any)[0].(map[string]any)
	if _, ok := ep["ports"]; ok || ep["port"] != int64(443) {
		t.Errorf("single port not spelled as port: %v", ep)
	}
	if _, ok := ep["tls"]; ok {
		t.Errorf("empty tls kept: %v", ep)
	}
	_, err = RawRules([]byte(`{"network_policies": {"x": {"endpoints": [{"host": "a", "enforcement": "NETWORK_ENFORCEMENT_MODE_ENFORCE"}]}}}`))
	if err == nil || !strings.Contains(err.Error(), "does not accept") {
		t.Errorf("enum name accepted: %v", err)
	}
	if raw, err := RawRules([]byte("version: 1\n")); err != nil || len(raw) != 0 {
		t.Errorf("RawRules() = %v, %v", raw, err)
	}
}
