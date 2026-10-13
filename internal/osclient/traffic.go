// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

package osclient

import (
	"context"
	"errors"
	"time"

	pb "github.com/NVIDIA/OpenShell/sdk/go/proto/openshellv1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/dennisklein/brig/internal/ui/store"
)

// LogLine is a line of a sandbox's log.
type LogLine struct {
	Time time.Time
	// Level is the line's level; OCSF for the events that ParseRequest
	// reads.
	Level   string
	Message string
}

// WatchFrom says where a watch of a sandbox's log starts.
type WatchFrom struct {
	// Cursor resumes after the event that carried it. Empty starts with
	// the end of the gateway's buffer, as Since and Tail bound it.
	Cursor string
	// Since leaves out lines older than this, unless zero.
	Since time.Time
	// Tail is how many of the buffer's last lines to send first.
	Tail uint32
}

// WatchEvent is what a watch sends: a line, or a warning that the gateway
// dropped lines.
type WatchEvent struct {
	// Cursor is where a watch resumes after this event, empty for a
	// warning.
	Cursor string
	Line   *LogLine
	// Warning says what the gateway dropped.
	Warning string
}

// ErrCursorLost ends a watch that resumed at a cursor the gateway no longer
// has: it restarted, or its buffer moved past it. A watch from the tail
// starts over.
var ErrCursorLost = errors.New("the gateway no longer has the lines after the last one read")

// Watch follows a sandbox's log, calling each for every line its supervisor
// sends and every gap, until the stream or ctx ends. It returns the error
// that ended the stream.
func (c *Client) Watch(ctx context.Context, workspace, sandbox string, from WatchFrom, each func(WatchEvent)) error {
	req := &pb.WatchSandboxRequest{
		WorkspaceScope:    scope(workspace),
		Sandbox:           sandbox,
		FollowLogs:        true,
		LogSources:        []string{"sandbox"},
		ResumeAfterCursor: from.Cursor,
	}
	if from.Cursor == "" {
		req.LogTailLines = from.Tail
		if !from.Since.IsZero() {
			req.SinceTime = timestamppb.New(from.Since)
		}
	}
	stream, err := c.api.WatchSandbox(ctx, req)
	if err != nil {
		return watchErr(err)
	}
	for {
		ev, err := stream.Recv()
		if err != nil {
			return watchErr(err)
		}
		switch p := ev.GetPayload().(type) {
		case *pb.SandboxStreamEvent_Log:
			l := p.Log
			line := &LogLine{Level: l.GetLevel(), Message: l.GetMessage()}
			if t := l.GetEventTime(); t != nil {
				line.Time = t.AsTime()
			}
			each(WatchEvent{Cursor: ev.GetCursor(), Line: line})
		case *pb.SandboxStreamEvent_Warning:
			each(WatchEvent{Warning: p.Warning.GetMessage()})
		}
	}
}

func watchErr(err error) error {
	if status.Code(err) == codes.OutOfRange {
		return ErrCursorLost
	}
	return err
}

// Drafts lists the network rules that OpenShell drafted from a sandbox's
// denied requests and that wait for approval.
func (c *Client) Drafts(ctx context.Context, workspace, sandbox string) ([]store.Draft, error) {
	r, err := call(ctx, func(ctx context.Context) (*pb.GetDraftPolicyResponse, error) {
		return c.api.GetDraftPolicy(ctx, &pb.GetDraftPolicyRequest{WorkspaceScope: scope(workspace), Sandbox: sandbox, StatusFilter: "pending"})
	})
	if err != nil {
		return nil, err
	}
	var out []store.Draft
	for _, ch := range r.GetChunks() {
		if ch.GetStatus() != "pending" {
			continue
		}
		out = append(out, draft(workspace, sandbox, ch))
	}
	return out, nil
}

func draft(workspace, sandbox string, ch *pb.PolicyChunk) store.Draft {
	d := store.Draft{
		Workspace: workspace, Sandbox: sandbox, ID: ch.GetId(), Rule: ch.GetRuleName(), Binary: ch.GetBinary(),
		Rationale: ch.GetRationale(), Validation: ch.GetValidationResult(), Security: ch.GetSecurityNotes(),
		Error: ch.GetApplicationError(), Hits: int(ch.GetHitCount()),
	}
	if t := ch.GetLastSeenTime(); t != nil {
		d.LastSeen = t.AsTime()
	}
	rule := ch.GetProposedRule()
	for _, e := range rule.GetEndpoints() {
		ports := e.GetPorts()
		if len(ports) == 0 && e.GetPort() != 0 {
			ports = []uint32{e.GetPort()}
		}
		if len(ports) == 0 {
			d.Endpoints = append(d.Endpoints, store.DraftEndpoint{Host: e.GetHost()})
		}
		for _, p := range ports {
			d.Endpoints = append(d.Endpoints, store.DraftEndpoint{Host: e.GetHost(), Port: int(p)})
		}
	}
	for _, b := range rule.GetBinaries() {
		d.Binaries = append(d.Binaries, b.GetPath())
	}
	if rule != nil {
		if y, err := ruleYAML(d.Rule, rule); err == nil {
			d.YAML = y
		} else {
			d.YAML = "# " + err.Error() + "\n"
		}
	}
	return d
}
