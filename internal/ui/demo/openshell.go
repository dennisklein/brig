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

// configDir is the demo's OpenShell config directory.
const configDir = Home + "/src/agent-openshell"

// defaultPolicy is the demo's policies/default.yaml.
const defaultPolicy = `version: 1
filesystem_policy:
  include_workdir: true
  read_only:
    - /usr
    - /lib
    - /etc
    - /proc
  read_write:
    - /sandbox
    - /tmp
    - /dev/null
landlock:
  compatibility: best_effort
process:
  run_as_user: sandbox
  run_as_group: sandbox
network_policies:
  pypi:
    name: pypi
    endpoints:
      - host: pypi.org
        port: 443
        access: read-only
    binaries:
      - path: /usr/bin/pip
`

// toolsPolicy is the demo's policies/tools.yaml: no network rules.
const toolsPolicy = `version: 1
filesystem_policy:
  include_workdir: true
  read_only:
    - /usr
    - /lib
    - /etc
    - /opt
  read_write:
    - /sandbox
    - /tmp
landlock:
  compatibility: best_effort
process:
  run_as_user: sandbox
  run_as_group: sandbox
`

// ProviderProfiles returns the demo's provider profiles, sorted by ID. The
// profiles of the workspace scope have no workspace yet; profilesIn gives
// them one.
func ProviderProfiles() []store.Profile {
	return []store.Profile{
		{
			ID: "claude-code", Name: "Claude Code", Category: "agent", Source: "user", Scope: "workspace",
			Credentials: []store.ProfileCredential{{Name: "oauth_token", EnvVars: []string{"CLAUDE_CODE_OAUTH_TOKEN"}, Auth: "bearer", Required: true}},
			Endpoints:   []string{"api.anthropic.com:443", "statsig.anthropic.com:443"},
			Binaries:    []string{"/usr/local/bin/claude"},
		},
		{
			ID: "github", Name: "GitHub", Category: "source control", Source: "user", Scope: "workspace",
			Credentials: []store.ProfileCredential{{Name: "token", EnvVars: []string{"GH_TOKEN"}, Auth: "bearer", Required: true}},
			Endpoints:   []string{"api.github.com:443", "github.com:443"},
			Binaries:    []string{"/usr/bin/gh", "/usr/bin/git", "/usr/libexec/git-core/git-remote-https"},
		},
		{
			ID: "gitlab-work", Name: "GitLab at work", Category: "source control", Source: "user", Scope: "workspace",
			Credentials: []store.ProfileCredential{{Name: "token", EnvVars: []string{"GITLAB_TOKEN"}, Auth: "header PRIVATE-TOKEN", Required: true}},
			Endpoints:   []string{"gitlab.example.org:443"},
			Binaries:    []string{"/usr/bin/glab", "/usr/bin/git"},
		},
		{
			ID: "openai", Name: "OpenAI-compatible API", Category: "inference", Source: "builtin", Scope: "platform",
			Credentials: []store.ProfileCredential{{Name: "api_key", EnvVars: []string{"OPENAI_API_KEY"}, Auth: "bearer", Required: true}},
			Endpoints:   []string{"models.example.org:443"},
			Binaries:    []string{"/usr/bin/node"},
		},
	}
}

// profilesIn returns the demo's profiles as a gateway lists them for the
// workspaces: the platform's once, and the profiles of the workspace scope
// for each workspace, sorted by workspace and ID. The profiles of a workspace
// that are not named in only are left out.
func profilesIn(workspaces []string, only map[string][]string) []store.Profile {
	var platform, own []store.Profile
	for _, p := range ProviderProfiles() {
		if p.Scope == "platform" {
			platform = append(platform, p)
		}
	}
	for _, ws := range workspaces {
		for _, p := range ProviderProfiles() {
			if p.Scope == "platform" || (only[ws] != nil && !slices.Contains(only[ws], p.ID)) {
				continue
			}
			p.Workspace = ws
			own = append(own, p)
		}
	}
	return append(platform, own...)
}

// demoProviders are the demo's providers by name, with their types.
var demoProviders = map[string]string{"llm": "openai", "github": "github", "gitlab-work": "gitlab-work", "claude-code": "claude-code"}

