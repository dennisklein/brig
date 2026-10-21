// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

package osclient

import (
	"context"
	"errors"
	"maps"
	"reflect"
	"slices"
	"sync"
	"testing"
	"time"

	dm "github.com/NVIDIA/OpenShell/sdk/go/proto/datamodelv1"
	pb "github.com/NVIDIA/OpenShell/sdk/go/proto/openshellv1"
	sb "github.com/NVIDIA/OpenShell/sdk/go/proto/sandboxv1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/structpb"
)

// sandboxGateway is a fake gateway for the calls of brig sandbox recreate.
type sandboxGateway struct {
	gateway
	mu      sync.Mutex
	created []*pb.CreateSandboxRequest
	calls   []string
	busy    int
}

func (g *sandboxGateway) GetSandbox(_ context.Context, r *pb.GetSandboxRequest) (*pb.SandboxResponse, error) {
	if r.GetName() != "webapp" || r.GetWorkspaceScope().GetWorkspace() != "team" {
		return nil, status.Error(codes.NotFound, "sandbox not found")
	}
	mounts, _ := structpb.NewStruct(map[string]any{"podman": map[string]any{"mounts": []any{
		map[string]any{"type": "volume", "source": "brig1", "target": "/usr/local/share/kit"},
		map[string]any{"type": "bind", "source": "/x", "target": "/y"},
	}}})
	return &pb.SandboxResponse{Sandbox: &pb.Sandbox{
		Metadata: &dm.ObjectMeta{Id: "1ac0", Name: "webapp", Workspace: "team", Labels: map[string]string{"app": "web"}},
		Spec: &pb.SandboxSpec{
			Template:                &pb.SandboxTemplate{Image: "localhost/claude-agent:2026-10-09", DriverConfig: mounts},
			Environment:             map[string]string{"A": "1"},
			Providers:               []string{"github", "claude-code"},
			Command:                 []string{"claude"},
			Tty:                     true,
			ProviderAttachmentEpoch: "7",
			RestartPolicy:           pb.SandboxRestartPolicy_SANDBOX_RESTART_POLICY_ALWAYS,
		},
		Status:                      &pb.SandboxStatus{Phase: pb.SandboxPhase_SANDBOX_PHASE_READY},
		CreatedFromWorkloadTemplate: &pb.SandboxWorkloadTemplateProvenance{Name: "claude"},
	}}, nil
}

func (g *sandboxGateway) ListServices(_ context.Context, r *pb.ListServicesRequest) (*pb.ListServicesResponse, error) {
	if err := checkScope(r.GetWorkspaceScope()); err != nil {
		return nil, err
	}
	return &pb.ListServicesResponse{Services: []*pb.ServiceEndpointResponse{
		{Endpoint: &pb.ServiceEndpoint{
			Sandbox: r.GetSandbox(), Name: "web", TargetPort: 8080,
			AuthorizationMode: pb.ServiceAuthorizationMode_SERVICE_AUTHORIZATION_MODE_STRIP,
		}},
		{Endpoint: &pb.ServiceEndpoint{Sandbox: "other", Name: "api", TargetPort: 9000}},
		{Endpoint: &pb.ServiceEndpoint{Sandbox: r.GetSandbox(), Name: "docs", TargetPort: 3000}},
	}}, nil
}

func (g *sandboxGateway) CreateSandbox(_ context.Context, r *pb.CreateSandboxRequest) (*pb.SandboxResponse, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.created = append(g.created, r)
	return &pb.SandboxResponse{}, nil
}

func (g *sandboxGateway) StopSandbox(_ context.Context, r *pb.StopSandboxRequest) (*pb.SandboxResponse, error) {
	return g.record("stop " + r.GetWorkspaceScope().GetWorkspace() + "/" + r.GetName())
}

func (g *sandboxGateway) StartSandbox(_ context.Context, r *pb.StartSandboxRequest) (*pb.SandboxResponse, error) {
	return g.record("start " + r.GetWorkspaceScope().GetWorkspace() + "/" + r.GetName())
}

func (g *sandboxGateway) record(call string) (*pb.SandboxResponse, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.busy > 0 {
		g.busy--
		return nil, status.Error(codes.FailedPrecondition, "previous driver operation is still pending")
	}
	g.calls = append(g.calls, call)
	return &pb.SandboxResponse{}, nil
}

