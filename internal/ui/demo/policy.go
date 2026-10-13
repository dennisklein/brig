// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

package demo

import (
	"fmt"
	"strings"

	"github.com/dennisklein/brig/internal/ui/store"
)

// rulePlan plays an openshell command on the gateway of the VM named vm
// that adds or approves a rule, as Allow… does. h.mu is held.
func (h *Host) rulePlan(vm, ws string, args []string) (plan, error) {
	if len(args) < 3 {
		return plan{}, fmt.Errorf("the demo cannot play openshell %q", args)
	}
	f := parseFlags(args[2:])
	if len(f.args) == 0 {
		return plan{}, fmt.Errorf("the demo cannot play openshell %q", args)
	}
	sandbox := f.args[0]
	switch args[0] + " " + args[1] {
	case "policy update":
		rule, binary, spec := first(f.values["--rule-name"]), first(f.values["--binary"]), first(f.values["--add-endpoint"])
		parts := strings.Split(spec, ":")
		if len(parts) < 2 {
			return plan{}, fmt.Errorf("the demo cannot read --add-endpoint %q", spec)
		}
		yaml := rule + ":\n  name: " + rule + "\n  endpoints:\n    - host: " + parts[0] + "\n      port: " + parts[1] + "\n"
		if len(parts) == 5 {
			yaml += "      protocol: " + parts[3] + "\n      enforcement: " + parts[4] + "\n      access: " + parts[2] + "\n"
		}
		yaml += "  binaries:\n    - path: " + binary + "\n"
		return plan{
			steps:  []step{{name: "Validate the rule"}, {name: "Merge it into the policy of " + sandbox}, {name: "Wait for " + sandbox + " to load it"}},
			output: fmt.Sprintf("✓ Rule %s updated on sandbox %s\n✓ Policy loaded by the sandbox\n", rule, sandbox),
			after: func() {
				addRule(h.st, vm, ws, sandbox, yaml)
				h.traffic.allow(vm, ws, sandbox, parts[0]+":"+parts[1], binary, rule)
			},
		}, nil
	case "rule approve":
		id := first(f.values["--chunk-id"])
		d, ok := h.traffic.draft(vm, ws, sandbox, id)
		if !ok {
			return plan{steps: []step{{name: "Approve draft " + id}}, fail: "no pending draft " + id + " for sandbox " + sandbox}, nil
		}
		return plan{
			steps:  []step{{name: "Approve draft " + id}, {name: "Merge it into the policy of " + sandbox}},
			output: fmt.Sprintf("✓ Approved draft %s (%s) for sandbox %s\n", id, d.Rule, sandbox),
			after: func() {
				addRule(h.st, vm, ws, sandbox, d.YAML)
				for _, e := range d.Endpoints {
					h.traffic.allow(vm, ws, sandbox, fmt.Sprintf("%s:%d", e.Host, e.Port), d.Binary, d.Rule)
				}
			},
		}, nil
	case "rule reject":
		id := first(f.values["--chunk-id"])
		d, ok := h.traffic.draft(vm, ws, sandbox, id)
		if !ok {
			return plan{steps: []step{{name: "Reject draft " + id}}, fail: "no pending draft " + id + " for sandbox " + sandbox}, nil
		}
		return plan{
			steps:  []step{{name: "Reject draft " + id}},
			output: fmt.Sprintf("✓ Rejected draft %s (%s) for sandbox %s\n", id, d.Rule, sandbox),
			after:  func() { h.traffic.reject(vm, ws, sandbox, id) },
		}, nil
	}
	return plan{}, fmt.Errorf("the demo cannot play openshell %s %s", args[0], args[1])
}

func first(xs []string) string {
	if len(xs) == 0 {
		return ""
	}
	return xs[0]
}

// addRule adds a rule, policy-file YAML under its key, to the base policy
// of the sandbox in the workspace ws.
func addRule(st *store.Store, vm, ws, sandbox, yaml string) {
	st.UpdateGateway(vm, func(gw *store.Gateway) bool {
		for i, s := range gw.Sandboxes {
			if s.Workspace != ws || s.Name != sandbox || s.Policy == nil {
				continue
			}
			base := s.Policy.Base
			if !strings.Contains(base, "network_policies:") {
				base += "network_policies:\n"
			}
			for _, line := range strings.SplitAfter(strings.TrimSuffix(yaml, "\n"), "\n") {
				base += "  " + strings.TrimSuffix(line, "\n") + "\n"
			}
			gw.Sandboxes = append([]store.Sandbox(nil), gw.Sandboxes...)
			s.PolicyVersion++
			s.Policy = policy(base, s.PolicyVersion, s.Providers)
			gw.Sandboxes[i] = s
			return true
		}
		return false
	})
}

