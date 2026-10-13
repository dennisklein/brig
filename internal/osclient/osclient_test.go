// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

package osclient

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	dm "github.com/NVIDIA/OpenShell/sdk/go/proto/datamodelv1"
	pb "github.com/NVIDIA/OpenShell/sdk/go/proto/openshellv1"
	sb "github.com/NVIDIA/OpenShell/sdk/go/proto/sandboxv1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/dennisklein/brig/internal/ui/store"
)

// gateway is a fake OpenShell gateway holding the sandboxes, providers,
// profiles and templates of the tests.
type gateway struct {
	pb.UnimplementedOpenShellServer
	// pageSize splits lists into pages of this many items.
	pageSize int
	mu       sync.Mutex
	// watches are the watch requests it got.
	watches []*pb.WatchSandboxRequest
	// profileScopes are the workspaces ListProviderProfiles was asked for.
	profileScopes []string
	// deleted are the sandboxes it was asked to delete, as workspace/name.
	deleted []string
	// deleteBusy is how many deletions it refuses as pending first.
	deleteBusy int
}

func (g *gateway) DeleteSandbox(_ context.Context, r *pb.DeleteSandboxRequest) (*pb.DeleteSandboxResponse, error) {
	if err := checkScope(r.GetWorkspaceScope()); err != nil {
		return nil, err
	}
	if !r.GetAllowMissing() {
		return nil, status.Error(codes.NotFound, "sandbox not found")
	}
	g.mu.Lock()
	if g.deleteBusy > 0 {
		g.deleteBusy--
		g.mu.Unlock()
		return nil, status.Error(codes.FailedPrecondition, "previous driver operation is still pending; retry and deletion are blocked")
	}
	g.deleted = append(g.deleted, r.GetWorkspaceScope().GetWorkspace()+"/"+r.GetName())
	g.mu.Unlock()
	return &pb.DeleteSandboxResponse{}, nil
}

func page[T any](items []T, token string, size int) ([]T, string) {
	start, _ := strconv.Atoi(token)
	end := min(len(items), start+size)
	next := ""
	if end < len(items) {
		next = strconv.Itoa(end)
	}
	return items[start:end], next
}

// workspaceNames are the workspaces of the fake gateway.
var workspaceNames = []string{"default", "team"}

// scopeOf returns the workspaces a selector names: the one, or with all
// set, every workspace when the selector asks for all.
func scopeOf(ws *dm.WorkspaceSelector, all bool) ([]string, error) {
	switch {
	case ws.GetAllWorkspaces() != nil && all:
		return workspaceNames, nil
	case ws.GetWorkspace() != "" && slices.Contains(workspaceNames, ws.GetWorkspace()):
		return []string{ws.GetWorkspace()}, nil
	}
	return nil, status.Error(codes.InvalidArgument, "bad workspace selector")
}

// checkScope accepts a selector of one existing workspace.
func checkScope(ws *dm.WorkspaceSelector) error {
	_, err := scopeOf(ws, false)
	return err
}

// metaIn is the metadata of an object in a workspace. The default workspace
// is left implicit, as a gateway may send it.
func metaIn(workspace, name string) *dm.ObjectMeta {
	if workspace == "default" {
		return &dm.ObjectMeta{Name: name}
	}
	return &dm.ObjectMeta{Name: name, Workspace: workspace}
}

func (g *gateway) Health(context.Context, *pb.HealthRequest) (*pb.HealthResponse, error) {
	return &pb.HealthResponse{Status: pb.ServiceStatus_SERVICE_STATUS_HEALTHY, Version: "0.1.3"}, nil
}

var created = time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)

func (g *gateway) ListWorkspaces(_ context.Context, r *pb.ListWorkspacesRequest) (*pb.ListWorkspacesResponse, error) {
	var all []*dm.Workspace
	// Reversed, to show that the client sorts.
	for _, n := range slices.Backward(workspaceNames) {
		all = append(all, &dm.Workspace{Metadata: &dm.ObjectMeta{Name: n}})
	}
	items, next := page(all, r.GetPageToken(), g.pageSize)
	return &pb.ListWorkspacesResponse{Workspaces: items, NextPageToken: next}, nil
}

