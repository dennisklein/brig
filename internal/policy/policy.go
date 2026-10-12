// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

// Package policy compares the policy of an OpenShell sandbox with the policy
// files it may have come from.
//
// A sandbox's policy is copied from a file when the sandbox is created, and
// rules added later with openshell policy update live in the sandbox only.
// The comparison is by network rule, keyed by the name under network_policies,
// on the parsed documents, so that comments, key order and defaults that one
// side writes out and the other leaves implicit are not differences.
package policy

import (
	"cmp"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"

	"go.yaml.in/yaml/v3"
)

const (
	networkKey = "network_policies"
	// defaultPort is the port of an endpoint that names none.
	defaultPort = 443
)

// Rule is one entry of network_policies, normalised: a tree of maps, slices
// and scalars in which an unset value and a zero value are the same, numbers
// are int64 where they are whole, and slices are sorted.
type Rule = map[string]any

// Doc is a parsed policy.
type Doc struct {
	// Network holds the network rules by key.
	Network map[string]Rule
	// Fixed holds the other sections, such as filesystem_policy, landlock
	// and process, by name. Only a restart of the sandbox applies them. The
	// version is left out: it numbers the schema, not the policy.
	Fixed map[string]any
}

// Parse reads a policy in YAML or JSON.
func Parse(data []byte) (*Doc, error) {
	var top map[string]any
	if err := yaml.Unmarshal(data, &top); err != nil {
		return nil, fmt.Errorf("parse policy: %w", err)
	}
	d := &Doc{Network: map[string]Rule{}, Fixed: map[string]any{}}
	for k, v := range top {
		switch k {
		case "version":
		case networkKey:
			c := clean(v)
			if c == nil {
				continue
			}
			rules, ok := c.(map[string]any)
			if !ok {
				return nil, fmt.Errorf("parse policy: %s is not a map of rules", networkKey)
			}
			for key, r := range rules {
				rule, ok := r.(map[string]any)
				if !ok {
					return nil, fmt.Errorf("parse policy: rule %s is not a map", key)
				}
				d.Network[key] = canonical(fixEndpoints(rule)).(map[string]any)
			}
		default:
			if c := clean(v); c != nil {
				d.Fixed[k] = canonical(c)
			}
		}
	}
	return d, nil
}

// clean converts what the YAML decoder returns to maps with string keys and
// int64 numbers, and drops values that are unset or zero.
func clean(v any) any {
	switch v := v.(type) {
	case map[string]any:
		out := map[string]any{}
		for k, e := range v {
			if c := clean(e); c != nil {
				out[k] = c
			}
		}
		if len(out) == 0 {
			return nil
		}
		return out
	case map[any]any:
		m := make(map[string]any, len(v))
		for k, e := range v {
			m[fmt.Sprint(k)] = e
		}
		return clean(m)
	case []any:
		var out []any
		for _, e := range v {
			if c := clean(e); c != nil {
				out = append(out, c)
			}
		}
		if len(out) == 0 {
			return nil
		}
		return out
	case string:
		if v == "" {
			return nil
		}
	case bool:
		if !v {
			return nil
		}
	case int:
		return clean(int64(v))
	case uint64:
		return clean(int64(v)) //nolint:gosec // G115: ports and counts are small
	case int64:
		if v == 0 {
			return nil
		}
	case float64:
		if v == 0 {
			return nil
		}
		if w := int64(v); float64(w) == v {
			return w
		}
	}
	return v
}

// fixEndpoints writes the endpoints of rule the way every source spells
// them: a single port as port, several as ports, and 443 where none is set.
func fixEndpoints(rule map[string]any) map[string]any {
	eps, _ := rule["endpoints"].([]any)
	for _, e := range eps {
		ep, ok := e.(map[string]any)
		if !ok {
			continue
		}
		ports, _ := ep["ports"].([]any)
		port, hasPort := ep["port"]
		switch {
		case len(ports) == 1 && (!hasPort || port == ports[0]):
			ep["port"] = ports[0]
			delete(ep, "ports")
		case len(ports) == 0 && !hasPort:
			ep["port"] = int64(defaultPort)
		}
	}
	return rule
}

// canonical sorts every slice in v by its JSON encoding. The order of
// endpoints, binaries, rules and paths means nothing to OpenShell, and its
// own output need not keep the order of the file.
func canonical(v any) any {
	switch v := v.(type) {
	case map[string]any:
		for k, e := range v {
			v[k] = canonical(e)
		}
	case []any:
		for i, e := range v {
			v[i] = canonical(e)
		}
		slices.SortStableFunc(v, order)
	}
	return v
}

