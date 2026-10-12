// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

package policy

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strings"

	"go.yaml.in/yaml/v3"
)

// NamedRule is a network rule as a sandbox has it, under its key.
type NamedRule struct {
	Key  string
	Rule map[string]any
}

// enumName matches how a protobuf enum value is spelled, such as
// NETWORK_ENFORCEMENT_MODE_ENFORCE, as opposed to the enforce of a policy file.
var enumName = regexp.MustCompile(`^[A-Z][A-Z0-9_]*$`)

// RawRules returns the network rules of a policy in YAML or JSON as they
// are written there, keeping the order of lists, without unset or zero
// values, and with a single port in ports spelled as port. It refuses a rule
// whose tls, enforcement or access is not spelled as a policy file spells
// it, since writing that into a file would break the file.
func RawRules(data []byte) (map[string]map[string]any, error) {
	var top map[string]any
	if err := yaml.Unmarshal(data, &top); err != nil {
		return nil, fmt.Errorf("parse policy: %w", err)
	}
	rules := map[string]map[string]any{}
	c := clean(top[networkKey])
	if c == nil {
		return rules, nil
	}
	m, ok := c.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("parse policy: %s is not a map of rules", networkKey)
	}
	for key, r := range m {
		rule, ok := r.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("parse policy: rule %s is not a map", key)
		}
		eps, _ := rule["endpoints"].([]any)
		for _, e := range eps {
			ep, ok := e.(map[string]any)
			if !ok {
				continue
			}
			for _, k := range []string{"tls", "enforcement", "access"} {
				if s, ok := ep[k].(string); ok && enumName.MatchString(s) {
					return nil, fmt.Errorf("rule %s: openshell gives %s as %q, which a policy file does not accept", key, k, s)
				}
			}
			if ports, _ := ep["ports"].([]any); len(ports) == 1 {
				if port, ok := ep["port"]; !ok || port == ports[0] {
					ep["port"] = ports[0]
					delete(ep, "ports")
				}
			}
		}
		rules[key] = rule
	}
	return rules, nil
}

// Save returns the policy file with each rule put under network_policies,
// replacing a rule of the same key. It edits the text: everything outside
// the rules it writes stays as it was, comments, blank lines and quoting
// included. It checks the result by parsing it again, and fails rather than
// return a file that does not hold exactly the old rules and the new.
func Save(file []byte, rules []NamedRule) ([]byte, error) {
	before, err := Parse(file)
	if err != nil {
		return nil, err
	}
	out, err := splice(file, rules)
	if err != nil {
		return nil, err
	}
	after, err := Parse(out)
	if err != nil {
		return nil, fmt.Errorf("the edited file does not parse: %w", err)
	}
	want := &Doc{Network: maps.Clone(before.Network), Fixed: before.Fixed}
	for _, r := range rules {
		n, err := normalise(r)
		if err != nil {
			return nil, err
		}
		want.Network[r.Key] = n
	}
	if d := Diff(after, want); d.Drift() {
		return nil, errors.New("editing the file would change more than the rules to save")
	}
	return out, nil
}

// normalise returns rule as Parse would read it from a file.
func normalise(r NamedRule) (Rule, error) {
	data, err := json.Marshal(map[string]any{networkKey: map[string]any{r.Key: r.Rule}})
	if err != nil {
		return nil, err
	}
	d, err := Parse(data)
	if err != nil {
		return nil, err
	}
	return d.Network[r.Key], nil
}

// edit replaces lines [start, end) with text.
type edit struct {
	start, end int
	text       string
}

