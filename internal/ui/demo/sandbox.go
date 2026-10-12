// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

package demo

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/dennisklein/brig/internal/ui/store"
)

// defaultSandboxImage is the image of a demo sandbox made without one.
const defaultSandboxImage = "ghcr.io/nvidia/openshell-community/sandboxes/base:latest"

// openshellPlan makes the made-up openshell command for argv, which names
// the gateway with -g and the workspace with --workspace first.
func (h *Host) openshellPlan(argv []string) (plan, error) {
	if len(argv) < 8 || argv[1] != "-g" || !strings.HasPrefix(argv[2], "brig-") || argv[3] != "--workspace" {
		return plan{}, fmt.Errorf("the demo cannot play %q", argv)
	}
	vm, ws := strings.TrimPrefix(argv[2], "brig-"), argv[4]
	if argv[5] == "policy" || argv[5] == "rule" {
		return h.rulePlan(vm, ws, argv[5:])
	}
	if argv[5] != "sandbox" {
		return plan{}, fmt.Errorf("the demo cannot play %q", argv)
	}
	rest := argv[6:]
	switch {
	case len(rest) == 2 && (rest[0] == "start" || rest[0] == "stop"):
		return sandboxPhasePlan(vm, ws, rest[0], rest[1]), nil
	case len(rest) == 2 && rest[0] == "delete":
		name := rest[1]
		return plan{
			steps:  []step{{name: "Delete sandbox " + name}, {name: "Remove its workspace"}},
			output: "Deleted sandbox " + name + "\n",
			gwVM:   vm,
			gw: func(g *store.Gateway) {
				g.Sandboxes = slices.DeleteFunc(slices.Clone(g.Sandboxes), func(s store.Sandbox) bool { return s.Workspace == ws && s.Name == name })
			},
		}, nil
	case len(rest) >= 4 && rest[0] == "provider" && (rest[1] == "attach" || rest[1] == "detach"):
		return providerPlan(vm, ws, rest[1], rest[2], rest[3]), nil
	}
	return plan{}, fmt.Errorf("the demo cannot play %q", argv)
}

// setSandbox changes the named sandbox of the workspace ws of g with fn.
func setSandbox(g *store.Gateway, ws, name string, fn func(*store.Sandbox)) {
	g.Sandboxes = slices.Clone(g.Sandboxes)
	if i := slices.IndexFunc(g.Sandboxes, func(s store.Sandbox) bool { return s.Workspace == ws && s.Name == name }); i >= 0 {
		fn(&g.Sandboxes[i])
	}
}

func sandboxPhasePlan(vm, ws, verb, name string) plan {
	phase, did := "Ready", "Started"
	if verb == "stop" {
		phase, did = "Stopped", "Stopped"
	}
	return plan{
		steps:  []step{{name: strings.ToUpper(verb[:1]) + verb[1:] + " sandbox " + name}, {name: "Wait for the supervisor"}},
		output: did + " sandbox " + name + "\n",
		gwVM:   vm,
		gw: func(g *store.Gateway) {
			setSandbox(g, ws, name, func(s *store.Sandbox) { s.Phase, s.Problem = phase, "" })
		},
	}
}

func providerPlan(vm, ws, verb, name, provider string) plan {
	return plan{
		steps:  []step{{name: strings.ToUpper(verb[:1]) + verb[1:] + " provider " + provider}, {name: "Wait for the sandbox to apply it"}},
		output: "Provider " + provider + " " + verb + "ed " + map[string]string{"attach": "to", "detach": "from"}[verb] + " " + name + "\n",
		gwVM:   vm,
		gw: func(g *store.Gateway) {
			setSandbox(g, ws, name, func(s *store.Sandbox) {
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
// what follows "brig sandbox create": the VM, its --workspace and --mount
// options, then -- and the options of openshell sandbox create.
func (h *Host) sandboxCreatePlan(rest []string) (plan, error) {
	if len(rest) == 0 {
		return plan{}, fmt.Errorf("the demo cannot play brig sandbox create without a VM")
	}
	vm := rest[0]
	var mounts, providers []string
	name, ws := "", store.DefaultWorkspace
	var template, image, policyFile string
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
		case !after && a == "--workspace":
			ws = next()
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
			if i := slices.IndexFunc(g.Templates, func(t store.Template) bool { return t.Workspace == ws && t.Name == template }); i >= 0 {
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
			Workspace: ws, Name: name, Phase: "Ready", Image: image, Template: template, Providers: providers,
			Created: time.Now(), PolicyVersion: 1, Policy: policy(base, 1, providers),
		})
		slices.SortFunc(g.Sandboxes, func(a, b store.Sandbox) int {
			return cmp.Or(strings.Compare(a.Workspace, b.Workspace), strings.Compare(a.Name, b.Name))
		})
	}
	return p, nil
}

// ListFiles implements ui.FileLister with the same made-up files in every
// sandbox.
func (h *Host) ListFiles(ctx context.Context, _, _, _, dir string) (store.Dir, error) {
	select {
	case <-ctx.Done():
		return store.Dir{}, ctx.Err()
	case <-time.After(150 * time.Millisecond):
	}
	now := time.Now()
	file := func(name string, size int64, ago time.Duration) store.File {
		return store.File{Name: name, Kind: "file", Size: size, Mod: now.Add(-ago)}
	}
	dirent := func(name string, ago time.Duration) store.File {
		return store.File{Name: name, Kind: "dir", Size: 4096, Mod: now.Add(-ago)}
	}
	switch dir {
	case "/sandbox":
		return store.Dir{Files: []store.File{
			dirent("webapp", 3*time.Hour), dirent(".pi", 26*time.Hour), file("notes.md", 1840, 2*24*time.Hour),
			file(".bashrc", 412, 9*24*time.Hour),
			{Name: "latest", Kind: "link", Size: 6, Mod: now.Add(-24 * time.Hour)},
		}}, nil
	case "/sandbox/webapp":
		return store.Dir{Files: []store.File{
			dirent(".git", 3*time.Hour), dirent("src", 5*time.Hour), dirent("node_modules", 8*24*time.Hour),
			file("package.json", 1290, 8*24*time.Hour), file("README.md", 3401, 8*24*time.Hour), file("package-lock.json", 183420, 8*24*time.Hour),
		}}, nil
	case "/sandbox/webapp/src":
		return store.Dir{Files: []store.File{
			file("index.ts", 2210, 5*time.Hour), file("app.ts", 9873, 5*time.Hour), file("util.ts", 640, 4*24*time.Hour),
		}}, nil
	case "/sandbox/.pi":
		return store.Dir{Files: []store.File{dirent("agent", 26*time.Hour)}}, nil
	}
	return store.Dir{}, nil
}
