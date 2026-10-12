// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

package policy

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
)

func parse(t *testing.T, s string) *Doc {
	t.Helper()
	d, err := Parse([]byte(s))
	if err != nil {
		t.Fatal(err)
	}
	return d
}

const base = `
version: 1
filesystem_policy:
  include_workdir: true
  read_only: [/usr, /etc]
  read_write: [/tmp]
landlock:
  compatibility: best_effort
`

const npm = `
network_policies:
  npm:
    name: npm
    endpoints:
      - host: registry.npmjs.org
        port: 443
        protocol: rest
        enforcement: enforce
        rules:
          - allow: { method: GET, path: "/**" }
    binaries:
      - path: /usr/bin/node
`

func TestParseNormalises(t *testing.T) {
	// A file as a person writes it, and the same policy as a gateway might
	// return it: JSON, zero values spelled out, ports as a list, other
	// order, no version.
	file := parse(t, base+npm)
	gateway := parse(t, `{
	  "filesystem_policy": {"include_workdir": true, "read_only": ["/etc", "/usr"], "read_write": ["/tmp"], "extra": []},
	  "landlock": {"compatibility": "best_effort"},
	  "process": {"run_as_user": "", "run_as_group": ""},
	  "network_policies": {"npm": {
	    "name": "npm",
	    "binaries": [{"path": "/usr/bin/node", "harness": false}],
	    "endpoints": [{"host": "registry.npmjs.org", "ports": [443], "port": 0, "protocol": "rest",
	      "enforcement": "enforce", "tls": "", "rules": [{"allow": {"method": "GET", "path": "/**", "query": {}}}]}]
	  }}
	}`)
	if r := Diff(gateway, file); r.Drift() {
		t.Errorf("equal policies differ: %+v", r)
	}
}

func TestEndpointPorts(t *testing.T) {
	ep := func(s string) Rule {
		d := parse(t, "network_policies:\n  r:\n    endpoints:\n      - host: h\n"+s)
		return d.Network["r"]
	}
	same := func(a, b string) {
		t.Helper()
		x, _ := json.Marshal(ep(a))
		y, _ := json.Marshal(ep(b))
		if string(x) != string(y) {
			t.Errorf("%q and %q differ:\n%s\n%s", a, b, x, y)
		}
	}
	same("", "        port: 443\n")
	same("        ports: [8080]\n", "        port: 8080\n")
	same("        port: 8080\n        ports: [8080]\n", "        port: 8080\n")
	if x, _ := json.Marshal(ep("        ports: [80, 443]\n")); !strings.Contains(string(x), `"ports":[80,443]`) {
		t.Errorf("several ports = %s", x)
	}
	if x, _ := json.Marshal(ep("        ports: [443, 80]\n")); !strings.Contains(string(x), `"ports":[80,443]`) {
		t.Errorf("ports are not sorted: %s", x)
	}
	a, b := ep("        port: 80\n"), ep("        port: 443\n")
	if equal(a, b) {
		t.Error("different ports compare equal")
	}
}

func TestDiff(t *testing.T) {
	sandbox := parse(t, base+npm+`
  pypi:
    endpoints:
      - host: pypi.org
        enforcement: audit
  gh:
    endpoints:
      - host: github.com
        protocol: rest
`)
	file := parse(t, `version: 1
filesystem_policy:
  include_workdir: true
  read_only: [/usr]
  read_write: [/tmp]
landlock:
  compatibility: best_effort
network_policies:
  gh:
    endpoints:
      - host: github.com
        protocol: rest
        enforcement: enforce
  crates:
    endpoints:
      - host: crates.io
`)
	got := Diff(sandbox, file)
	type row struct {
		key   string
		state State
		audit bool
	}
	var rows []row
	for _, r := range got.Rules {
		rows = append(rows, row{r.Key, r.State, r.Audit})
	}
	want := []row{{"crates", NotApplied, false}, {"gh", Changed, false}, {"npm", Unsaved, false}, {"pypi", Unsaved, true}}
	if !slices.Equal(rows, want) {
		t.Errorf("rules = %+v, want %+v", rows, want)
	}
	if !slices.Equal(got.Fixed, []string{"filesystem_policy"}) {
		t.Errorf("fixed = %v", got.Fixed)
	}
	if !got.Drift() {
		t.Error("Drift() = false")
	}
	if r := Diff(sandbox, sandbox); r.Drift() || r.Rules == nil || r.Fixed == nil {
		t.Errorf("a policy differs from itself, or leaves nil slices: %+v", r)
	}
	if d := Diff(sandbox, &Doc{Network: map[string]Rule{}, Fixed: map[string]any{}}); len(d.Fixed) != 2 {
		t.Errorf("an empty file differs in %v, want filesystem_policy and landlock", d.Fixed)
	}
}

func TestParseRejects(t *testing.T) {
	for _, in := range []string{"network_policies: [a]", "network_policies:\n  r: 3", "{", "- a"} {
		if _, err := Parse([]byte(in)); err == nil {
			t.Errorf("Parse(%q) succeeded", in)
		}
	}
	for _, in := range []string{"", "version: 1", "network_policies:", "network_policies: {}"} {
		d, err := Parse([]byte(in))
		if err != nil || len(d.Network) != 0 || len(d.Fixed) != 0 {
			t.Errorf("Parse(%q) = %+v, %v; want an empty policy", in, d, err)
		}
	}
}

func TestInfer(t *testing.T) {
	sandbox := parse(t, base+npm)
	cand := func(name, path, doc string) Candidate {
		return Candidate{Name: name, Path: path, Doc: parse(t, doc)}
	}
	def := cand("default", "/a/default.yaml", base)
	webapp := cand("webapp", "/a/webapp.yaml", base+npm)
	other := cand("other", "/a/other.yaml", base+npm)
	foreign := cand("foreign", "/a/foreign.yaml", "filesystem_policy:\n  read_only: [/]\n"+npm)

	for _, tc := range []struct {
		name       string
		sandbox    string
		cands      []Candidate
		want       string
		wantTiedAs []string
	}{
		{"fewest differences", "webapp", []Candidate{def, webapp}, "/a/webapp.yaml", nil},
		{"the default plus a rule added later", "webapp", []Candidate{def, foreign}, "/a/default.yaml", nil},
		{"named after the sandbox", "webapp", []Candidate{other, webapp}, "/a/webapp.yaml", nil},
		{"default beats others", "x", []Candidate{other, {Name: "default", Path: "/b/default.yaml", Doc: webapp.Doc}}, "/b/default.yaml", nil},
		{"ambiguous", "x", []Candidate{other, cand("third", "/a/third.yaml", base+npm)}, "", []string{"/a/other.yaml", "/a/third.yaml"}},
		{"same name twice", "webapp", []Candidate{webapp, {Name: "webapp", Path: "/b/webapp.yaml", Doc: webapp.Doc}}, "", []string{"/a/webapp.yaml", "/b/webapp.yaml"}},
		{"none qualifies", "webapp", []Candidate{foreign}, "", nil},
		{"no candidates", "webapp", nil, "", nil},
	} {
		best, tied := Infer(tc.sandbox, sandbox, tc.cands)
		got := ""
		if best != nil {
			got = best.Path
		}
		var tiedPaths []string
		for _, c := range tied {
			tiedPaths = append(tiedPaths, c.Path)
		}
		if got != tc.want || !slices.Equal(tiedPaths, tc.wantTiedAs) {
			t.Errorf("%s: Infer = %q, tied %q; want %q, tied %q", tc.name, got, tiedPaths, tc.want, tc.wantTiedAs)
		}
	}
}
