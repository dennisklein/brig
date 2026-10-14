// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

package osclient

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	pb "github.com/NVIDIA/OpenShell/sdk/go/proto/openshellv1"
	sb "github.com/NVIDIA/OpenShell/sdk/go/proto/sandboxv1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

// ErrNoSandbox is the error of Sandbox for a sandbox the gateway does not
// have.
var ErrNoSandbox = errors.New("no such sandbox")

// SandboxRecord is what brig keeps of a sandbox to create it again.
type SandboxRecord struct {
	ID        string            `json:"id"`
	Workspace string            `json:"workspace"`
	Name      string            `json:"name"`
	Phase     string            `json:"phase"`
	Image     string            `json:"image"`
	Template  string            `json:"template,omitempty"`
	Labels    map[string]string `json:"labels,omitempty"`
	// Problem is what the gateway says is wrong with the sandbox, if
	// anything.
	Problem string `json:"problem,omitempty"`
	// Spec is the sandbox's spec as the gateway resolved it, template
	// fields included, as protobuf JSON.
	Spec json.RawMessage `json:"spec"`
}

// Volume returns the name of the Podman volume that holds the sandbox's
// /sandbox, as OpenShell's Podman driver names it.
func (r SandboxRecord) Volume() string { return "openshell-sandbox-" + r.ID + "-workspace" }

// Providers returns the names of the providers attached to the sandbox.
func (r SandboxRecord) Providers() []string {
	s, err := r.spec()
	if err != nil {
		return nil
	}
	return slices.Sorted(slices.Values(s.GetProviders()))
}

// Command returns the sandbox's command, empty for the image's.
func (r SandboxRecord) Command() []string {
	s, err := r.spec()
	if err != nil {
		return nil
	}
	return s.GetCommand()
}

// VolumeMounts returns the Podman volumes that the spec's driver
// configuration attaches, as brig sandbox create --mount does.
func (r SandboxRecord) VolumeMounts() []string {
	s, err := r.spec()
	if err != nil {
		return nil
	}
	var out []string
	mounts, _ := s.GetTemplate().GetDriverConfig().AsMap()["podman"].(map[string]any)
	list, _ := mounts["mounts"].([]any)
	for _, m := range list {
		if m, ok := m.(map[string]any); ok && m["type"] == "volume" {
			if src, ok := m["source"].(string); ok {
				out = append(out, src)
			}
		}
	}
	return out
}

func (r SandboxRecord) spec() (*pb.SandboxSpec, error) {
	s := &pb.SandboxSpec{}
	if err := protojson.Unmarshal(r.Spec, s); err != nil {
		return nil, fmt.Errorf("the spec of sandbox %s: %w", r.Name, err)
	}
	return s, nil
}

// Sandbox reads a sandbox with its full spec. A sandbox the gateway does
// not have is ErrNoSandbox.
func (c *Client) Sandbox(ctx context.Context, workspace, name string) (SandboxRecord, error) {
	r, err := call(ctx, func(ctx context.Context) (*pb.SandboxResponse, error) {
		return c.api.GetSandbox(ctx, &pb.GetSandboxRequest{WorkspaceScope: scope(workspace), Name: name})
	})
	if status.Code(err) == codes.NotFound {
		return SandboxRecord{}, fmt.Errorf("sandbox %s in workspace %s: %w", name, workspace, ErrNoSandbox)
	}
	if err != nil {
		return SandboxRecord{}, err
	}
	s := r.GetSandbox()
	spec, err := protojson.Marshal(s.GetSpec())
	if err != nil {
		return SandboxRecord{}, err
	}
	st := sandbox(s)
	return SandboxRecord{
		ID: s.GetMetadata().GetId(), Workspace: st.Workspace, Name: st.Name, Phase: st.Phase, Image: st.Image,
		Template: st.Template, Labels: st.Labels, Problem: st.Problem, Spec: spec,
	}, nil
}

// Service is an HTTP service that a sandbox exposes through the gateway.
type Service struct {
	Name string `json:"name"`
	Port uint32 `json:"port"`
	// Auth is the service's authorization mode as its protobuf name.
	Auth string `json:"auth,omitempty"`
}

// Services lists the services that a sandbox exposes.
func (c *Client) Services(ctx context.Context, workspace, sandbox string) ([]Service, error) {
	list, err := pages(ctx, func(ctx context.Context, token string) ([]*pb.ServiceEndpointResponse, string, error) {
		r, err := c.api.ListServices(ctx, &pb.ListServicesRequest{WorkspaceScope: scope(workspace), Sandbox: sandbox, PageSize: pageSize, PageToken: token})
		return r.GetServices(), r.GetNextPageToken(), err
	})
	if err != nil {
		return nil, err
	}
	out := []Service{}
	for _, s := range list {
		e := s.GetEndpoint()
		if e.GetSandbox() != "" && e.GetSandbox() != sandbox {
			continue
		}
		out = append(out, Service{Name: e.GetName(), Port: e.GetTargetPort(), Auth: e.GetAuthorizationMode().String()})
	}
	slices.SortFunc(out, func(a, b Service) int { return strings.Compare(a.Name, b.Name) })
	return out, nil
}