func (g *gateway) ListSandboxes(_ context.Context, r *pb.ListSandboxesRequest) (*pb.ListSandboxesResponse, error) {
	wss, err := scopeOf(r.GetWorkspaceScope(), true)
	if err != nil {
		return nil, err
	}
	var all []*pb.Sandbox
	for _, ws := range wss {
		switch ws {
		case "default":
			all = append(all,
				&pb.Sandbox{
					Metadata: &dm.ObjectMeta{Name: "webapp", CreatedTime: timestamppb.New(created), Labels: map[string]string{"brig.dev/run": "r1"}},
					Spec: &pb.SandboxSpec{
						Template:    &pb.SandboxTemplate{Image: "localhost/pi-agent:2026-10-08"},
						Providers:   []string{"llm", "github"},
						Environment: map[string]string{"SECRET": "s3cret"},
					},
					Status:                      &pb.SandboxStatus{Phase: pb.SandboxPhase_SANDBOX_PHASE_READY, CurrentPolicyVersion: 3},
					CreatedFromWorkloadTemplate: &pb.SandboxWorkloadTemplateProvenance{Name: "pi"},
				},
				&pb.Sandbox{
					Metadata: &dm.ObjectMeta{Name: "broken"},
					Status: &pb.SandboxStatus{Phase: pb.SandboxPhase_SANDBOX_PHASE_ERROR, Conditions: []*pb.SandboxCondition{
						{Type: "Ready", Status: "False", Message: "image pull failed"},
					}},
				},
				// Named explicitly, as the default workspace may be.
				&pb.Sandbox{Metadata: &dm.ObjectMeta{Name: "odd", Workspace: "default"}, Status: &pb.SandboxStatus{Phase: 42}},
			)
		case "team":
			all = append(all,
				// The same name as the default workspace's sandbox.
				&pb.Sandbox{
					Metadata: metaIn("team", "webapp"),
					Spec:     &pb.SandboxSpec{Template: &pb.SandboxTemplate{Image: "localhost/claude-agent:2026-10-01"}, Providers: []string{"llm"}},
					Status:   &pb.SandboxStatus{Phase: pb.SandboxPhase_SANDBOX_PHASE_STOPPED, CurrentPolicyVersion: 7},
				},
				&pb.Sandbox{Metadata: &dm.ObjectMeta{Name: "ci", Workspace: "team", Labels: map[string]string{"brig.dev/run": "r1"}}, Status: &pb.SandboxStatus{Phase: pb.SandboxPhase_SANDBOX_PHASE_READY, CurrentPolicyVersion: 1}},
			)
		}
	}
	items, next := page(all, r.GetPageToken(), g.pageSize)
	return &pb.ListSandboxesResponse{Sandboxes: items, NextPageToken: next}, nil
}

func (g *gateway) ListProviders(_ context.Context, r *pb.ListProvidersRequest) (*pb.ListProvidersResponse, error) {
	wss, err := scopeOf(r.GetWorkspaceScope(), true)
	if err != nil {
		return nil, err
	}
	var all []*dm.Provider
	for _, ws := range wss {
		switch ws {
		case "default":
			all = append(all,
				&dm.Provider{Metadata: metaIn(ws, "llm"), Type: "openai", Credentials: map[string]string{"OPENAI_API_KEY": "sk-not-redacted"}},
				&dm.Provider{Metadata: metaIn(ws, "github"), Type: "github", Credentials: map[string]string{"GH_TOKEN": "REDACTED", "GH_HOST": "REDACTED"}},
			)
		case "team":
			// The same name as in the default workspace.
			all = append(all, &dm.Provider{Metadata: metaIn(ws, "llm"), Type: "anthropic", Credentials: map[string]string{"ANTHROPIC_API_KEY": "REDACTED"}})
		}
	}
	items, next := page(all, r.GetPageToken(), g.pageSize)
	return &pb.ListProvidersResponse{Providers: items, NextPageToken: next}, nil
}

