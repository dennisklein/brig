// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

package osclient

import (
	"context"
	"errors"
	"fmt"
	"time"

	pb "github.com/NVIDIA/OpenShell/sdk/go/proto/openshellv1"
	sb "github.com/NVIDIA/OpenShell/sdk/go/proto/sandboxv1"
)

// NetworkRule is one endpoint that programs of a sandbox may reach, as
// AddRule adds it under a rule's key: what openshell policy update builds
// from --rule-name, --binary and one --add-endpoint.
type NetworkRule struct {
	// Key is the rule's key under network_policies.
	Key  string
	Host string
	Port int
	// Protocol is "rest" to inspect HTTP requests, or empty for the whole
	// connection.
	Protocol string
	// Access and Enforcement apply with "rest", as policy files spell them:
	// read-only, read-write or full; enforce or audit.
	Access, Enforcement string
	// Binaries are the full paths of the programs that may connect.
	Binaries []string
}

// PolicyChange is the revision that a change to a sandbox's policy made.
type PolicyChange struct {
	Version uint32
	Hash    string
	// Unchanged says that the sandbox had the rule already, so no
	// revision was made.
	Unchanged bool
}

// ErrGlobalPolicy refuses to change the policy of a sandbox whose gateway
// applies one global policy to every sandbox.
var ErrGlobalPolicy = errors.New("the gateway applies a global policy to every sandbox; delete it before changing one sandbox's policy")

var (
	accessPresets = map[string]sb.NetworkAccessPreset{
		"read-only":  sb.NetworkAccessPreset_NETWORK_ACCESS_PRESET_READ_ONLY,
		"read-write": sb.NetworkAccessPreset_NETWORK_ACCESS_PRESET_READ_WRITE,
		"full":       sb.NetworkAccessPreset_NETWORK_ACCESS_PRESET_FULL,
	}
	enforcementModes = map[string]sb.NetworkEnforcementMode{
		"enforce": sb.NetworkEnforcementMode_NETWORK_ENFORCEMENT_MODE_ENFORCE,
		"audit":   sb.NetworkEnforcementMode_NETWORK_ENFORCEMENT_MODE_AUDIT,
	}
)

// proto builds the rule as the gateway takes it.
func (r NetworkRule) proto() (*sb.NetworkPolicyRule, error) {
	if r.Key == "" || r.Host == "" || r.Port < 1 || r.Port > 65535 || len(r.Binaries) == 0 {
		return nil, errors.New("a network rule needs a key, a host, a port and a program")
	}
	ep := &sb.NetworkEndpoint{Host: r.Host, Port: uint32(r.Port), Ports: []uint32{uint32(r.Port)}, Protocol: r.Protocol}
	switch r.Protocol {
	case "rest":
		var ok bool
		if ep.Access, ok = accessPresets[r.Access]; !ok {
			return nil, fmt.Errorf("unknown access level %q", r.Access)
		}
		if ep.Enforcement, ok = enforcementModes[r.Enforcement]; !ok {
			return nil, fmt.Errorf("unknown enforcement %q", r.Enforcement)
		}
	case "":
	default:
		return nil, fmt.Errorf("unknown protocol %q", r.Protocol)
	}
	rule := &sb.NetworkPolicyRule{Name: r.Key, Endpoints: []*sb.NetworkEndpoint{ep}}
	for _, b := range r.Binaries {
		rule.Binaries = append(rule.Binaries, &sb.NetworkBinary{Path: b})
	}
	return rule, nil
}

