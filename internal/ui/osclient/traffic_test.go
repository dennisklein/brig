// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

package osclient

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	pb "github.com/NVIDIA/OpenShell/sdk/go/proto/openshellv1"
	sb "github.com/NVIDIA/OpenShell/sdk/go/proto/sandboxv1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/dennisklein/brig/internal/ui/store"
)

var logTime = time.Date(2026, 10, 12, 9, 30, 0, 0, time.UTC)

// WatchSandbox sends two lines and a warning, after a status snapshot as
// OpenShell does, or refuses a cursor it does not know.
func (g *gateway) WatchSandbox(r *pb.WatchSandboxRequest, s grpc.ServerStreamingServer[pb.SandboxStreamEvent]) error {
	if err := checkScope(r.GetWorkspaceScope()); err != nil {
		return err
	}
	g.mu.Lock()
	g.watches = append(g.watches, r)
	g.mu.Unlock()
	if r.GetResumeAfterCursor() == "gone" {
		return status.Error(codes.OutOfRange, "cursor trimmed")
	}
	events := []*pb.SandboxStreamEvent{
		{Payload: &pb.SandboxStreamEvent_Sandbox{Sandbox: &pb.Sandbox{}}},
		{Cursor: "c1", Payload: &pb.SandboxStreamEvent_Log{Log: &pb.SandboxLogLine{
			EventTime: timestamppb.New(logTime), Level: "OCSF", Source: "sandbox",
			Message: "NET:OPEN [MED] DENIED /usr/bin/node(7) -> registry.npmjs.org:443",
		}}},
		{Payload: &pb.SandboxStreamEvent_Warning{Warning: &pb.SandboxStreamWarning{Message: "dropped 3 events"}}},
		{Cursor: "c2", Payload: &pb.SandboxStreamEvent_Log{Log: &pb.SandboxLogLine{Level: "INFO", Message: "policy reloaded"}}},
	}
	for _, e := range events {
		if err := s.Send(e); err != nil {
			return err
		}
	}
	return nil
}

func (g *gateway) GetDraftPolicy(_ context.Context, r *pb.GetDraftPolicyRequest) (*pb.GetDraftPolicyResponse, error) {
	if err := checkScope(r.GetWorkspaceScope()); err != nil {
		return nil, err
	}
	if r.GetStatusFilter() != "pending" || r.GetSandbox() != "webapp" {
		return nil, status.Error(codes.InvalidArgument, "unexpected request")
	}
	return &pb.GetDraftPolicyResponse{Chunks: []*pb.PolicyChunk{
		{
			Id: "ch-1", Status: "pending", RuleName: "allow_registry_npmjs_org_443", Binary: "/usr/bin/node",
			Rationale: "node was denied 4 times", ValidationResult: "no new findings", HitCount: 4,
			LastSeenTime: timestamppb.New(logTime), ReviewToken: "not shown",
			ProposedRule: &sb.NetworkPolicyRule{
				Name: "allow_registry_npmjs_org_443",
				Endpoints: []*sb.NetworkEndpoint{{
					Host: "registry.npmjs.org", Port: 443, Ports: []uint32{443},
					Access: sb.NetworkAccessPreset_NETWORK_ACCESS_PRESET_READ_ONLY, Protocol: "rest",
					Enforcement: sb.NetworkEnforcementMode_NETWORK_ENFORCEMENT_MODE_ENFORCE, AdvisorProposed: true,
				}},
				Binaries: []*sb.NetworkBinary{{Path: "/usr/bin/node"}},
			},
		},
		// The gateway may send decided chunks too.
		{Id: "ch-0", Status: "approved", RuleName: "old"},
	}}, nil
}

func TestWatch(t *testing.T) {
	g := &gateway{pageSize: 10}
	c := serve(t, g)
	ctx := context.Background()
	since := logTime.Add(-time.Hour)
	var got []WatchEvent
	err := c.Watch(ctx, "webapp", WatchFrom{Since: since, Tail: 2000}, func(e WatchEvent) { got = append(got, e) })
	if err == nil {
		t.Fatal("a stream that ended reported no error")
	}
	want := []WatchEvent{
		{Cursor: "c1", Line: &LogLine{Time: logTime, Level: "OCSF", Message: "NET:OPEN [MED] DENIED /usr/bin/node(7) -> registry.npmjs.org:443"}},
		{Warning: "dropped 3 events"},
		{Cursor: "c2", Line: &LogLine{Level: "INFO", Message: "policy reloaded"}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("events:\ngot  %+v\nwant %+v", got, want)
	}
	g.mu.Lock()
	r := g.watches[0]
	g.mu.Unlock()
	if r.GetSandbox() != "webapp" || !r.GetFollowLogs() || r.GetLogTailLines() != 2000 ||
		!r.GetSinceTime().AsTime().Equal(since) || !reflect.DeepEqual(r.GetLogSources(), []string{"sandbox"}) {
		t.Errorf("request from the tail: %v", r)
	}

	// Resuming sends only the cursor; one the gateway lost ends the watch
	// with ErrCursorLost.
	err = c.Watch(ctx, "webapp", WatchFrom{Cursor: "gone", Since: since, Tail: 2000}, func(WatchEvent) {})
	if !errors.Is(err, ErrCursorLost) {
		t.Errorf("err = %v, want ErrCursorLost", err)
	}
	g.mu.Lock()
	r = g.watches[1]
	g.mu.Unlock()
	if r.GetResumeAfterCursor() != "gone" || r.GetSinceTime() != nil || r.GetLogTailLines() != 0 {
		t.Errorf("request with a cursor: %v", r)
	}
}

func TestDrafts(t *testing.T) {
	c := serve(t, &gateway{pageSize: 10})
	got, err := c.Drafts(context.Background(), "webapp")
	if err != nil {
		t.Fatal(err)
	}
	want := []store.Draft{{
		Sandbox: "webapp", ID: "ch-1", Rule: "allow_registry_npmjs_org_443", Binary: "/usr/bin/node",
		Endpoints: []store.DraftEndpoint{{Host: "registry.npmjs.org", Port: 443}},
		Binaries:  []string{"/usr/bin/node"},
		YAML: `allow_registry_npmjs_org_443:
  name: allow_registry_npmjs_org_443
  endpoints:
    - host: registry.npmjs.org
      port: 443
      protocol: rest
      enforcement: enforce
      access: read-only
  binaries:
    - path: /usr/bin/node
`,
		Rationale: "node was denied 4 times", Validation: "no new findings", Hits: 4, LastSeen: logTime,
	}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Drafts:\ngot  %+v\nwant %+v", got, want)
	}
}