func splice(file []byte, rules []NamedRule) ([]byte, error) {
	lines := strings.SplitAfter(string(file), "\n")
	if lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	if n := len(lines); n > 0 && !strings.HasSuffix(lines[n-1], "\n") {
		lines[n-1] += "\n"
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(file, &doc); err != nil {
		return nil, fmt.Errorf("parse policy: %w", err)
	}
	var top *yaml.Node
	if doc.Kind == yaml.DocumentNode && len(doc.Content) == 1 {
		top = doc.Content[0]
	}
	if top != nil && (top.Kind != yaml.MappingNode || top.Style&yaml.FlowStyle != 0) {
		return nil, errors.New("the policy file is not a block mapping")
	}

	// Find network_policies.
	var key, val *yaml.Node
	next := 0 // the line after network_policies and its rules
	if top != nil {
		for i := 0; i+1 < len(top.Content); i += 2 {
			if top.Content[i].Value == networkKey {
				key, val = top.Content[i], top.Content[i+1]
				next = len(lines)
				if i+2 < len(top.Content) {
					next = top.Content[i+2].Line - 1
				}
				next = backOver(lines, next, key.Column-1)
				break
			}
		}
	}

	var edits []edit
	switch {
	case key == nil:
		// Add the section at the end.
		text, err := render(rules, 2)
		if err != nil {
			return nil, err
		}
		edits = append(edits, edit{len(lines), len(lines), networkKey + ":\n" + text})
	case isNull(val) || (val.Kind == yaml.MappingNode && len(val.Content) == 0 && val.Line == key.Line):
		// network_policies: with nothing, null or {}: spell it as a block.
		line := strings.Repeat(" ", key.Column-1) + networkKey + ":"
		if c := val.LineComment; c != "" {
			line += " " + c
		}
		text, err := render(rules, key.Column-1+2)
		if err != nil {
			return nil, err
		}
		edits = append(edits, edit{key.Line - 1, key.Line, line + "\n" + text})
	case val.Kind == yaml.MappingNode && val.Style&yaml.FlowStyle == 0:
		indent := val.Content[0].Column - 1
		ends := map[string][2]int{}
		var last int
		for i := 0; i+1 < len(val.Content); i += 2 {
			k := val.Content[i]
			end := next
			if i+2 < len(val.Content) {
				end = backOver(lines, val.Content[i+2].Line-1, indent)
			}
			ends[k.Value] = [2]int{k.Line - 1, end}
			last = end
		}
		var added []NamedRule
		for _, r := range rules {
			span, ok := ends[r.Key]
			if !ok {
				added = append(added, r)
				continue
			}
			text, err := render([]NamedRule{r}, indent)
			if err != nil {
				return nil, err
			}
			edits = append(edits, edit{span[0], span[1], text})
		}
		if len(added) > 0 {
			text, err := render(added, indent)
			if err != nil {
				return nil, err
			}
			edits = append(edits, edit{last, last, text})
		}
	default:
		return nil, fmt.Errorf("%s is not written as a block mapping; add the rules by hand", networkKey)
	}

	// Apply from the bottom up, so that earlier line numbers stay valid.
	slices.SortStableFunc(edits, func(a, b edit) int { return b.start - a.start })
	for _, e := range edits {
		lines = slices.Replace(lines, e.start, e.end, e.text)
	}
	return []byte(strings.Join(lines, "")), nil
}

// backOver returns the line at which the item before line ends: line, less
// the blank lines and the comments indented no deeper than indent above it,
// which belong to the item at line.
func backOver(lines []string, line, indent int) int {
	for line > 0 {
		l := lines[line-1]
		t := strings.TrimLeft(l, " ")
		if t != "\n" && t != "" && (!strings.HasPrefix(t, "#") || len(l)-len(t) > indent) {
			break
		}
		line--
	}
	return line
}

func isNull(n *yaml.Node) bool {
	return n.Kind == yaml.ScalarNode && n.Tag == "!!null"
}

// render writes rules as a YAML mapping indented by indent spaces.
func render(rules []NamedRule, indent int) (string, error) {
	m := &yaml.Node{Kind: yaml.MappingNode}
	for _, r := range rules {
		v, err := toNode(r.Rule, "")
		if err != nil {
			return "", err
		}
		m.Content = append(m.Content, &yaml.Node{Kind: yaml.ScalarNode, Value: r.Key}, v)
	}
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(m); err != nil {
		return "", err
	}
	if err := enc.Close(); err != nil {
		return "", err
	}
	pad := strings.Repeat(" ", indent)
	var out strings.Builder
	for l := range strings.Lines(buf.String()) {
		if l != "\n" {
			out.WriteString(pad)
		}
		out.WriteString(l)
	}
	return out.String(), nil
}

// keyOrder is the order in which the keys of rules, endpoints and their
// parts are written, as OpenShell's examples write them. Other keys follow
// in alphabetical order.
var keyOrder = []string{"name", "host", "port", "ports", "method", "path", "protocol", "tls", "enforcement", "access", "rules", "deny_rules", "endpoints", "binaries"}

func toNode(v any, parent string) (*yaml.Node, error) {
	switch v := v.(type) {
	case map[string]any:
		keys := slices.SortedFunc(maps.Keys(v), func(a, b string) int {
			i, j := slices.Index(keyOrder, a), slices.Index(keyOrder, b)
			switch {
			case i >= 0 && j >= 0:
				return i - j
			case i >= 0:
				return -1
			case j >= 0:
				return 1
			}
			return strings.Compare(a, b)
		})
		n := &yaml.Node{Kind: yaml.MappingNode}
		flat := true
		for _, k := range keys {
			c, err := toNode(v[k], k)
			if err != nil {
				return nil, err
			}
			flat = flat && c.Kind == yaml.ScalarNode
			n.Content = append(n.Content, &yaml.Node{Kind: yaml.ScalarNode, Value: k}, c)
		}
		// - allow: { method: GET, path: "/..." }
		if flat && (parent == "allow" || parent == "deny") {
			n.Style = yaml.FlowStyle
		}
		return n, nil
	case []any:
		n := &yaml.Node{Kind: yaml.SequenceNode}
		for _, e := range v {
			c, err := toNode(e, parent)
			if err != nil {
				return nil, err
			}
			n.Content = append(n.Content, c)
		}
		return n, nil
	}
	n := &yaml.Node{}
	if err := n.Encode(v); err != nil {
		return nil, err
	}
	return n, nil
}
