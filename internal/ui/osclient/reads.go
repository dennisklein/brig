// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

package osclient

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"

	pb "github.com/NVIDIA/OpenShell/sdk/go/proto/openshellv1"
	sb "github.com/NVIDIA/OpenShell/sdk/go/proto/sandboxv1"

	"github.com/dennisklein/brig/internal/ui/store"
)

// phases names sandbox phases as the openshell CLI does.
var phases = map[pb.SandboxPhase]string{
	pb.SandboxPhase_SANDBOX_PHASE_PROVISIONING: "Provisioning",
	pb.SandboxPhase_SANDBOX_PHASE_READY:        "Ready",
	pb.SandboxPhase_SANDBOX_PHASE_ERROR:        "Error",
	pb.SandboxPhase_SANDBOX_PHASE_DELETING:     "Deleting",
	pb.SandboxPhase_SANDBOX_PHASE_STOPPING:     "Stopping",
	pb.SandboxPhase_SANDBOX_PHASE_STOPPED:      "Stopped",
	pb.SandboxPhase_SANDBOX_PHASE_STARTING:     "Starting",
	pb.SandboxPhase_SANDBOX_PHASE_COMPLETED:    "Completed",
}

// Sandboxes lists the gateway's sandboxes, sorted by name, without their
// policies.
func (c *Client) Sandboxes(ctx context.Context) ([]store.Sandbox, error) {
	list, err := pages(ctx, func(ctx context.Context, token string) ([]*pb.Sandbox, string, error) {
		r, err := c.api.ListSandboxes(ctx, &pb.ListSandboxesRequest{WorkspaceScope: scope(), PageSize: pageSize, PageToken: token})
		return r.GetSandboxes(), r.GetNextPageToken(), err
	})
	if err != nil {
		return nil, err
	}
	out := make([]store.Sandbox, 0, len(list))
	for _, s := range list {
		out = append(out, sandbox(s))
	}
	slices.SortFunc(out, func(a, b store.Sandbox) int { return strings.Compare(a.Name, b.Name) })
	return out, nil
}

func sandbox(s *pb.Sandbox) store.Sandbox {
	phase, ok := phases[s.GetStatus().GetPhase()]
	if !ok {
		phase = "Unknown"
	}
	out := store.Sandbox{
		Name:          s.GetMetadata().GetName(),
		Phase:         phase,
		Image:         s.GetSpec().GetTemplate().GetImage(),
		Template:      s.GetCreatedFromWorkloadTemplate().GetName(),
		Providers:     slices.Clone(s.GetSpec().GetProviders()),
		PolicyVersion: s.GetStatus().GetCurrentPolicyVersion(),
	}
	if t := s.GetMetadata().GetCreatedTime(); t != nil {
		out.Created = t.AsTime()
	}
	slices.Sort(out.Providers)
	for _, c := range s.GetStatus().GetConditions() {
		if c.GetStatus() == "False" && c.GetMessage() != "" {
			out.Problem = c.GetMessage()
			break
		}
	}
	return out
}

// Providers lists the gateway's providers, sorted by name. They carry the
// names of their credentials only: the values never leave this function,
// although the gateway sends only placeholders for them anyway.
func (c *Client) Providers(ctx context.Context) ([]store.Provider, error) {
	list, err := pages(ctx, func(ctx context.Context, token string) ([]store.Provider, string, error) {
		r, err := c.api.ListProviders(ctx, &pb.ListProvidersRequest{WorkspaceScope: scope(), PageSize: pageSize, PageToken: token})
		out := make([]store.Provider, 0, len(r.GetProviders()))
		for _, p := range r.GetProviders() {
			out = append(out, store.Provider{
				Name:        p.GetMetadata().GetName(),
				Type:        p.GetType(),
				Credentials: slices.Sorted(maps.Keys(p.GetCredentials())),
			})
		}
		return out, r.GetNextPageToken(), err
	})
	if err != nil {
		return nil, err
	}
	slices.SortFunc(list, func(a, b store.Provider) int { return strings.Compare(a.Name, b.Name) })
	return list, nil
}

// categories names profile categories.
var categories = map[pb.ProviderProfileCategory]string{
	pb.ProviderProfileCategory_PROVIDER_PROFILE_CATEGORY_OTHER:          "other",
	pb.ProviderProfileCategory_PROVIDER_PROFILE_CATEGORY_INFERENCE:      "inference",
	pb.ProviderProfileCategory_PROVIDER_PROFILE_CATEGORY_AGENT:          "agent",
	pb.ProviderProfileCategory_PROVIDER_PROFILE_CATEGORY_SOURCE_CONTROL: "source control",
	pb.ProviderProfileCategory_PROVIDER_PROFILE_CATEGORY_MESSAGING:      "messaging",
	pb.ProviderProfileCategory_PROVIDER_PROFILE_CATEGORY_DATA:           "data",
	pb.ProviderProfileCategory_PROVIDER_PROFILE_CATEGORY_KNOWLEDGE:      "knowledge",
}

