// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

package osclient

import (
	"context"
	"maps"
	"slices"
	"time"

	pb "github.com/NVIDIA/OpenShell/sdk/go/proto/openshellv1"
)

// Revision is a revision of a sandbox's policy, as the gateway keeps it
// until the sandbox is deleted.
type Revision struct {
	Version uint32
	Hash    string
	// Status is pending, loaded, failed or superseded; LoadError says why
	// a failed one failed.
	Status    string
	LoadError string
	Created   time.Time
	Loaded    time.Time
	// Policy is the revision's policy in the shape of a policy file, as
	// YAML; Err says why it could not be written.
	Policy string
	Err    string
	// Provenance holds the annotations that the change came with.
	Provenance map[string]string
}

// revisionsPage is how many revisions a page of ListSandboxPolicies asks
// for.
const revisionsPage = 100

// Revisions lists the revisions of a sandbox's policy newer than after,
// oldest first. The gateway lists the newest first, so the pages stop at
// the first revision not newer than after.
func (c *Client) Revisions(ctx context.Context, workspace, sandbox string, after uint32) ([]Revision, error) {
	var out []Revision
	token := ""
	for range maxPages {
		r, err := call(ctx, func(ctx context.Context) (*pb.ListSandboxPoliciesResponse, error) {
			return c.api.ListSandboxPolicies(ctx, &pb.ListSandboxPoliciesRequest{
				WorkspaceScope: scope(workspace), Sandbox: sandbox, PageSize: revisionsPage, PageToken: token,
			})
		})
		if err != nil {
			return nil, err
		}
		done := false
		for _, rev := range r.GetRevisions() {
			if rev.GetVersion() <= after {
				done = true
				continue
			}
			out = append(out, revision(rev))
		}
		if done || r.GetNextPageToken() == "" {
			break
		}
		token = r.GetNextPageToken()
	}
	slices.SortFunc(out, func(a, b Revision) int { return int(a.Version) - int(b.Version) })
	return slices.CompactFunc(out, func(a, b Revision) bool { return a.Version == b.Version }), nil
}

func revision(rev *pb.SandboxPolicyRevision) Revision {
	out := Revision{
		Version: rev.GetVersion(), Hash: rev.GetPolicyHash(),
		Status: PolicyStatuses[rev.GetStatus()], LoadError: rev.GetLoadError(),
	}
	if out.Status == "" {
		out.Status = "unknown"
	}
	if t := rev.GetCreatedTime(); t != nil {
		out.Created = t.AsTime()
	}
	if t := rev.GetLoadedTime(); t != nil {
		out.Loaded = t.AsTime()
	}
	if p := rev.GetProvenance(); len(p) > 0 {
		out.Provenance = maps.Clone(p)
	}
	if rev.GetPolicy() == nil {
		return out
	}
	doc, err := policyDoc(rev.GetPolicy())
	if err == nil {
		out.Policy, err = doc.yaml()
	}
	if err != nil {
		out.Err = err.Error()
	}
	return out
}
