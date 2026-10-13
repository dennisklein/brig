// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

package osclient

import (
	"context"
	"errors"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	pb "github.com/NVIDIA/OpenShell/sdk/go/proto/openshellv1"
	sb "github.com/NVIDIA/OpenShell/sdk/go/proto/sandboxv1"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// policyGateway is the fake gateway with a sandbox policy that can be
// changed: each UpdateConfig makes version 4, which loads, or not, as
// status says.
type policyGateway struct {
	gateway
	global bool
	status pb.PolicyStatus
	mu     sync.Mutex
	update *pb.UpdateConfigRequest
	asked  int
}

func (g *policyGateway) GetSandboxConfig(ctx context.Context, r *sb.GetSandboxConfigRequest) (*sb.GetSandboxConfigResponse, error) {
	resp, err := g.gateway.GetSandboxConfig(ctx, r)
	if err == nil {
		resp.PolicyHash = "h3"
		if g.global {
			resp.PolicySource = sb.PolicySource_POLICY_SOURCE_GLOBAL
		}
	}
	return resp, err
}

func (g *policyGateway) UpdateConfig(_ context.Context, r *pb.UpdateConfigRequest) (*pb.UpdateConfigResponse, error) {
	if err := checkScope(r.GetWorkspaceScope()); err != nil {
		return nil, err
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	g.update = r
	if r.GetMergeOperations()[0].GetAddRule().GetRuleName() == "same" {
		return &pb.UpdateConfigResponse{Version: 3, PolicyHash: "h3"}, nil
	}
	return &pb.UpdateConfigResponse{Version: 4, PolicyHash: "h4", Annotations: r.GetAnnotations()}, nil
}

func (g *policyGateway) GetSandboxPolicyStatus(_ context.Context, r *pb.GetSandboxPolicyStatusRequest) (*pb.GetSandboxPolicyStatusResponse, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.asked++
	st := pb.PolicyStatus_POLICY_STATUS_PENDING
	if g.asked > 1 {
		st = g.status
	}
	active := uint32(3)
	if st == pb.PolicyStatus_POLICY_STATUS_LOADED {
		active = 4
	}
	return &pb.GetSandboxPolicyStatusResponse{
		Revision:      &pb.SandboxPolicyRevision{Version: r.GetVersion(), Status: st, LoadError: "bad rule"},
		ActiveVersion: active,
	}, nil
}

func TestAddRule(t *testing.T) {
	waitEvery = time.Millisecond
	g := &policyGateway{status: pb.PolicyStatus_POLICY_STATUS_LOADED}
	c := serve(t, g)
	ctx := context.Background()
	rule := NetworkRule{Key: "pip", Host: "pypi.org", Port: 443, Protocol: "rest", Access: "read-only", Enforcement: "audit", Binaries: []string{"/usr/bin/pip"}}
	ann := map[string]string{"brig.dev/by": "policy-allow"}
	ch, err := c.AddRule(ctx, "team", "webapp", rule, ann)
	if err != nil || ch.Version != 4 || ch.Hash != "h4" || ch.Unchanged {
		t.Fatalf("AddRule = %+v, %v", ch, err)
	}
	want := &pb.UpdateConfigRequest{
		WorkspaceScope: scope("team"), Sandbox: "webapp", Annotations: ann,
		MergeOperations: []*pb.PolicyMergeOperation{{Operation: &pb.PolicyMergeOperation_AddRule{AddRule: &pb.AddNetworkRule{
			RuleName: "pip",
			Rule: &sb.NetworkPolicyRule{
				Name: "pip",
				Endpoints: []*sb.NetworkEndpoint{{
					Host: "pypi.org", Port: 443, Ports: []uint32{443}, Protocol: "rest",
					Access:      sb.NetworkAccessPreset_NETWORK_ACCESS_PRESET_READ_ONLY,
					Enforcement: sb.NetworkEnforcementMode_NETWORK_ENFORCEMENT_MODE_AUDIT,
				}},
				Binaries: []*sb.NetworkBinary{{Path: "/usr/bin/pip"}},
			},
		}}}},
	}
	if !proto.Equal(g.update, want) {
		t.Errorf("UpdateConfig got\n%v\nwant\n%v", g.update, want)
	}
	how, active, err := c.WaitLoaded(ctx, "team", "webapp", ch.Version)
	if err != nil || how != PolicyLoaded || active != 4 {
		t.Errorf("WaitLoaded = %q, %d, %v", how, active, err)
	}

	// A rule the sandbox has already makes no revision.
	rule.Key = "same"
	if ch, err := c.AddRule(ctx, "default", "webapp", rule, nil); err != nil || !ch.Unchanged {
		t.Errorf("AddRule of a rule it has = %+v, %v", ch, err)
	}
	// The whole connection has no access level.
	rule.Key, rule.Protocol = "l4", ""
	if _, err := c.AddRule(ctx, "team", "webapp", rule, nil); err != nil {
		t.Fatal(err)
	}
	ep := g.update.GetMergeOperations()[0].GetAddRule().GetRule().GetEndpoints()[0]
	if ep.GetProtocol() != "" || ep.GetAccess() != 0 || ep.GetEnforcement() != 0 {
		t.Errorf("whole-connection endpoint: %v", ep)
	}
}

func TestAddRuleRefuses(t *testing.T) {
	c := serve(t, &policyGateway{global: true})
	rule := NetworkRule{Key: "pip", Host: "pypi.org", Port: 443, Binaries: []string{"/usr/bin/pip"}}
	if _, err := c.AddRule(context.Background(), "default", "webapp", rule, nil); !errors.Is(err, ErrGlobalPolicy) {
		t.Errorf("global policy: %v", err)
	}
	for _, bad := range []NetworkRule{
		{Host: "pypi.org", Port: 443, Binaries: []string{"/b"}},
		{Key: "k", Host: "pypi.org", Port: 443},
		{Key: "k", Host: "pypi.org", Port: 443, Binaries: []string{"/b"}, Protocol: "rest", Access: "some"},
		{Key: "k", Host: "pypi.org", Port: 443, Binaries: []string{"/b"}, Protocol: "sql"},
	} {
		if _, err := bad.proto(); err == nil {
			t.Errorf("%+v: no error", bad)
		}
	}
}

func TestWaitLoadedFails(t *testing.T) {
	waitEvery = time.Millisecond
	c := serve(t, &policyGateway{status: pb.PolicyStatus_POLICY_STATUS_FAILED})
	_, active, err := c.WaitLoaded(context.Background(), "default", "webapp", 4)
	if err == nil || !strings.Contains(err.Error(), "bad rule") || active != 3 {
		t.Errorf("WaitLoaded = %d, %v", active, err)
	}
	c = serve(t, &policyGateway{status: pb.PolicyStatus_POLICY_STATUS_PENDING})
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, _, err := c.WaitLoaded(ctx, "default", "webapp", 4); err == nil || !strings.Contains(err.Error(), "deadline") {
		t.Errorf("WaitLoaded of a pending revision = %v", err)
	}
}

// revisionGateway lists seven revisions of webapp's policy, newest first,
// in pages of two.
type revisionGateway struct{ gateway }

func (g *revisionGateway) ListSandboxPolicies(_ context.Context, r *pb.ListSandboxPoliciesRequest) (*pb.ListSandboxPoliciesResponse, error) {
	if err := checkScope(r.GetWorkspaceScope()); err != nil {
		return nil, err
	}
	var all []*pb.SandboxPolicyRevision
	for v := uint32(7); v >= 1; v-- {
		rev := &pb.SandboxPolicyRevision{
			Version: v, PolicyHash: "h" + strconv.Itoa(int(v)), Status: pb.PolicyStatus_POLICY_STATUS_SUPERSEDED,
			CreatedTime: timestamppb.New(created.Add(time.Duration(v) * time.Minute)), Policy: testPolicy(),
		}
		if v == 7 {
			rev.Status = pb.PolicyStatus_POLICY_STATUS_LOADED
			rev.Provenance = map[string]string{"brig.dev/by": "policy-allow"}
		}
		all = append(all, rev)
	}
	items, next := page(all, r.GetPageToken(), 2)
	return &pb.ListSandboxPoliciesResponse{Revisions: items, NextPageToken: next}, nil
}

func TestRevisions(t *testing.T) {
	c := serve(t, &revisionGateway{})
	revs, err := c.Revisions(context.Background(), "default", "webapp", 4)
	if err != nil {
		t.Fatal(err)
	}
	var versions []uint32
	for _, r := range revs {
		versions = append(versions, r.Version)
	}
	if !slices.Equal(versions, []uint32{5, 6, 7}) {
		t.Fatalf("versions %v", versions)
	}
	last := revs[2]
	if last.Status != PolicyLoaded || last.Hash != "h7" || last.Provenance["brig.dev/by"] != "policy-allow" ||
		!last.Created.Equal(created.Add(7*time.Minute)) || !strings.Contains(last.Policy, "network_policies:") || last.Err != "" {
		t.Errorf("revision 7: %+v", last)
	}
	if revs[0].Status != PolicySuperseded {
		t.Errorf("revision 5: %+v", revs[0])
	}
}