func TestSandbox(t *testing.T) {
	c := serve(t, &sandboxGateway{})
	ctx := context.Background()
	r, err := c.Sandbox(ctx, "team", "webapp")
	if err != nil {
		t.Fatal(err)
	}
	if r.ID != "1ac0" || r.Name != "webapp" || r.Workspace != "team" || r.Phase != "Ready" || r.Template != "claude" ||
		r.Image != "localhost/claude-agent:2026-10-09" || r.Labels["app"] != "web" {
		t.Errorf("record %+v", r)
	}
	if r.Volume() != "openshell-sandbox-1ac0-workspace" {
		t.Errorf("volume %s", r.Volume())
	}
	if got := r.Providers(); !slices.Equal(got, []string{"claude-code", "github"}) {
		t.Errorf("providers %q", got)
	}
	if got := r.Command(); !slices.Equal(got, []string{"claude"}) {
		t.Errorf("command %q", got)
	}
	if got := r.VolumeMounts(); !slices.Equal(got, []string{"brig1"}) {
		t.Errorf("volume mounts %q", got)
	}
	if _, err := c.Sandbox(ctx, "team", "gone"); !errors.Is(err, ErrNoSandbox) {
		t.Errorf("a missing sandbox: %v", err)
	}

	services, err := c.Services(ctx, "team", "webapp")
	want := []Service{{Name: "docs", Port: 3000, Auth: "SERVICE_AUTHORIZATION_MODE_UNSPECIFIED"}, {Name: "web", Port: 8080, Auth: "SERVICE_AUTHORIZATION_MODE_STRIP"}}
	if err != nil || !reflect.DeepEqual(services, want) {
		t.Errorf("services %+v, %v", services, err)
	}
}

func TestBasePolicyLeavesOutProviderRules(t *testing.T) {
	c := serve(t, &sandboxGateway{})
	data, err := c.BasePolicy(context.Background(), "team", "webapp")
	if err != nil {
		t.Fatal(err)
	}
	r, _ := createRequest(CreateRequest{Spec: []byte("{}"), Policy: data})
	if got := slices.Sorted(maps.Keys(r.GetSpec().GetPolicy().GetNetworkPolicies())); !slices.Equal(got, []string{"pypi"}) {
		t.Errorf("rules %q", got)
	}
	if !proto.Equal(r.GetSpec().GetPolicy().GetFilesystem(), testPolicy().GetFilesystem()) {
		t.Errorf("filesystem %v", r.GetSpec().GetPolicy().GetFilesystem())
	}
}

func TestCreateSandbox(t *testing.T) {
	g := &sandboxGateway{}
	c := serve(t, g)
	ctx := context.Background()
	old, err := c.Sandbox(ctx, "team", "webapp")
	if err != nil {
		t.Fatal(err)
	}
	policy := []byte(`{"networkPolicies":{"npm":{"name":"npm"}}}`)
	services := []Service{{Name: "web", Port: 8080, Auth: "SERVICE_AUTHORIZATION_MODE_STRIP"}}
	labels := map[string]string{"app": "web"}
	annotations := map[string]string{"brig.dev/recreated-from": "localhost/claude-agent:2026-10-09"}

	// From a template, only the governance fields go with the request.
	err = c.CreateSandbox(ctx, CreateRequest{
		Workspace: "team", Name: "webapp", Spec: old.Spec, Template: "claude", Image: "ignored",
		Policy: policy, Labels: labels, Annotations: annotations, Services: services,
	})
	if err != nil {
		t.Fatal(err)
	}
	// Without, the whole spec does, with the new image.
	err = c.CreateSandbox(ctx, CreateRequest{Workspace: "team", Name: "webapp", Spec: old.Spec, Image: "localhost/claude-agent:2026-10-14"})
	if err != nil {
		t.Fatal(err)
	}
	if len(g.created) != 2 {
		t.Fatalf("created %v", g.created)
	}
	tm, img := g.created[0], g.created[1]
	wantPolicy := &sb.SandboxPolicy{NetworkPolicies: map[string]*sb.NetworkPolicyRule{"npm": {Name: "npm"}}}
	wantTemplate := &pb.SandboxSpec{
		Policy: wantPolicy, Providers: []string{"github", "claude-code"}, Command: []string{"claude"}, Tty: true,
		RestartPolicy: pb.SandboxRestartPolicy_SANDBOX_RESTART_POLICY_ALWAYS,
	}
	if tm.GetWorkloadTemplate() != "claude" || tm.GetName() != "webapp" || tm.GetWorkspaceScope().GetWorkspace() != "team" ||
		!proto.Equal(tm.GetSpec(), wantTemplate) || !maps.Equal(tm.GetLabels(), labels) ||
		!maps.Equal(tm.GetAnnotations(), annotations) {
		t.Errorf("create from the template %v", tm)
	}
	if len(tm.GetServiceExposures()) != 1 || tm.GetServiceExposures()[0].GetService() != "web" || tm.GetServiceExposures()[0].GetTargetPort() != 8080 ||
		tm.GetServiceExposures()[0].GetAuthorizationMode() != pb.ServiceAuthorizationMode_SERVICE_AUTHORIZATION_MODE_STRIP {
		t.Errorf("exposures %v", tm.GetServiceExposures())
	}
	if img.GetWorkloadTemplate() != "" || img.GetSpec().GetTemplate().GetImage() != "localhost/claude-agent:2026-10-14" ||
		img.GetSpec().GetProviderAttachmentEpoch() != "" || img.GetSpec().GetEnvironment()["A"] != "1" ||
		len(img.GetSpec().GetTemplate().GetDriverConfig().GetFields()) == 0 || img.GetSpec().GetPolicy() != nil {
		t.Errorf("create from the image %v", img)
	}
}