// ListProviderProfiles takes one named workspace only, like the real
// gateway, and lists the platform's profiles with the workspace's own.
func (g *gateway) ListProviderProfiles(_ context.Context, r *pb.ListProviderProfilesRequest) (*pb.ListProviderProfilesResponse, error) {
	if err := checkScope(r.GetWorkspaceScope()); err != nil {
		return nil, err
	}
	g.mu.Lock()
	g.profileScopes = append(g.profileScopes, r.GetWorkspaceScope().GetWorkspace())
	g.mu.Unlock()
	platform := &pb.ProviderProfile{Id: "claude-code", DisplayName: "Claude Code", Source: "builtin", Scope: "platform"}
	github := &pb.ProviderProfile{
		Id: "github", DisplayName: "GitHub", Category: pb.ProviderProfileCategory_PROVIDER_PROFILE_CATEGORY_SOURCE_CONTROL,
		Source: "user", Scope: "workspace",
		Credentials: []*pb.ProviderProfileCredential{{Name: "token", EnvVars: []string{"GH_TOKEN"}, AuthStyle: "bearer", Required: true}},
		Endpoints: []*sb.NetworkEndpoint{
			{Host: "api.github.com", Port: 443},
			{Host: "github.com", Ports: []uint32{22, 443}},
		},
		Binaries: []*sb.NetworkBinary{{Path: "/usr/bin/gh"}},
	}
	if r.GetWorkspaceScope().GetWorkspace() == "team" {
		github = &pb.ProviderProfile{Id: "github", DisplayName: "GitHub (team)", Source: "user", Scope: "workspace"}
	}
	return &pb.ListProviderProfilesResponse{Profiles: []*pb.ProviderProfile{platform, github}}, nil
}

func (g *gateway) ListSandboxTemplates(_ context.Context, r *pb.ListSandboxTemplatesRequest) (*pb.ListSandboxTemplatesResponse, error) {
	wss, err := scopeOf(r.GetWorkspaceScope(), true)
	if err != nil {
		return nil, err
	}
	var all []*pb.SandboxWorkloadTemplate
	for _, ws := range wss {
		name, image := "pi", "localhost/pi-agent:2026-10-08"
		if ws == "team" {
			name, image = "claude", "localhost/claude-agent:2026-10-01"
		}
		all = append(all, &pb.SandboxWorkloadTemplate{
			Metadata: metaIn(ws, name),
			Spec: &pb.SandboxWorkloadTemplateSpec{Workload: &pb.SandboxWorkloadConfig{
				Image: image, Resources: &pb.SandboxResources{Cpu: "2", Memory: "4Gi"},
			}},
		})
	}
	items, next := page(all, r.GetPageToken(), g.pageSize)
	return &pb.ListSandboxTemplatesResponse{Templates: items, NextPageToken: next}, nil
}

func (g *gateway) GetSandboxConfig(_ context.Context, r *sb.GetSandboxConfigRequest) (*sb.GetSandboxConfigResponse, error) {
	if err := checkScope(r.GetWorkspaceScope()); err != nil {
		return nil, err
	}
	version := uint32(3)
	if r.GetWorkspaceScope().GetWorkspace() == "team" {
		version = 7
	}
	return &sb.GetSandboxConfigResponse{Version: version, PolicySource: sb.PolicySource_POLICY_SOURCE_SANDBOX, Policy: testPolicy()}, nil
}

// lastExec is what the fake gateway was last asked to run.
var lastExec atomic.Pointer[pb.ExecSandboxRequest]

