// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"slices"
	"strings"
	"time"
)

// TopicGateways is the topic of the snapshot's OpenShell gateways.
const TopicGateways Topic = "gateways"

// The states of a Gateway.
const (
	// GatewayConnecting: the VM runs and brig ui has not reached its
	// gateway yet.
	GatewayConnecting = "connecting"
	// GatewayReady: the gateway answers.
	GatewayReady = "ready"
	// GatewayUnreachable: the last attempt to reach the gateway failed.
	GatewayUnreachable = "unreachable"
	// GatewayStopped: the VM does not run. The gateway's parts are what it
	// held when it last answered.
	GatewayStopped = "stopped"
)

// Gateway is the OpenShell gateway of a VM: what brig ui read from it over
// gRPC, and the VM's OpenShell config directories as brig reads them.
type Gateway struct {
	// VM is the name of the VM that runs the gateway.
	VM string
	// State is one of the Gateway states.
	State string
	// Err says why the gateway is unreachable.
	Err string
	// Version is the gateway's OpenShell version.
	Version string
	// Seen is when the gateway last answered; zero if it never did.
	Seen time.Time
	// Sandboxes are sorted by name.
	Sandboxes []Sandbox
	// Providers are sorted by name.
	Providers []Provider
	// Profiles are the provider profiles the gateway offers, sorted by ID.
	Profiles []Profile
	// Templates are the sandbox templates, sorted by name.
	Templates []Template
	// Config is what the VM's config directories define.
	Config GatewayConfig
}

// Name is the gateway's name on the host, such as brig-dev.
func (g Gateway) Name() string { return "brig-" + g.VM }

// Answered reports whether the gateway answered at least once, so that its
// parts are worth showing.
func (g Gateway) Answered() bool { return !g.Seen.IsZero() }

// Sandbox returns the named sandbox of the gateway.
func (g Gateway) Sandbox(name string) (Sandbox, bool) {
	for _, s := range g.Sandboxes {
		if s.Name == name {
			return s, true
		}
	}
	return Sandbox{}, false
}

// Provider returns the named provider of the gateway.
func (g Gateway) Provider(name string) (Provider, bool) {
	for _, p := range g.Providers {
		if p.Name == name {
			return p, true
		}
	}
	return Provider{}, false
}

// Profile returns the provider profile with the ID id.
func (g Gateway) Profile(id string) (Profile, bool) {
	for _, p := range g.Profiles {
		if p.ID == id {
			return p, true
		}
	}
	return Profile{}, false
}

// Sandbox is an OpenShell sandbox.
type Sandbox struct {
	Name string
	// Phase is the sandbox's phase as the openshell CLI names it, such as
	// Ready, Provisioning or Stopped.
	Phase string
	Image string
	// Template is the sandbox template it was created from, if any.
	Template string
	// Providers are the names of the attached providers.
	Providers []string
	Created   time.Time
	// Problem is the message of a condition that is not met, if any.
	Problem string
	// PolicyVersion is the version of the policy the sandbox loaded.
	PolicyVersion uint32
	// Policy is the sandbox's policy, nil until read.
	Policy *Policy
}

// Ready reports whether the sandbox runs and can be connected to.
func (s Sandbox) Ready() bool { return s.Phase == "Ready" }

// Policy is a sandbox's policy, as YAML in the format of policy files.
type Policy struct {
	// Effective is the policy the sandbox enforces: its base policy and
	// the rules of the attached providers.
	Effective string
	// Base is the effective policy without the providers' rules.
	Base string
	// ProviderRules are the keys of the providers' rules under
	// network_policies.
	ProviderRules []string
	// Version numbers the policy's revisions.
	Version uint32
	// Global says that a global policy of the gateway overrides the
	// sandbox's own.
	Global bool
	// Err says why the policy could not be read or shown.
	Err string
}

// Provider is an OpenShell provider. It holds the names of its credentials
// only, never their values.
type Provider struct {
	Name string
	// Type is the ID of the provider profile it was created from.
	Type string
	// Credentials are the names of its credentials.
	Credentials []string
}

// Profile is an OpenShell provider profile.
type Profile struct {
	ID          string
	Name        string
	Description string
	Category    string
	// Source is where the profile comes from: builtin, user, or an
	// interceptor.
	Source string
	// Scope is platform or workspace, or empty.
	Scope       string
	Credentials []ProfileCredential
	// Endpoints are host:port pairs the profile's credentials may go to.
	Endpoints []string
	// Binaries are the programs that may use them.
	Binaries []string
}

// ProfileCredential is a credential that a provider of a profile holds.
type ProfileCredential struct {
	Name    string
	EnvVars []string
	// Auth says how the proxy sends it, such as "bearer" or "header
	// X-Api-Key".
	Auth     string
	Required bool
}

// Template is a sandbox template.
type Template struct {
	Name   string
	Image  string
	CPU    string
	Memory string
}

// GatewayConfig is what a VM's OpenShell config directories define, as
// brig sync applies it.
type GatewayConfig struct {
	Dirs []string
	// Providers maps provider names to the files that define them.
	Providers map[string]string
	// Profiles maps profile IDs to the files that define them.
	Profiles map[string]string
	// DefaultPolicy is the default sandbox policy's file, if any, and
	// DefaultPolicyText its content.
	DefaultPolicy     string
	DefaultPolicyText string
	// Policies are the files of policies/.
	Policies []string
	// Err says why the directories could not be read.
	Err string
}

// UpdateGateway changes the gateway of the VM named vm with fn, adding an
// empty one first if there is none. When fn reports no change, nothing is
// published.
func (s *Store) UpdateGateway(vm string, fn func(*Gateway) bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	gws := slices.Clone(s.snap.Gateways)
	i, found := slices.BinarySearchFunc(gws, vm, func(g Gateway, name string) int { return strings.Compare(g.VM, name) })
	if !found {
		gws = slices.Insert(gws, i, Gateway{VM: vm, State: GatewayStopped})
	}
	if !fn(&gws[i]) && found {
		return
	}
	s.snap.Gateways = gws
	s.notify(TopicGateways)
}

// RemoveGateways drops the gateways of the VMs that keep does not hold.
func (s *Store) RemoveGateways(keep func(vm string) bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	gws := slices.DeleteFunc(slices.Clone(s.snap.Gateways), func(g Gateway) bool { return !keep(g.VM) })
	if len(gws) == len(s.snap.Gateways) {
		return
	}
	s.snap.Gateways = gws
	s.notify(TopicGateways)
}

// Gateway returns the gateway of the VM named vm.
func (sn Snapshot) Gateway(vm string) (Gateway, bool) {
	for _, g := range sn.Gateways {
		if g.VM == vm {
			return g, true
		}
	}
	return Gateway{}, false
}

// SandboxCount returns how many sandboxes the gateways hold, and how many of
// them are ready.
func (sn Snapshot) SandboxCount() (ready, total int) {
	for _, g := range sn.Gateways {
		for _, s := range g.Sandboxes {
			total++
			if s.Ready() {
				ready++
			}
		}
	}
	return ready, total
}