// AddRule merges the rule into the policy of a sandbox, as openshell policy
// update --add-endpoint does: an endpoint is added to a rule of the same key,
// and a rule without one is created. The gateway keeps the annotations with
// the revision it makes, as its provenance, which no later change alters.
func (c *Client) AddRule(ctx context.Context, workspace, sandbox string, r NetworkRule, annotations map[string]string) (PolicyChange, error) {
	rule, err := r.proto()
	if err != nil {
		return PolicyChange{}, err
	}
	cur, err := call(ctx, func(ctx context.Context) (*sb.GetSandboxConfigResponse, error) {
		return c.api.GetSandboxConfig(ctx, &sb.GetSandboxConfigRequest{WorkspaceScope: scope(workspace), Name: sandbox})
	})
	if err != nil {
		return PolicyChange{}, err
	}
	if cur.GetPolicySource() == sb.PolicySource_POLICY_SOURCE_GLOBAL {
		return PolicyChange{}, ErrGlobalPolicy
	}
	resp, err := call(ctx, func(ctx context.Context) (*pb.UpdateConfigResponse, error) {
		return c.api.UpdateConfig(ctx, &pb.UpdateConfigRequest{
			WorkspaceScope: scope(workspace),
			Sandbox:        sandbox,
			MergeOperations: []*pb.PolicyMergeOperation{{
				Operation: &pb.PolicyMergeOperation_AddRule{AddRule: &pb.AddNetworkRule{RuleName: r.Key, Rule: rule}},
			}},
			Annotations: annotations,
		})
	})
	if err != nil {
		return PolicyChange{}, err
	}
	return PolicyChange{
		Version: resp.GetVersion(), Hash: resp.GetPolicyHash(),
		Unchanged: resp.GetVersion() == cur.GetVersion() && resp.GetPolicyHash() == cur.GetPolicyHash(),
	}, nil
}

// The outcomes of a policy revision that WaitLoaded waits for.
const (
	// PolicyLoaded: the sandbox enforces the revision.
	PolicyLoaded = "loaded"
	// PolicySuperseded: a newer revision came before the sandbox loaded
	// this one.
	PolicySuperseded = "superseded"
)

// PolicyStatuses names the load states of policy revisions.
var PolicyStatuses = map[pb.PolicyStatus]string{
	pb.PolicyStatus_POLICY_STATUS_PENDING:    "pending",
	pb.PolicyStatus_POLICY_STATUS_LOADED:     PolicyLoaded,
	pb.PolicyStatus_POLICY_STATUS_FAILED:     "failed",
	pb.PolicyStatus_POLICY_STATUS_SUPERSEDED: PolicySuperseded,
}

// waitEvery is how often WaitLoaded asks for the revision's state.
var waitEvery = time.Second

// WaitLoaded waits until the sandbox loaded the policy revision version, or
// a newer one superseded it, and returns which, with the version the sandbox
// enforces. A revision that fails to load is an error with the sandbox's
// reason; ctx bounds the wait.
func (c *Client) WaitLoaded(ctx context.Context, workspace, sandbox string, version uint32) (string, uint32, error) {
	for {
		select {
		case <-ctx.Done():
			return "", 0, fmt.Errorf("waiting for sandbox %s to load policy version %d: %w", sandbox, version, ctx.Err())
		case <-time.After(waitEvery):
		}
		r, err := call(ctx, func(ctx context.Context) (*pb.GetSandboxPolicyStatusResponse, error) {
			return c.api.GetSandboxPolicyStatus(ctx, &pb.GetSandboxPolicyStatusRequest{WorkspaceScope: scope(workspace), Sandbox: sandbox, Version: version})
		})
		if err != nil {
			return "", 0, err
		}
		switch r.GetRevision().GetStatus() {
		case pb.PolicyStatus_POLICY_STATUS_LOADED:
			return PolicyLoaded, r.GetActiveVersion(), nil
		case pb.PolicyStatus_POLICY_STATUS_SUPERSEDED:
			return PolicySuperseded, r.GetActiveVersion(), nil
		case pb.PolicyStatus_POLICY_STATUS_FAILED:
			return "", r.GetActiveVersion(), fmt.Errorf("sandbox %s could not load policy version %d and keeps version %d: %s",
				sandbox, version, r.GetActiveVersion(), r.GetRevision().GetLoadError())
		}
	}
}