// order sorts whole numbers by value and everything else by JSON encoding.
func order(a, b any) int {
	x, xok := a.(int64)
	y, yok := b.(int64)
	if xok && yok {
		return cmp.Compare(x, y)
	}
	p, _ := json.Marshal(a)
	q, _ := json.Marshal(b)
	return strings.Compare(string(p), string(q))
}

// State says how a network rule differs between a sandbox and a file.
type State string

// The states of a rule that is not the same on both sides.
const (
	// Unsaved: the sandbox has the rule, the file does not.
	Unsaved State = "unsaved"
	// NotApplied: the file has the rule, the sandbox does not.
	NotApplied State = "not_applied"
	// Changed: both have a rule of that key, with different content.
	Changed State = "changed"
)

// RuleDiff is a network rule that differs.
type RuleDiff struct {
	Key   string `json:"key"`
	State State  `json:"state"`
	// Sandbox and File are the rule on either side, nil where there is none.
	Sandbox Rule `json:"sandbox,omitempty"`
	File    Rule `json:"file,omitempty"`
	// Audit is set when an endpoint of the sandbox's rule only logs, as
	// rules do that were added to learn what an agent needs.
	Audit bool `json:"audit,omitempty"`
}

// Result is the difference between a sandbox and a file.
type Result struct {
	// Rules are the differing network rules, sorted by key.
	Rules []RuleDiff `json:"rules"`
	// Fixed names the sections other than the network rules that differ.
	// A sandbox takes a new value only when it is recreated, so these are
	// reported and never offered for saving.
	Fixed []string `json:"fixed"`
}

// Drift reports whether the sandbox and the file differ at all.
func (r Result) Drift() bool { return len(r.Rules) > 0 || len(r.Fixed) > 0 }

// Diff compares the policy of a sandbox with a file's.
func Diff(sandbox, file *Doc) Result {
	r := Result{Rules: []RuleDiff{}, Fixed: []string{}}
	keys := slices.Sorted(maps.Keys(sandbox.Network))
	for k := range file.Network {
		if _, ok := sandbox.Network[k]; !ok {
			keys = append(keys, k)
		}
	}
	slices.Sort(keys)
	for _, k := range keys {
		s, inSandbox := sandbox.Network[k]
		f, inFile := file.Network[k]
		d := RuleDiff{Key: k, Sandbox: s, File: f, Audit: hasAudit(s)}
		switch {
		case inSandbox && !inFile:
			d.State = Unsaved
		case !inSandbox:
			d.State = NotApplied
		case !equal(s, f):
			d.State = Changed
		default:
			continue
		}
		r.Rules = append(r.Rules, d)
	}
	sections := slices.Sorted(maps.Keys(sandbox.Fixed))
	for k := range file.Fixed {
		if _, ok := sandbox.Fixed[k]; !ok {
			sections = append(sections, k)
		}
	}
	slices.Sort(sections)
	for _, k := range sections {
		if !equal(sandbox.Fixed[k], file.Fixed[k]) {
			r.Fixed = append(r.Fixed, k)
		}
	}
	return r
}

func hasAudit(rule Rule) bool {
	eps, _ := rule["endpoints"].([]any)
	for _, e := range eps {
		if ep, ok := e.(map[string]any); ok && ep["enforcement"] == "audit" {
			return true
		}
	}
	return false
}

// equal compares two normalised values.
func equal(a, b any) bool {
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return string(x) == string(y)
}

// Candidate is a policy file that a sandbox may have come from.
type Candidate struct {
	// Name is the file name without its extension.
	Name string
	Path string
	Doc  *Doc
}

// Infer picks the candidate that the sandbox most likely came from: the one
// whose sections other than the network rules are the sandbox's, and whose
// network rules differ in the fewest places. Among equals, the file named
// after the sandbox wins, then default.yaml. It returns nil if no candidate
// qualifies, and with it the candidates that tie when more than one does,
// which is a choice for the user.
func Infer(sandboxName string, sandbox *Doc, candidates []Candidate) (best *Candidate, tied []Candidate) {
	score := func(c Candidate) int {
		d := Diff(sandbox, c.Doc)
		if len(d.Fixed) > 0 {
			return -1
		}
		return len(d.Rules)
	}
	rank := func(c Candidate) int {
		switch c.Name {
		case sandboxName:
			return 0
		case "default":
			return 1
		}
		return 2
	}
	least, bestRank := -1, 0
	for _, c := range candidates {
		s := score(c)
		switch {
		case s < 0:
		case least < 0 || s < least || (s == least && rank(c) < bestRank):
			least, bestRank, tied = s, rank(c), []Candidate{c}
		case s == least && rank(c) == bestRank:
			tied = append(tied, c)
		}
	}
	switch len(tied) {
	case 0:
		return nil, nil
	case 1:
		return &tied[0], nil
	}
	return nil, tied
}