// Profiles lists the provider profiles that brig's workspace sees, its own
// and the platform's, sorted by ID.
func (c *Client) Profiles(ctx context.Context) ([]store.Profile, error) {
	list, err := pages(ctx, func(ctx context.Context, token string) ([]*pb.ProviderProfile, string, error) {
		r, err := c.api.ListProviderProfiles(ctx, &pb.ListProviderProfilesRequest{WorkspaceScope: scope(), PageSize: pageSize, PageToken: token})
		return r.GetProfiles(), r.GetNextPageToken(), err
	})
	if err != nil {
		return nil, err
	}
	out := make([]store.Profile, 0, len(list))
	for _, p := range list {
		out = append(out, profile(p))
	}
	slices.SortFunc(out, func(a, b store.Profile) int { return strings.Compare(a.ID, b.ID) })
	return out, nil
}

func profile(p *pb.ProviderProfile) store.Profile {
	out := store.Profile{
		ID: p.GetId(), Name: p.GetDisplayName(), Description: p.GetDescription(),
		Category: categories[p.GetCategory()], Source: p.GetSource(), Scope: p.GetScope(),
	}
	for _, c := range p.GetCredentials() {
		auth := c.GetAuthStyle()
		switch {
		case c.GetHeaderName() != "":
			auth = strings.TrimSpace(auth + " " + c.GetHeaderName())
		case c.GetQueryParam() != "":
			auth = strings.TrimSpace(auth + " " + c.GetQueryParam())
		}
		out.Credentials = append(out.Credentials, store.ProfileCredential{
			Name: c.GetName(), EnvVars: slices.Clone(c.GetEnvVars()), Auth: auth, Required: c.GetRequired(),
		})
	}
	for _, e := range p.GetEndpoints() {
		out.Endpoints = append(out.Endpoints, endpoints(e)...)
	}
	for _, b := range p.GetBinaries() {
		out.Binaries = append(out.Binaries, b.GetPath())
	}
	return out
}

// endpoints writes an endpoint as host:port, once per port.
func endpoints(e *sb.NetworkEndpoint) []string {
	ports := e.GetPorts()
	if len(ports) == 0 && e.GetPort() != 0 {
		ports = []uint32{e.GetPort()}
	}
	if len(ports) == 0 {
		return []string{e.GetHost()}
	}
	out := make([]string, len(ports))
	for i, p := range ports {
		out[i] = fmt.Sprintf("%s:%d", e.GetHost(), p)
	}
	return out
}

// Templates lists the gateway's sandbox templates, sorted by name.
func (c *Client) Templates(ctx context.Context) ([]store.Template, error) {
	list, err := pages(ctx, func(ctx context.Context, token string) ([]*pb.SandboxWorkloadTemplate, string, error) {
		r, err := c.api.ListSandboxTemplates(ctx, &pb.ListSandboxTemplatesRequest{WorkspaceScope: scope(), PageSize: pageSize, PageToken: token})
		return r.GetTemplates(), r.GetNextPageToken(), err
	})
	if err != nil {
		return nil, err
	}
	out := make([]store.Template, 0, len(list))
	for _, t := range list {
		w := t.GetSpec().GetWorkload()
		out = append(out, store.Template{
			Name: t.GetMetadata().GetName(), Image: w.GetImage(),
			CPU: w.GetResources().GetCpu(), Memory: w.GetResources().GetMemory(),
		})
	}
	slices.SortFunc(out, func(a, b store.Template) int { return strings.Compare(a.Name, b.Name) })
	return out, nil
}

// Policy reads the policy a sandbox enforces, as openshell policy get does.
// A policy it cannot show comes back with Err set rather than as an error,
// which is left for a failed call.
func (c *Client) Policy(ctx context.Context, sandbox string) (*store.Policy, error) {
	r, err := call(ctx, func(ctx context.Context) (*sb.GetSandboxConfigResponse, error) {
		return c.api.GetSandboxConfig(ctx, &sb.GetSandboxConfigRequest{WorkspaceScope: scope(), Name: sandbox})
	})
	if err != nil {
		return nil, err
	}
	p := &store.Policy{Version: r.GetVersion(), Global: r.GetPolicySource() == sb.PolicySource_POLICY_SOURCE_GLOBAL}
	if p.Global && r.GetGlobalPolicyVersion() > 0 {
		p.Version = r.GetGlobalPolicyVersion()
	}
	if r.GetPolicy() == nil {
		p.Err = "the sandbox has no policy"
		return p, nil
	}
	doc, err := policyDoc(r.GetPolicy())
	if err != nil {
		p.Err = err.Error()
		return p, nil
	}
	if p.Effective, err = doc.yaml(); err != nil {
		p.Err = err.Error()
		return p, nil
	}
	p.ProviderRules = doc.providerRules()
	if p.Base, err = doc.base().yaml(); err != nil {
		p.Err = err.Error()
	}
	return p, nil
}