func (g *gateway) ExecSandbox(r *pb.ExecSandboxRequest, stream grpc.ServerStreamingServer[pb.ExecSandboxEvent]) error {
	if err := checkScope(r.GetWorkspaceScope()); err != nil {
		return err
	}
	lastExec.Store(r)
	if r.GetSandbox() == "webapp" && r.GetWorkspaceScope().GetWorkspace() == "team" {
		_ = stream.Send(&pb.ExecSandboxEvent{Payload: &pb.ExecSandboxEvent_Stdout{Stdout: &pb.ExecSandboxStdout{Data: []byte("f\t1\t1760000000.0\tteam-only\x00")}}})
		return stream.Send(&pb.ExecSandboxEvent{Payload: &pb.ExecSandboxEvent_Exit{Exit: &pb.ExecSandboxExit{}}})
	}
	send := func(ev *pb.ExecSandboxEvent) {
		_ = stream.Send(ev)
	}
	stdout := func(s string) {
		send(&pb.ExecSandboxEvent{Payload: &pb.ExecSandboxEvent_Stdout{Stdout: &pb.ExecSandboxStdout{Data: []byte(s)}}})
	}
	exit := func(code int32) {
		send(&pb.ExecSandboxEvent{Payload: &pb.ExecSandboxEvent_Exit{Exit: &pb.ExecSandboxExit{ExitCode: code}}})
	}
	switch r.GetSandbox() {
	case "webapp":
		// Records may split across messages.
		stdout("d\t4096\t1760000000.5\t.git\x00f\t12\t1760000100.0\tREAD")
		stdout("ME.md\x00l\t9\t1760000200.0\tlatest\x00d\t4096\t1760000300.0\tmy dir\nx\x00s\t0\t1760000400.0\tsock\x00")
		exit(0)
	case "empty":
		exit(0)
	case "nodir":
		send(&pb.ExecSandboxEvent{Payload: &pb.ExecSandboxEvent_Stderr{Stderr: &pb.ExecSandboxStderr{Data: []byte("find: '/sandbox/x': No such file or directory\n")}}})
		exit(1)
	case "garbled":
		stdout("what\x00")
		exit(0)
	case "silent":
	case "task":
		stdout("out")
		send(&pb.ExecSandboxEvent{Payload: &pb.ExecSandboxEvent_Stderr{Stderr: &pb.ExecSandboxStderr{Data: []byte("err")}}})
		stdout("put")
		exit(3)
	case "huge":
		var b strings.Builder
		for i := range maxFiles + 5 {
			b.WriteString("f\t1\t1\tf" + strconv.Itoa(i) + "\x00")
		}
		stdout(b.String())
		exit(0)
	}
	return nil
}

func testPolicy() *sb.SandboxPolicy {
	return &sb.SandboxPolicy{
		Filesystem: &sb.FilesystemPolicy{ReadOnly: []string{"/usr", "/etc"}, ReadWrite: []string{"/sandbox"}},
		Landlock:   &sb.LandlockPolicy{},
		Process:    &sb.ProcessPolicy{RunAsUser: "sandbox"},
		NetworkPolicies: map[string]*sb.NetworkPolicyRule{
			"pypi": {
				Name: "pypi",
				Endpoints: []*sb.NetworkEndpoint{{
					Host: "pypi.org", Ports: []uint32{443},
					Access: sb.NetworkAccessPreset_NETWORK_ACCESS_PRESET_READ_ONLY, Enforcement: sb.NetworkEnforcementMode_NETWORK_ENFORCEMENT_MODE_AUDIT,
					AdvisorProposed: true,
				}},
				Binaries: []*sb.NetworkBinary{{Path: "/usr/bin/pip"}},
			},
			"_provider_github": {
				Name: "_provider_github",
				Endpoints: []*sb.NetworkEndpoint{
					{Host: "api.github.com", Port: 443, ProviderCredentialed: true},
					{Host: "github.com", Ports: []uint32{22, 443}, Rules: []*sb.L7Rule{{Allow: &sb.L7Allow{Method: "GET", Path: "/**"}}}},
				},
				Binaries: []*sb.NetworkBinary{{Path: "/usr/bin/gh"}},
			},
		},
	}
}