// policyAllowPlan plays brig policy allow, which argv holds after "policy
// allow": VM, sandbox and host, or with --draft only the first two, then the
// options. With --save it refuses as brig does before it changes the
// sandbox. h.mu is held.
func (h *Host) policyAllowPlan(argv []string) (plan, error) {
	f := parseFlags(argv)
	draft := first(f.values["--draft"])
	if len(f.args) < 2 || (draft == "" && len(f.args) < 3) {
		return plan{}, fmt.Errorf("the demo cannot play brig policy allow %q", argv)
	}
	vm, sandbox, ws := f.args[0], f.args[1], first(f.values["--workspace"])
	rule := first(f.values["--rule"])
	var args []string
	if draft != "" {
		args = []string{"rule", "approve", sandbox, "--chunk-id", draft}
	} else {
		spec := f.args[2]
		if !f.set["--l4"] {
			spec += ":" + first(f.values["--access"]) + ":rest:enforce"
			if f.set["--audit"] {
				spec = strings.TrimSuffix(spec, "enforce") + "audit"
			}
		}
		args = []string{"policy", "update", sandbox, "--rule-name", rule, "--binary", first(f.values["--binary"]), "--add-endpoint", spec, "--wait"}
	}
	allow, err := h.rulePlan(vm, ws, args)
	if err != nil {
		return plan{}, err
	}
	if !f.set["--save"] {
		allow.output += fmt.Sprintf("Only %s has the rule. brig policy save %s %s --rule %s keeps it for new sandboxes.\n", sandbox, vm, sandbox, rule)
		return allow, nil
	}
	save := policySavePlan(vm, sandbox, []string{"--rule", rule, "--to", first(f.values["--to"])})
	if save.fail != "" {
		// Nothing changes: brig checks before it adds the rule.
		return plan{steps: save.steps, fail: save.fail}, nil
	}
	allow.steps = append(allow.steps, save.steps...)
	allow.output += save.output
	return allow, nil
}

// policySavePlan plays brig policy save. The demo's sandboxes come from the
// default policy, so that it refuses as brig does without --to.
func policySavePlan(vm, sandbox string, args []string) plan {
	f := parseFlags(args)
	steps := []step{{name: "Compare " + sandbox + " with its policy file"}}
	to := first(f.values["--to"])
	if to == "" {
		return plan{
			steps: steps,
			fail: fmt.Sprintf("sandbox %s came from the default policy %s/policies/default.yaml, which every new sandbox gets; "+
				"save into a policy file of its own with --to %s, or into the default with --to %s/policies/default.yaml", sandbox, tildeDir, sandbox, tildeDir),
		}
	}
	target := to
	if !strings.Contains(to, "/") {
		target = tildeDir + "/policies/" + strings.TrimSuffix(to, ".yaml") + ".yaml"
	}
	rules := "every unsaved rule"
	if r := f.values["--rule"]; len(r) > 0 {
		rules = strings.Join(r, ", ")
	}
	if f.set["--dry-run"] {
		return plan{
			steps: steps,
			output: fmt.Sprintf("Would create %s as a copy of %s/policies/default.yaml with rules %s:\n", target, tildeDir, rules) +
				"--- " + target + "\n+++ " + target + "\n@@ network_policies @@\n   pypi:\n" +
				"+  allow_registry_npmjs_org_443:\n+    name: allow_registry_npmjs_org_443\n+    endpoints:\n+      - host: registry.npmjs.org\n+        port: 443\n" +
				"(made-up diff; nothing was compared)\n",
		}
	}
	return plan{
		steps:  append(steps, step{name: "Write " + target}),
		output: fmt.Sprintf("Created %s with rules %s.\nbrig remembers it as the policy file of sandbox %s on %s.\n", target, rules, sandbox, vm),
	}
}

// tildeDir is the demo's OpenShell config directory as a user writes it.
const tildeDir = "~/src/agent-openshell"
