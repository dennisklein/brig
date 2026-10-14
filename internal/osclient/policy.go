// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

package osclient

import (
	"bytes"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"

	sb "github.com/NVIDIA/OpenShell/sdk/go/proto/sandboxv1"
	"go.yaml.in/yaml/v3"
	"google.golang.org/protobuf/encoding/protojson"
)

// providerRulePrefix starts the keys of the network rules that attached
// providers add to a sandbox's policy. openshell policy get --base leaves
// them out.
const providerRulePrefix = "_provider_"

// policy is a sandbox policy in the shape of a policy file, the shape that
// openshell policy get prints: enums as the words policy files use, a
// single port as port, filesystem as filesystem_policy, unset fields left
// out (openshell-policy's from_proto at OpenShell v0.1.4).
type policy map[string]any

// policyDoc converts a policy from its protobuf form.
func policyDoc(p *sb.SandboxPolicy) (policy, error) {
	data, err := protojson.MarshalOptions{UseProtoNames: true}.Marshal(p)
	if err != nil {
		return nil, err
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var m map[string]any
	if err := dec.Decode(&m); err != nil {
		return nil, err
	}
	out := policy{"version": json.Number("1")}
	for k, v := range m {
		switch k {
		case "version":
			if v != json.Number("0") {
				out[k] = v
			}
		case "filesystem":
			fs, _ := v.(map[string]any)
			if _, ok := fs["include_workdir"]; !ok && fs != nil {
				fs["include_workdir"] = false
			}
			out["filesystem_policy"] = fs
		case "landlock":
			ll, _ := v.(map[string]any)
			if ll != nil && ll["compatibility"] == nil {
				ll["compatibility"] = "best_effort"
			}
			out[k] = ll
		case "network_policies":
			rules, _ := v.(map[string]any)
			for _, r := range rules {
				rule, _ := r.(map[string]any)
				eps, _ := rule["endpoints"].([]any)
				for _, e := range eps {
					if ep, ok := e.(map[string]any); ok {
						endpoint(ep)
					}
				}
			}
			out[k] = rules
		default:
			out[k] = v
		}
	}
	return out, nil
}

// enums maps the protobuf names of an endpoint's enum values to the words
// of policy files, by field.
var enums = map[string]map[string]string{
	"tls": {
		"NETWORK_TLS_MODE_SKIP":        "skip",
		"NETWORK_TLS_MODE_TERMINATE":   "terminate",
		"NETWORK_TLS_MODE_PASSTHROUGH": "passthrough",
	},
	"enforcement": {
		"NETWORK_ENFORCEMENT_MODE_ENFORCE": "enforce",
		"NETWORK_ENFORCEMENT_MODE_AUDIT":   "audit",
	},
	"access": {
		"NETWORK_ACCESS_PRESET_READ_ONLY":  "read-only",
		"NETWORK_ACCESS_PRESET_READ_WRITE": "read-write",
		"NETWORK_ACCESS_PRESET_FULL":       "full",
	},
}

// serverOnly are endpoint fields that the gateway sets and policy files do
// not have.
var serverOnly = []string{"advisor_proposed", "provider_credentialed", "token_grant_owner"}

// endpoint rewrites an endpoint as a policy file writes it.
func endpoint(ep map[string]any) {
	for field, words := range enums {
		if s, ok := ep[field].(string); ok {
			// An unspecified value is left out; one this version
			// does not know shows as it came.
			if w, ok := words[s]; ok {
				ep[field] = w
			} else if strings.HasSuffix(s, "_UNSPECIFIED") {
				delete(ep, field)
			}
		}
	}
	for _, k := range serverOnly {
		delete(ep, k)
	}
	if ports, ok := ep["ports"].([]any); ok && len(ports) == 1 {
		ep["port"] = ports[0]
		delete(ep, "ports")
	}
	mcp := strings.HasPrefix(fmt.Sprint(ep["protocol"]), "mcp")
	if n, ok := ep["json_rpc_max_body_bytes"]; ok {
		delete(ep, "json_rpc_max_body_bytes")
		if mcp {
			m, _ := ep["mcp"].(map[string]any)
			if m == nil {
				m = map[string]any{}
			}
			m["max_body_bytes"] = n
			ep["mcp"] = m
		} else {
			ep["json_rpc"] = map[string]any{"max_body_bytes": n}
		}
	}
}

// base returns the policy without the providers' rules.
func (p policy) base() policy {
	out := maps.Clone(p)
	rules, ok := p["network_policies"].(map[string]any)
	if !ok {
		return out
	}
	kept := map[string]any{}
	for k, v := range rules {
		if !strings.HasPrefix(k, providerRulePrefix) {
			kept[k] = v
		}
	}
	if len(kept) == 0 {
		delete(out, "network_policies")
	} else {
		out["network_policies"] = kept
	}
	return out
}

// providerRules returns the keys of the providers' rules, sorted.
func (p policy) providerRules() []string {
	rules, _ := p["network_policies"].(map[string]any)
	var out []string
	for k := range rules {
		if strings.HasPrefix(k, providerRulePrefix) {
			out = append(out, k)
		}
	}
	slices.Sort(out)
	return out
}

// keyOrder lists the keys of a mapping in the order policy files write
// them, by the kind of mapping; other keys follow in sorted order.
var keyOrder = map[string][]string{
	"top":      {"version", "filesystem_policy", "landlock", "process", "network_policies", "network_middlewares"},
	"rule":     {"name", "endpoints", "binaries"},
	"endpoint": {"host", "path", "port", "ports", "protocol", "tls", "enforcement", "access", "rules", "allowed_ips", "deny_rules"},
}

// yaml writes the policy as YAML, two spaces per level.
func (p policy) yaml() (string, error) {
	n, err := node(map[string]any(p), "top")
	if err != nil {
		return "", err
	}
	return encode(n)
}

// ruleYAML writes a network rule as a policy file does, under its key.
func ruleYAML(key string, r *sb.NetworkPolicyRule) (string, error) {
	data, err := protojson.MarshalOptions{UseProtoNames: true}.Marshal(r)
	if err != nil {
		return "", err
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var rule map[string]any
	if err := dec.Decode(&rule); err != nil {
		return "", err
	}
	eps, _ := rule["endpoints"].([]any)
	for _, e := range eps {
		if ep, ok := e.(map[string]any); ok {
			endpoint(ep)
		}
	}
	n, err := node(map[string]any{key: rule}, "rules")
	if err != nil {
		return "", err
	}
	return encode(n)
}

// encode writes n as YAML, two spaces per level.
func encode(n *yaml.Node) (string, error) {
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(n); err != nil {
		return "", err
	}
	if err := enc.Close(); err != nil {
		return "", err
	}
	return buf.String(), nil
}

// node builds the YAML node of v, a mapping of the given kind. The kinds of
// nested mappings follow from their keys.
func node(v any, kind string) (*yaml.Node, error) {
	switch v := v.(type) {
	case map[string]any:
		n := &yaml.Node{Kind: yaml.MappingNode}
		for _, k := range orderedKeys(v, kind) {
			child := ""
			switch {
			case kind == "top" && k == "network_policies":
				child = "rules"
			case kind == "rules":
				child = "rule"
			case kind == "rule" && k == "endpoints":
				child = "endpoints"
			}
			val, err := node(v[k], child)
			if err != nil {
				return nil, err
			}
			n.Content = append(n.Content, &yaml.Node{Kind: yaml.ScalarNode, Value: k}, val)
		}
		return n, nil
	case []any:
		n := &yaml.Node{Kind: yaml.SequenceNode}
		child := ""
		if kind == "endpoints" {
			child = "endpoint"
		}
		for _, item := range v {
			c, err := node(item, child)
			if err != nil {
				return nil, err
			}
			n.Content = append(n.Content, c)
		}
		return n, nil
	case json.Number:
		n := &yaml.Node{}
		if i, err := v.Int64(); err == nil {
			return n, n.Encode(i)
		}
		f, err := v.Float64()
		if err != nil {
			return nil, err
		}
		return n, n.Encode(f)
	default:
		n := &yaml.Node{}
		return n, n.Encode(v)
	}
}

// orderedKeys returns the keys of m in the order of its kind.
func orderedKeys(m map[string]any, kind string) []string {
	rank := map[string]int{}
	for i, k := range keyOrder[kind] {
		rank[k] = i + 1
	}
	keys := slices.Collect(maps.Keys(m))
	slices.SortFunc(keys, func(a, b string) int {
		ra, rb := rank[a], rank[b]
		switch {
		case ra != 0 && rb != 0:
			return ra - rb
		case ra != 0:
			return -1
		case rb != 0:
			return 1
		}
		return strings.Compare(a, b)
	})
	return keys
}

// PolicyJSON converts a policy file to the protobuf JSON of the policy, as
// openshell does when it creates a sandbox with --policy: the shape that
// policyDoc writes, read back. A key the protobuf lacks is an error.
func PolicyJSON(file []byte) (json.RawMessage, error) {
	var doc map[string]any
	if err := yaml.Unmarshal(file, &doc); err != nil {
		return nil, err
	}
	if fs, ok := doc["filesystem_policy"]; ok {
		delete(doc, "filesystem_policy")
		doc["filesystem"] = fs
	}
	rules, _ := doc["network_policies"].(map[string]any)
	for _, r := range rules {
		rule, _ := r.(map[string]any)
		eps, _ := rule["endpoints"].([]any)
		for _, e := range eps {
			if ep, ok := e.(map[string]any); ok {
				protoEndpoint(ep)
			}
		}
	}
	data, err := json.Marshal(doc)
	if err != nil {
		return nil, err
	}
	p := &sb.SandboxPolicy{}
	if err := protojson.Unmarshal(data, p); err != nil {
		return nil, fmt.Errorf("the policy file does not fit OpenShell's policy: %w", err)
	}
	return protojson.Marshal(p)
}

// protoEndpoint rewrites an endpoint of a policy file as the protobuf has
// it, undoing endpoint.
func protoEndpoint(ep map[string]any) {
	for field, words := range enums {
		if s, ok := ep[field].(string); ok {
			for name, word := range words {
				if word == s {
					ep[field] = name
				}
			}
		}
	}
	for _, k := range []string{"json_rpc", "mcp"} {
		m, ok := ep[k].(map[string]any)
		if !ok {
			continue
		}
		if n, ok := m["max_body_bytes"]; ok {
			ep["json_rpc_max_body_bytes"] = n
			delete(m, "max_body_bytes")
		}
		if k == "json_rpc" || len(m) == 0 {
			delete(ep, k)
		}
	}
}