// providerRule writes the network rule that attaching provider name adds to
// a sandbox's policy, two levels deep under network_policies.
func providerRule(name string) string {
	var p store.Profile
	for _, pr := range ProviderProfiles() {
		if pr.ID == demoProviders[name] {
			p = pr
		}
	}
	var b strings.Builder
	key := "_provider_" + strings.ReplaceAll(name, "-", "_")
	fmt.Fprintf(&b, "  %s:\n    name: %s\n    endpoints:\n", key, key)
	for _, e := range p.Endpoints {
		host, port, _ := strings.Cut(e, ":")
		fmt.Fprintf(&b, "      - host: %s\n        port: %s\n", host, port)
	}
	b.WriteString("    binaries:\n")
	for _, bin := range p.Binaries {
		fmt.Fprintf(&b, "      - path: %s\n", bin)
	}
	return b.String()
}

// policy returns a sandbox's policy: base from a policy file, plus the rules
// of its providers.
func policy(base string, version uint32, providers []string) *store.Policy {
	p := &store.Policy{Base: base, Effective: base, Version: version}
	if len(providers) == 0 {
		return p
	}
	if !strings.Contains(base, "network_policies:") {
		p.Effective += "network_policies:\n"
	}
	names := slices.Sorted(slices.Values(providers))
	for _, name := range names {
		p.Effective += providerRule(name)
		p.ProviderRules = append(p.ProviderRules, "_provider_"+strings.ReplaceAll(name, "-", "_"))
	}
	return p
}