// pki writes a CA, a server certificate for 127.0.0.1 and a client
// bundle as brig keeps it, and returns the bundle's directory and the
// server's TLS config.
func pki(t *testing.T) (string, *tls.Config) {
	t.Helper()
	newKey := func() *ecdsa.PrivateKey {
		k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		return k
	}
	caKey := newKey()
	caTmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "openshell-ca"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	ca, _ := x509.ParseCertificate(caDER)
	issue := func(serial int64, usage x509.ExtKeyUsage, ips []net.IP) ([]byte, *ecdsa.PrivateKey) {
		k := newKey()
		tmpl := &x509.Certificate{
			SerialNumber: big.NewInt(serial), Subject: pkix.Name{CommonName: "x"},
			NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
			ExtKeyUsage: []x509.ExtKeyUsage{usage}, IPAddresses: ips,
		}
		der, err := x509.CreateCertificate(rand.Reader, tmpl, ca, &k.PublicKey, caKey)
		if err != nil {
			t.Fatal(err)
		}
		return der, k
	}
	pemOf := func(typ string, der []byte) []byte { return pem.EncodeToMemory(&pem.Block{Type: typ, Bytes: der}) }
	keyPEM := func(k *ecdsa.PrivateKey) []byte {
		der, err := x509.MarshalPKCS8PrivateKey(k)
		if err != nil {
			t.Fatal(err)
		}
		return pemOf("PRIVATE KEY", der)
	}
	srvDER, srvKey := issue(2, x509.ExtKeyUsageServerAuth, []net.IP{net.IPv4(127, 0, 0, 1)})
	cliDER, cliKey := issue(3, x509.ExtKeyUsageClientAuth, nil)

	dir := t.TempDir()
	for name, data := range map[string][]byte{
		"ca.crt": pemOf("CERTIFICATE", caDER), "tls.crt": pemOf("CERTIFICATE", cliDER), "tls.key": keyPEM(cliKey),
	} {
		if err := os.WriteFile(filepath.Join(dir, name), data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	srvCert, err := tls.X509KeyPair(pemOf("CERTIFICATE", srvDER), keyPEM(srvKey))
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(ca)
	return dir, &tls.Config{Certificates: []tls.Certificate{srvCert}, ClientCAs: pool, ClientAuth: tls.RequireAndVerifyClientCert, MinVersion: tls.VersionTLS12}
}

// serve runs the fake gateway on 127.0.0.1 with mTLS and returns a client
// of it.
func serve(t *testing.T, g pb.OpenShellServer) *Client {
	t.Helper()
	dir, cfg := pki(t)
	l, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := grpc.NewServer(grpc.Creds(credentials.NewTLS(cfg)))
	pb.RegisterOpenShellServer(srv, g)
	go func() { _ = srv.Serve(l) }()
	t.Cleanup(srv.Stop)
	c, err := Dial(l.Addr().(*net.TCPAddr).Port, dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func TestReads(t *testing.T) {
	c := serve(t, &gateway{pageSize: 1})
	ctx := context.Background()

	v, err := c.Version(ctx)
	if err != nil || v != "0.1.3" {
		t.Fatalf("Version: %q, %v", v, err)
	}

	wss, err := c.Workspaces(ctx)
	if err != nil || !reflect.DeepEqual(wss, []string{"default", "team"}) {
		t.Errorf("Workspaces = %v, %v", wss, err)
	}

	sbs, err := c.Sandboxes(ctx)
	if err != nil {
		t.Fatal(err)
	}
	want := []store.Sandbox{
		{Workspace: "default", Name: "broken", Phase: "Error", Problem: "image pull failed"},
		{Workspace: "default", Name: "odd", Phase: "Unknown"},
		{
			Workspace: "default", Name: "webapp", Phase: "Ready", Image: "localhost/pi-agent:2026-10-08", Template: "pi",
			Providers: []string{"github", "llm"}, Labels: map[string]string{"brig.dev/run": "r1"}, Created: created, PolicyVersion: 3,
		},
		{Workspace: "team", Name: "ci", Phase: "Ready", Labels: map[string]string{"brig.dev/run": "r1"}, PolicyVersion: 1},
		{
			Workspace: "team", Name: "webapp", Phase: "Stopped", Image: "localhost/claude-agent:2026-10-01",
			Providers: []string{"llm"}, PolicyVersion: 7,
		},
	}
	if !reflect.DeepEqual(sbs, want) {
		t.Errorf("Sandboxes:\ngot  %+v\nwant %+v", sbs, want)
	}

	provs, err := c.Providers(ctx)
	if err != nil {
		t.Fatal(err)
	}
	wantProvs := []store.Provider{
		{Workspace: "default", Name: "github", Type: "github", Credentials: []string{"GH_HOST", "GH_TOKEN"}},
		{Workspace: "default", Name: "llm", Type: "openai", Credentials: []string{"OPENAI_API_KEY"}},
		{Workspace: "team", Name: "llm", Type: "anthropic", Credentials: []string{"ANTHROPIC_API_KEY"}},
	}
	if !reflect.DeepEqual(provs, wantProvs) {
		t.Errorf("Providers:\ngot  %+v\nwant %+v", provs, wantProvs)
	}

	profs, err := c.Profiles(ctx, wss)
	if err != nil {
		t.Fatal(err)
	}
	// The platform's profile comes with every workspace and is listed
	// once, with no workspace.
	wantProfs := []store.Profile{
		{ID: "claude-code", Name: "Claude Code", Source: "builtin", Scope: "platform"},
		{
			Workspace: "default", ID: "github", Name: "GitHub", Category: "source control", Source: "user", Scope: "workspace",
			Credentials: []store.ProfileCredential{{Name: "token", EnvVars: []string{"GH_TOKEN"}, Auth: "bearer", Required: true}},
			Endpoints:   []string{"api.github.com:443", "github.com:22", "github.com:443"},
			Binaries:    []string{"/usr/bin/gh"},
		},
		{Workspace: "team", ID: "github", Name: "GitHub (team)", Source: "user", Scope: "workspace"},
	}
	if !reflect.DeepEqual(profs, wantProfs) {
		t.Errorf("Profiles:\ngot  %+v\nwant %+v", profs, wantProfs)
	}

	tmpls, err := c.Templates(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if want := []store.Template{
		{Workspace: "default", Name: "pi", Image: "localhost/pi-agent:2026-10-08", CPU: "2", Memory: "4Gi"},
		{Workspace: "team", Name: "claude", Image: "localhost/claude-agent:2026-10-01", CPU: "2", Memory: "4Gi"},
	}; !reflect.DeepEqual(tmpls, want) {
		t.Errorf("Templates: %+v", tmpls)
	}

	p, err := c.Policy(ctx, "default", "webapp")
	if err != nil {
		t.Fatal(err)
	}
	if p.Err != "" || p.Version != 3 || p.Global || !reflect.DeepEqual(p.ProviderRules, []string{"_provider_github"}) {
		t.Errorf("Policy: %+v", p)
	}
	if strings.Contains(p.Base, "_provider_") || !strings.Contains(p.Effective, "_provider_github:") {
		t.Errorf("Policy base:\n%s\neffective:\n%s", p.Base, p.Effective)
	}
}

// TestWorkspaceKeying asks for the same sandbox name in two workspaces and
// gets each workspace's own answer.
func TestWorkspaceKeying(t *testing.T) {
	g := &gateway{pageSize: 2}
	c := serve(t, g)
	ctx := context.Background()

	for ws, version := range map[string]uint32{"default": 3, "team": 7} {
		p, err := c.Policy(ctx, ws, "webapp")
		if err != nil || p.Version != version {
			t.Errorf("Policy(%s, webapp) = %+v, %v; want version %d", ws, p, err, version)
		}
	}
	if _, err := c.Policy(ctx, "gone", "webapp"); err == nil {
		t.Error("a policy of a workspace that does not exist was read")
	}
	if _, err := c.Profiles(ctx, []string{"default", "team"}); err != nil {
		t.Fatal(err)
	}
	g.mu.Lock()
	scopes := slices.Clone(g.profileScopes)
	g.mu.Unlock()
	if !reflect.DeepEqual(scopes, []string{"default", "team"}) {
		t.Errorf("profiles were asked for the workspaces %v", scopes)
	}
	d, err := c.ListDir(ctx, "team", "webapp", "/sandbox")
	if err != nil || len(d.Files) != 1 || d.Files[0].Name != "team-only" {
		t.Errorf("ListDir in team = %+v, %v", d, err)
	}
	if got := lastExec.Load().GetWorkspaceScope().GetWorkspace(); got != "team" {
		t.Errorf("ListDir asked workspace %q", got)
	}
}

func TestPolicyYAML(t *testing.T) {
	doc, err := policyDoc(testPolicy())
	if err != nil {
		t.Fatal(err)
	}
	got, err := doc.yaml()
	if err != nil {
		t.Fatal(err)
	}
	want := `version: 1
filesystem_policy:
  include_workdir: false
  read_only:
    - /usr
    - /etc
  read_write:
    - /sandbox
landlock:
  compatibility: best_effort
process:
  run_as_user: sandbox
network_policies:
  _provider_github:
    name: _provider_github
    endpoints:
      - host: api.github.com
        port: 443
      - host: github.com
        ports:
          - 22
          - 443
        rules:
          - allow:
              method: GET
              path: /**
    binaries:
      - path: /usr/bin/gh
  pypi:
    name: pypi
    endpoints:
      - host: pypi.org
        port: 443
        enforcement: audit
        access: read-only
    binaries:
      - path: /usr/bin/pip
`
	if got != want {
		t.Errorf("got\n%s\nwant\n%s", got, want)
	}
	base, err := doc.base().yaml()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(base, "_provider_") || !strings.Contains(base, "pypi:") {
		t.Errorf("base:\n%s", base)
	}
}

func TestDialWithoutBundle(t *testing.T) {
	_, err := Dial(42443, t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "brig start copies it") {
		t.Errorf("err = %v", err)
	}
}

func TestBundleChanged(t *testing.T) {
	c := serve(t, &gateway{pageSize: 10})
	if c.BundleChanged() {
		t.Fatal("changed right after Dial")
	}
	later := time.Now().Add(time.Minute)
	if err := os.Chtimes(filepath.Join(c.dir, "tls.crt"), later, later); err != nil {
		t.Fatal(err)
	}
	if !c.BundleChanged() {
		t.Error("a newer tls.crt went unnoticed")
	}
}

func TestUntrustedGateway(t *testing.T) {
	c := serve(t, &gateway{pageSize: 10})
	// A bundle whose CA did not sign the gateway's certificate.
	other, _ := pki(t)
	bad, err := Dial(portOf(c), other)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = bad.Close() }()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := bad.Version(ctx); err == nil {
		t.Error("a gateway that the bundle's CA did not sign answered")
	}
}

func portOf(c *Client) int {
	_, port, _ := net.SplitHostPort(strings.TrimPrefix(c.conn.Target(), "passthrough:///"))
	p, _ := strconv.Atoi(port)
	return p
}

func TestListDir(t *testing.T) {
	c := serve(t, &gateway{})
	ctx := context.Background()
	d, err := c.ListDir(ctx, "default", "webapp", "/sandbox/src")
	if err != nil {
		t.Fatal(err)
	}
	want := []store.File{
		{Name: ".git", Kind: "dir", Size: 4096, Mod: time.Unix(1760000000, 0)},
		{Name: "README.md", Kind: "file", Size: 12, Mod: time.Unix(1760000100, 0)},
		{Name: "latest", Kind: "link", Size: 9, Mod: time.Unix(1760000200, 0)},
		{Name: "my dir\nx", Kind: "dir", Size: 4096, Mod: time.Unix(1760000300, 0)},
		{Name: "sock", Kind: "other", Mod: time.Unix(1760000400, 0)},
	}
	if !reflect.DeepEqual(d.Files, want) || d.Truncated {
		t.Errorf("ListDir = %+v, want %+v", d, want)
	}
	// The sandbox runs find with the directory as one argument, and no
	// shell sees it.
	req := lastExec.Load()
	if got := req.GetCommand(); !reflect.DeepEqual(got, ListCommand("/sandbox/src")) || got[0] != "find" || got[1] != "/sandbox/src" {
		t.Errorf("ran %q", got)
	}
	if req.GetSandbox() != "webapp" || !req.GetNoLoginShell() || req.GetTty() || req.GetStdin() != nil {
		t.Errorf("request: %+v", req)
	}

	if d, err := c.ListDir(ctx, "default", "empty", "/sandbox"); err != nil || len(d.Files) != 0 {
		t.Errorf("empty: %+v, %v", d, err)
	}
	if _, err := c.ListDir(ctx, "default", "nodir", "/sandbox/x"); err == nil || !strings.Contains(err.Error(), "No such file or directory") {
		t.Errorf("failed listing: %v", err)
	}
	if _, err := c.ListDir(ctx, "default", "garbled", "/sandbox"); err == nil {
		t.Error("a garbled listing was accepted")
	}
	if _, err := c.ListDir(ctx, "default", "silent", "/sandbox"); err == nil || !strings.Contains(err.Error(), "exit status") {
		t.Errorf("a listing without exit status: %v", err)
	}
	if d, err := c.ListDir(ctx, "default", "huge", "/sandbox"); err != nil || len(d.Files) != maxFiles || !d.Truncated {
		t.Errorf("huge: %d files, truncated %v, %v", len(d.Files), d.Truncated, err)
	}
}

func TestExec(t *testing.T) {
	c := serve(t, &gateway{})
	ctx := context.Background()
	var stdout, stderr strings.Builder
	code, err := c.Exec(ctx, ExecRequest{
		Workspace: "default", Sandbox: "task", Command: []string{"make", "test"}, Workdir: "task",
		Env: map[string]string{"CI": "1"}, Timeout: 90 * time.Second, Stdin: []byte("in"),
	}, &stdout, &stderr)
	if err != nil || code != 3 || stdout.String() != "output" || stderr.String() != "err" {
		t.Errorf("Exec = %d, %v; stdout %q, stderr %q", code, err, stdout.String(), stderr.String())
	}
	req := lastExec.Load()
	if !reflect.DeepEqual(req.GetCommand(), []string{"make", "test"}) || req.GetWorkdir() != "task" || req.GetEnvironment()["CI"] != "1" ||
		req.GetExecutionTimeout().AsDuration() != 90*time.Second || string(req.GetStdin()) != "in" || req.GetNoLoginShell() || req.GetTty() {
		t.Errorf("request: %+v", req)
	}
	if _, err := c.Exec(ctx, ExecRequest{Workspace: "default", Sandbox: "silent", Command: []string{"true"}}, &stdout, &stderr); err == nil {
		t.Error("a command without exit status succeeded")
	}
	if _, err := c.Exec(ctx, ExecRequest{Workspace: "default", Sandbox: "task", Stdin: make([]byte, MaxStdin+1)}, &stdout, &stderr); err == nil {
		t.Error("Exec took more standard input than the gateway does")
	}
}

func TestDeleteSandbox(t *testing.T) {
	orig := deleteRetry
	deleteRetry = time.Millisecond
	t.Cleanup(func() { deleteRetry = orig })
	g := &gateway{deleteBusy: 2}
	c := serve(t, g)
	if err := c.DeleteSandbox(context.Background(), "team", "run-1"); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(g.deleted, []string{"team/run-1"}) {
		t.Errorf("deleted %q", g.deleted)
	}
}
