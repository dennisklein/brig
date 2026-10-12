// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

package demo

import (
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/dennisklein/brig/internal/ui/store"
)

// defaultSandboxImage is the image of a demo sandbox made without one.
const defaultSandboxImage = "ghcr.io/nvidia/openshell-community/sandboxes/base:latest"

// openshellPlan makes the made-up openshell command for argv, which names
// the gateway with -g first.
func (h *Host) openshellPlan(argv []string) (plan, error) {
	if len(argv) < 6 || argv[1] != "-g" || !strings.HasPrefix(argv[2], "brig-") || argv[3] != "sandbox" {
		return plan{}, fmt.Errorf("the demo cannot play %q", argv)
	}
	vm := strings.TrimPrefix(argv[2], "brig-")
	rest := argv[4:]
	switch {
	case len(rest) == 2 && (rest[0] == "start" || rest[0] == "stop"):
		return sandboxPhasePlan(vm, rest[0], rest[1]), nil
	case len(rest) == 2 && rest[0] == "delete":
		name := rest[1]
		return plan{
			steps:  []step{{name: "Delete sandbox " + name}, {name: "Remove its workspace"}},
			output: "Deleted sandbox " + name + "\n",
			gwVM:   vm,
			gw: func(g *store.Gateway) {
				g.Sandboxes = slices.DeleteFunc(slices.Clone(g.Sandboxes), func(s store.Sandbox) bool { return s.Name == name })
			},
		}, nil
	case len(rest) >= 4 && rest[0] == "provider" && (rest[1] == "attach" || rest[1] == "detach"):
		return providerPlan(vm, rest[1], rest[2], rest[3]), nil
	}
	return plan{}, fmt.Errorf("the demo cannot play %q", argv)
}

// setSandbox changes the named sandbox of g with fn.
func setSandbox(g *store.Gateway, name string, fn func(*store.Sandbox)) {
	g.Sandboxes = slices.Clone(g.Sandboxes)
	if i := slices.IndexFunc(g.Sandboxes, func(s store.Sandbox) bool { return s.Name == name }); i >= 0 {
		fn(&g.Sandboxes[i])
	}
}

func sandboxPhasePlan(vm, verb, name string) plan {
	phase, did := "Ready", "Started"
	if verb == "stop" {
		phase, did = "Stopped", "Stopped"
	}
	return plan{
		steps:  []step{{name: strings.ToUpper(verb[:1]) + verb[1:] + " sandbox " + name}, {name: "Wait for the supervisor"}},
		output: did + " sandbox " + name + "\n",
		gwVM:   vm,
		gw:     func(g *store.Gateway) { setSandbox(g, name, func(s *store.Sandbox) { s.Phase, s.Problem = phase, "" }) },
	}
}

func providerPlan(vm, verb, name, provider string) plan {
	return plan{
		steps:  []step{{name: strings.ToUpper(verb[:1]) + verb[1:] + " provider " + provider}, {name: "Wait for the sandbox to apply it"}},
		output: "Provider " + provider + " " + verb + "ed " + map[string]string{"attach": "to", "detach": "from"}[verb] + " " + name + "\n",
		gwVM:   vm,
		gw: func(g *store.Gateway) {
			setSandbox(g, name, func(s *store.Sandbox) {
				providers := slices.DeleteFunc(slices.Clone(s.Providers), func(p string) bool { return p == provider })
				if verb == "attach" {
					providers = append(providers, provider)
				}
				slices.Sort(providers)
				base := ""
				if s.Policy != nil {
					base = s.Policy.Base
				}
				s.Providers = providers
				s.PolicyVersion++
				s.Policy = policy(base, s.PolicyVersion, providers)
			})
		},
	}
}

// sandboxCreatePlan makes the made-up brig sandbox create command. rest is
// what follows "brig sandbox create": the VM, its --mount options, then --
// and the options of openshell sandbox create.
func (h *Host) sandboxCreatePlan(rest []string) (plan, error) {
	if len(rest) == 0 {
		return plan{}, fmt.Errorf("the demo cannot play brig sandbox create without a VM")
	}
	vm := rest[0]
	var mounts, providers []string
	var name, template, image, policyFile string
	after := false
	for i := 1; i < len(rest); i++ {
		a := rest[i]
		next := func() string {
			if i+1 < len(rest) {
				i++
				return rest[i]
			}
			return ""
		}
		switch {
		case a == "--":
			after = true
		case !after && a == "--mount":
			mounts = append(mounts, next())
		case after && a == "--name":
			name = next()
		case after && a == "--template":
			template = next()
		case after && a == "--from":
			image = next()
		case after && a == "--provider":
			providers = append(providers, next())
		case after && a == "--policy":
			policyFile = next()
		}
	}
	if name == "" {
		return plan{}, fmt.Errorf("the demo cannot play brig sandbox create without a name")
	}
	p := plan{gwVM: vm, output: "Created sandbox " + name + "\n"}
	for _, m := range mounts {
		src, _, _ := strings.Cut(m, ":")
		p.steps = append(p.steps, step{name: "Share " + src + " read-only"})
	}
	p.steps = append(p.steps, step{name: "Create sandbox " + name}, step{name: "Wait for " + name + " to run"})
	p.gw = func(g *store.Gateway) {
		if template != "" {
			if i := slices.IndexFunc(g.Templates, func(t store.Template) bool { return t.Name == template }); i >= 0 {
				image = g.Templates[i].Image
			}
		}
		if image == "" {
			image = defaultSandboxImage
		}
		base := defaultPolicy
		if strings.HasSuffix(policyFile, "/tools.yaml") {
			base = toolsPolicy
		}
		slices.Sort(providers)
		g.Sandboxes = append(slices.Clone(g.Sandboxes), store.Sandbox{
			Name: name, Phase: "Ready", Image: image, Template: template, Providers: providers,
			Created: time.Now(), PolicyVersion: 1, Policy: policy(base, 1, providers),
		})
		slices.SortFunc(g.Sandboxes, func(a, b store.Sandbox) int { return strings.Compare(a.Name, b.Name) })
	}
	return p, nil
}