// Gateways returns the demo's gateways as of now.
func Gateways(now time.Time) []store.Gateway {
	ago := func(d time.Duration) time.Time { return now.Add(-d) }
	sandboxIn := func(ws, name, phase, image, template string, created time.Duration, base string, version uint32, providers ...string) store.Sandbox {
		providers = slices.Sorted(slices.Values(providers))
		return store.Sandbox{
			Workspace: ws, Name: name, Phase: phase, Image: image, Template: template, Providers: providers,
			Created: ago(created), PolicyVersion: version, Policy: policy(base, version, providers),
		}
	}
	sandbox := func(name, phase, image, template string, created time.Duration, base string, version uint32, providers ...string) store.Sandbox {
		return sandboxIn(store.DefaultWorkspace, name, phase, image, template, created, base, version, providers...)
	}
	providersIn := func(ws string, names ...string) []store.Provider {
		var out []store.Provider
		for _, n := range slices.Sorted(slices.Values(names)) {
			var creds []string
			for _, p := range ProviderProfiles() {
				if p.ID == demoProviders[n] {
					creds = p.Credentials[0].EnvVars
				}
			}
			out = append(out, store.Provider{Workspace: ws, Name: n, Type: demoProviders[n], Credentials: creds})
		}
		return out
	}
	providers := func(names ...string) []store.Provider { return providersIn(store.DefaultWorkspace, names...) }
	def := []string{store.DefaultWorkspace}
	config := func(providers ...string) store.GatewayConfig {
		c := store.GatewayConfig{
			Dirs:              []string{configDir},
			Workspaces:        map[string]store.WorkspaceConfig{store.DefaultWorkspace: {Providers: map[string]string{}, Profiles: map[string]string{}}},
			DefaultPolicy:     configDir + "/policies/default.yaml",
			DefaultPolicyText: defaultPolicy,
			Policies:          []string{configDir + "/policies/default.yaml", configDir + "/policies/tools.yaml"},
		}
		for _, p := range providers {
			c.Workspaces[store.DefaultWorkspace].Providers[p] = configDir + "/providers/" + p + ".yaml"
		}
		for _, id := range []string{"claude-code", "github", "gitlab-work"} {
			c.Workspaces[store.DefaultWorkspace].Profiles[id] = configDir + "/profiles/" + id + ".yaml"
		}
		return c
	}
	// withTeam adds the providers and profiles that the config directories
	// define for the workspace team.
	withTeam := func(c store.GatewayConfig) store.GatewayConfig {
		c.Workspaces["team"] = store.WorkspaceConfig{
			Providers: map[string]string{"github": configDir + "/workspaces/team/providers/github.yaml", "llm": configDir + "/workspaces/team/providers/llm.yaml"},
			Profiles:  map[string]string{"github": configDir + "/workspaces/team/profiles/github.yaml"},
		}
		return c
	}
	const (
		pi     = "localhost/pi-agent:2026-10-08"
		claude = "localhost/claude-agent:2026-10-01"
	)
	return []store.Gateway{
		{
			Workspaces: []string{store.DefaultWorkspace}, VM: "airgap", State: store.GatewayStopped, Version: "0.1.1", Seen: ago(6 * 24 * time.Hour),
			Sandboxes: []store.Sandbox{
				sandbox("corpus-scan", "Stopped", "localhost/pi-agent:2026-09-15", "pi", 30*24*time.Hour, defaultPolicy, 1, "llm"),
			},
			Providers: providers("llm"),
			Profiles:  profilesIn(def, map[string][]string{store.DefaultWorkspace: {}}),
			Templates: []store.Template{{Workspace: store.DefaultWorkspace, Name: "pi", Image: "localhost/pi-agent:2026-09-15", CPU: "2", Memory: "4Gi"}},
		},
		{
			Workspaces: []string{store.DefaultWorkspace, "team"}, VM: "dev", State: store.GatewayReady, Version: "0.1.2", Seen: now,
			Sandboxes: []store.Sandbox{
				sandbox("kit-dev", "Ready", pi, "pi", 26*time.Hour, defaultPolicy, 1, "llm"),
				sandbox("tools", "Stopped", claude, "claude", 17*24*time.Hour, toolsPolicy, 2, "claude-code", "gitlab-work"),
				sandbox("webapp", "Ready", pi, "pi", 9*24*time.Hour, defaultPolicy, 4, "llm", "github"),
				// The same name as a sandbox of the default workspace.
				sandboxIn("team", "webapp", "Ready", pi, "pi", 3*24*time.Hour, defaultPolicy, 2, "llm"),
			},
			Providers: append(providers("claude-code", "github", "gitlab-work", "llm"), providersIn("team", "github", "llm")...),
			Profiles:  profilesIn([]string{store.DefaultWorkspace, "team"}, map[string][]string{"team": {"github"}}),
			Templates: []store.Template{
				{Workspace: store.DefaultWorkspace, Name: "claude", Image: claude, CPU: "2", Memory: "4Gi"},
				{Workspace: store.DefaultWorkspace, Name: "pi", Image: pi, CPU: "2", Memory: "4Gi"},
				{Workspace: "team", Name: "pi", Image: pi, CPU: "4", Memory: "8Gi"},
			},
			Config: withTeam(config("claude-code", "github", "gitlab-work", "llm")),
		},
		{
			Workspaces: []string{store.DefaultWorkspace}, VM: "review", State: store.GatewayReady, Version: "0.1.2", Seen: now,
			Sandboxes: []store.Sandbox{
				sandbox("pr-review", "Ready", claude, "claude", 4*24*time.Hour, defaultPolicy, 1, "claude-code", "github"),
			},
			Providers: providers("claude-code", "github", "llm"),
			Profiles:  profilesIn(def, nil),
			Templates: []store.Template{{Workspace: store.DefaultWorkspace, Name: "claude", Image: claude, CPU: "2", Memory: "4Gi"}},
			Config:    config("claude-code", "github", "gitlab-work", "llm"),
		},
	}
}

// followVMs makes the gateways follow their VMs, as the gateway collector
// would: a running VM's gateway answers, a stopped one's keeps what it last
// held, and a deleted VM's goes.
func followVMs(st *store.Store, vms []store.VM) {
	names := map[string]bool{}
	for _, v := range vms {
		names[v.Name] = true
		st.UpdateGateway(v.Name, func(gw *store.Gateway) bool {
			switch {
			case v.Running() && gw.State != store.GatewayReady:
				gw.State, gw.Version, gw.Seen = store.GatewayReady, v.OpenShell, time.Now()
			case !v.Running() && gw.State != store.GatewayStopped:
				gw.State, gw.Seen = store.GatewayStopped, time.Now()
			default:
				return false
			}
			return true
		})
	}
	st.RemoveGateways(func(vm string) bool { return names[vm] })
	st.RemoveTraffic(func(vm string) bool { return names[vm] })
}