// BasePolicy returns the policy that a sandbox enforces without the rules
// of its providers, which the gateway adds again to a sandbox that attaches
// them, as protobuf JSON.
func (c *Client) BasePolicy(ctx context.Context, workspace, sandbox string) (json.RawMessage, error) {
	r, err := call(ctx, func(ctx context.Context) (*sb.GetSandboxConfigResponse, error) {
		return c.api.GetSandboxConfig(ctx, &sb.GetSandboxConfigRequest{WorkspaceScope: scope(workspace), Name: sandbox})
	})
	if err != nil {
		return nil, err
	}
	if r.GetPolicy() == nil {
		return nil, fmt.Errorf("sandbox %s has no policy", sandbox)
	}
	p := proto.CloneOf(r.GetPolicy())
	maps.DeleteFunc(p.GetNetworkPolicies(), func(k string, _ *sb.NetworkPolicyRule) bool {
		return strings.HasPrefix(k, providerRulePrefix)
	})
	return protojson.Marshal(p)
}

// CreateRequest is a sandbox for CreateSandbox to create from the spec of
// another.
type CreateRequest struct {
	Workspace, Name string
	// Spec is the other sandbox's spec, as SandboxRecord has it.
	Spec json.RawMessage
	// Template creates the sandbox from this template, which then
	// supplies the image, environment, resources and driver
	// configuration; Spec gives only the policy, providers, command,
	// terminal and restart policy, which are all a create from a
	// template takes. Without Template, the sandbox gets all of Spec.
	Template string
	// Image replaces the image of Spec; it is ignored with Template.
	Image string
	// Policy replaces the policy of Spec, as protobuf JSON, unless nil.
	Policy      json.RawMessage
	Labels      map[string]string
	Annotations map[string]string
	Services    []Service
}

// CreateSandbox creates a sandbox and returns without waiting for it to be
// ready.
func (c *Client) CreateSandbox(ctx context.Context, r CreateRequest) error {
	req, err := createRequest(r)
	if err != nil {
		return err
	}
	_, err = call(ctx, func(ctx context.Context) (*pb.SandboxResponse, error) {
		return c.api.CreateSandbox(ctx, req)
	})
	return err
}

func createRequest(r CreateRequest) (*pb.CreateSandboxRequest, error) {
	old := &pb.SandboxSpec{}
	if err := protojson.Unmarshal(r.Spec, old); err != nil {
		return nil, fmt.Errorf("the spec of sandbox %s: %w", r.Name, err)
	}
	if r.Policy != nil {
		old.Policy = &sb.SandboxPolicy{}
		if err := protojson.Unmarshal(r.Policy, old.Policy); err != nil {
			return nil, fmt.Errorf("the policy of sandbox %s: %w", r.Name, err)
		}
	}
	spec := old
	if r.Template != "" {
		spec = &pb.SandboxSpec{
			Policy: old.GetPolicy(), Providers: old.GetProviders(), Command: old.GetCommand(), Tty: old.GetTty(),
			RestartPolicy: old.GetRestartPolicy(),
		}
	} else {
		if r.Image != "" {
			if spec.Template == nil {
				spec.Template = &pb.SandboxTemplate{}
			}
			spec.Template.Image = r.Image
		}
		// The gateway owns it.
		spec.ProviderAttachmentEpoch = ""
	}
	req := &pb.CreateSandboxRequest{
		WorkspaceScope: scope(r.Workspace), Name: r.Name, Spec: spec, WorkloadTemplate: r.Template,
		Labels: maps.Clone(r.Labels), Annotations: maps.Clone(r.Annotations),
	}
	for _, s := range r.Services {
		req.ServiceExposures = append(req.ServiceExposures, &pb.SandboxServiceExposure{
			Service: s.Name, TargetPort: s.Port,
			AuthorizationMode: pb.ServiceAuthorizationMode(pb.ServiceAuthorizationMode_value[s.Auth]),
		})
	}
	return req, nil
}

// StopSandbox stops a sandbox and returns without waiting for it to stop.
func (c *Client) StopSandbox(ctx context.Context, workspace, name string) error {
	return retryBusy(ctx, deleteTimeout, func(ctx context.Context) error {
		_, err := c.api.StopSandbox(ctx, &pb.StopSandboxRequest{WorkspaceScope: scope(workspace), Name: name})
		return err
	})
}

// StartSandbox starts a stopped sandbox and returns without waiting for it
// to be ready.
func (c *Client) StartSandbox(ctx context.Context, workspace, name string) error {
	return retryBusy(ctx, deleteTimeout, func(ctx context.Context) error {
		_, err := c.api.StartSandbox(ctx, &pb.StartSandboxRequest{WorkspaceScope: scope(workspace), Name: name})
		return err
	})
}

// retryBusy calls f until the gateway no longer refuses it as busy with
// another operation on the sandbox, for at most timeout.
func retryBusy(ctx context.Context, timeout time.Duration, f func(context.Context) error) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	pause := deleteRetry
	for {
		err := f(ctx)
		if !busy(err) {
			return err
		}
		select {
		case <-ctx.Done():
			return err
		case <-time.After(pause):
		}
		pause = min(2*pause, 5*time.Second)
	}
}