func TestStopAndStartRetryWhileBusy(t *testing.T) {
	orig := deleteRetry
	deleteRetry = time.Millisecond
	t.Cleanup(func() { deleteRetry = orig })
	g := &sandboxGateway{busy: 2}
	c := serve(t, g)
	ctx := context.Background()
	if err := c.StopSandbox(ctx, "team", "webapp"); err != nil {
		t.Fatal(err)
	}
	if err := c.StartSandbox(ctx, "team", "webapp"); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(g.calls, []string{"stop team/webapp", "start team/webapp"}) {
		t.Errorf("calls %q", g.calls)
	}
}

func TestPolicyJSONReadsWhatPolicyYAMLWrites(t *testing.T) {
	doc, err := policyDoc(testPolicy())
	if err != nil {
		t.Fatal(err)
	}
	file, err := doc.yaml()
	if err != nil {
		t.Fatal(err)
	}
	data, err := PolicyJSON([]byte(file))
	if err != nil {
		t.Fatal(err)
	}
	r, err := createRequest(CreateRequest{Spec: []byte("{}"), Policy: data})
	if err != nil {
		t.Fatal(err)
	}
	// Policy files lack what the gateway sets.
	want := testPolicy()
	want.Landlock.Compatibility = "best_effort"
	want.NetworkPolicies["pypi"].Endpoints[0].AdvisorProposed = false
	want.NetworkPolicies["_provider_github"].Endpoints[0].ProviderCredentialed = false
	want.NetworkPolicies["pypi"].Endpoints[0].Port, want.NetworkPolicies["pypi"].Endpoints[0].Ports = 443, nil
	want.Version = 1
	if got := r.GetSpec().GetPolicy(); !proto.Equal(got, want) {
		t.Errorf("got %v\nwant %v", got, want)
	}
	if _, err := PolicyJSON([]byte("version: 1\nnetwork_policies:\n  x:\n    endpoints:\n      - host: a\n        colour: red\n")); err == nil {
		t.Error("a key that OpenShell lacks was accepted")
	}
}

func TestKeepRule(t *testing.T) {
	from, err := PolicyJSON([]byte("version: 1\nnetwork_policies:\n  docker:\n    endpoints: [{host: h, ports: [1, 2], protocol: tcp}]\n  pypi:\n    endpoints: [{host: pypi.org, port: 443}]\n"))
	if err != nil {
		t.Fatal(err)
	}
	to, err := PolicyJSON([]byte("version: 1\nlandlock:\n  compatibility: hard_requirement\n"))
	if err != nil {
		t.Fatal(err)
	}
	got, kept, err := KeepRule(from, to, "docker")
	if err != nil || !kept {
		t.Fatalf("KeepRule = %v, %v", kept, err)
	}
	p := &sb.SandboxPolicy{}
	if err := protojson.Unmarshal(got, p); err != nil {
		t.Fatal(err)
	}
	if r := p.GetNetworkPolicies(); len(r) != 1 || len(r["docker"].GetEndpoints()[0].GetPorts()) != 2 || p.GetLandlock().GetCompatibility() != "hard_requirement" {
		t.Errorf("policy %v", p)
	}
	if again, kept, err := KeepRule(from, got, "docker"); err != nil || kept || string(again) != string(got) {
		t.Errorf("KeepRule with the rule there: %v, %v", kept, err)
	}
	if again, kept, err := KeepRule(to, got, "docker"); err != nil || kept || string(again) != string(got) {
		t.Errorf("KeepRule without the rule to keep: %v, %v", kept, err)
	}
}
