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
	"google.golang.org/grpc/credentials"
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

func checkScope(ws *dm.WorkspaceSelector) error {
	if ws.GetWorkspace() != "default" {
		return errWorkspace
	}
	return nil
}

var errWorkspace = &net.AddrError{Err: "not the default workspace"}

func (g *gateway) Health(context.Context, *pb.HealthRequest) (*pb.HealthResponse, error) {
	return &pb.HealthResponse{Status: pb.ServiceStatus_SERVICE_STATUS_HEALTHY, Version: "0.1.3"}, nil
}

var created = time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)

func (g *gateway) ListSandboxes(_ context.Context, r *pb.ListSandboxesRequest) (*pb.ListSandboxesResponse, error) {
	if err := checkScope(r.GetWorkspaceScope()); err != nil {
		return nil, err
	}
	all := []*pb.Sandbox{
		{
			Metadata: &dm.ObjectMeta{Name: "webapp", CreatedTime: timestamppb.New(created)},
			Spec: &pb.SandboxSpec{
				Template:    &pb.SandboxTemplate{Image: "localhost/pi-agent:2026-10-08"},
				Providers:   []string{"llm", "github"},
				Environment: map[string]string{"SECRET": "s3cret"},
			},
			Status:                      &pb.SandboxStatus{Phase: pb.SandboxPhase_SANDBOX_PHASE_READY, CurrentPolicyVersion: 3},
			CreatedFromWorkloadTemplate: &pb.SandboxWorkloadTemplateProvenance{Name: "pi"},
		},
		{
			Metadata: &dm.ObjectMeta{Name: "broken"},
			Status: &pb.SandboxStatus{Phase: pb.SandboxPhase_SANDBOX_PHASE_ERROR, Conditions: []*pb.SandboxCondition{
				{Type: "Ready", Status: "False", Message: "image pull failed"},
			}},
		},
		{Metadata: &dm.ObjectMeta{Name: "odd"}, Status: &pb.SandboxStatus{Phase: 42}},
	}
	items, next := page(all, r.GetPageToken(), g.pageSize)
	return &pb.ListSandboxesResponse{Sandboxes: items, NextPageToken: next}, nil
}

func (g *gateway) ListProviders(_ context.Context, r *pb.ListProvidersRequest) (*pb.ListProvidersResponse, error) {
	if err := checkScope(r.GetWorkspaceScope()); err != nil {
		return nil, err
	}
	all := []*dm.Provider{
		{Metadata: &dm.ObjectMeta{Name: "llm"}, Type: "openai", Credentials: map[string]string{"OPENAI_API_KEY": "sk-not-redacted"}},
		{Metadata: &dm.ObjectMeta{Name: "github"}, Type: "github", Credentials: map[string]string{"GH_TOKEN": "REDACTED", "GH_HOST": "REDACTED"}},
	}
	items, next := page(all, r.GetPageToken(), g.pageSize)
	return &pb.ListProvidersResponse{Providers: items, NextPageToken: next}, nil
}

func (g *gateway) ListProviderProfiles(_ context.Context, r *pb.ListProviderProfilesRequest) (*pb.ListProviderProfilesResponse, error) {
	if err := checkScope(r.GetWorkspaceScope()); err != nil {
		return nil, err
	}
	return &pb.ListProviderProfilesResponse{Profiles: []*pb.ProviderProfile{{
		Id: "github", DisplayName: "GitHub", Category: pb.ProviderProfileCategory_PROVIDER_PROFILE_CATEGORY_SOURCE_CONTROL,
		Source: "user", Scope: "workspace",
		Credentials: []*pb.ProviderProfileCredential{{Name: "token", EnvVars: []string{"GH_TOKEN"}, AuthStyle: "bearer", Required: true}},
		Endpoints: []*sb.NetworkEndpoint{
			{Host: "api.github.com", Port: 443},
			{Host: "github.com", Ports: []uint32{22, 443}},
		},
		Binaries: []*sb.NetworkBinary{{Path: "/usr/bin/gh"}},
	}}}, nil
}

func (g *gateway) ListSandboxTemplates(_ context.Context, r *pb.ListSandboxTemplatesRequest) (*pb.ListSandboxTemplatesResponse, error) {
	if err := checkScope(r.GetWorkspaceScope()); err != nil {
		return nil, err
	}
	return &pb.ListSandboxTemplatesResponse{Templates: []*pb.SandboxWorkloadTemplate{{
		Metadata: &dm.ObjectMeta{Name: "pi"},
		Spec: &pb.SandboxWorkloadTemplateSpec{Workload: &pb.SandboxWorkloadConfig{
			Image: "localhost/pi-agent:2026-10-08", Resources: &pb.SandboxResources{Cpu: "2", Memory: "4Gi"},
		}},
	}}}, nil
}

func (g *gateway) GetSandboxConfig(_ context.Context, r *sb.GetSandboxConfigRequest) (*sb.GetSandboxConfigResponse, error) {
	if err := checkScope(r.GetWorkspaceScope()); err != nil {
		return nil, err
	}
	return &sb.GetSandboxConfigResponse{Version: 3, PolicySource: sb.PolicySource_POLICY_SOURCE_SANDBOX, Policy: testPolicy()}, nil
}

// lastExec is what the fake gateway was last asked to run.
var lastExec atomic.Pointer[pb.ExecSandboxRequest]

func (g *gateway) ExecSandbox(r *pb.ExecSandboxRequest, stream grpc.ServerStreamingServer[pb.ExecSandboxEvent]) error {
	if err := checkScope(r.GetWorkspaceScope()); err != nil {
		return err
	}
	lastExec.Store(r)
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
func serve(t *testing.T, g *gateway) *Client {
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

	sbs, err := c.Sandboxes(ctx)
	if err != nil {
		t.Fatal(err)
	}
	want := []store.Sandbox{
		{Name: "broken", Phase: "Error", Problem: "image pull failed"},
		{Name: "odd", Phase: "Unknown"},
		{
			Name: "webapp", Phase: "Ready", Image: "localhost/pi-agent:2026-10-08", Template: "pi",
			Providers: []string{"github", "llm"}, Created: created, PolicyVersion: 3,
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
		{Name: "github", Type: "github", Credentials: []string{"GH_HOST", "GH_TOKEN"}},
		{Name: "llm", Type: "openai", Credentials: []string{"OPENAI_API_KEY"}},
	}
	if !reflect.DeepEqual(provs, wantProvs) {
		t.Errorf("Providers:\ngot  %+v\nwant %+v", provs, wantProvs)
	}

	profs, err := c.Profiles(ctx)
	if err != nil {
		t.Fatal(err)
	}
	wantProfs := []store.Profile{{
		ID: "github", Name: "GitHub", Category: "source control", Source: "user", Scope: "workspace",
		Credentials: []store.ProfileCredential{{Name: "token", EnvVars: []string{"GH_TOKEN"}, Auth: "bearer", Required: true}},
		Endpoints:   []string{"api.github.com:443", "github.com:22", "github.com:443"},
		Binaries:    []string{"/usr/bin/gh"},
	}}
	if !reflect.DeepEqual(profs, wantProfs) {
		t.Errorf("Profiles:\ngot  %+v\nwant %+v", profs, wantProfs)
	}

	tmpls, err := c.Templates(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if want := []store.Template{{Name: "pi", Image: "localhost/pi-agent:2026-10-08", CPU: "2", Memory: "4Gi"}}; !reflect.DeepEqual(tmpls, want) {
		t.Errorf("Templates: %+v", tmpls)
	}

	p, err := c.Policy(ctx, "webapp")
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
	d, err := c.ListDir(ctx, "webapp", "/sandbox/src")
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

	if d, err := c.ListDir(ctx, "empty", "/sandbox"); err != nil || len(d.Files) != 0 {
		t.Errorf("empty: %+v, %v", d, err)
	}
	if _, err := c.ListDir(ctx, "nodir", "/sandbox/x"); err == nil || !strings.Contains(err.Error(), "No such file or directory") {
		t.Errorf("failed listing: %v", err)
	}
	if _, err := c.ListDir(ctx, "garbled", "/sandbox"); err == nil {
		t.Error("a garbled listing was accepted")
	}
	if _, err := c.ListDir(ctx, "silent", "/sandbox"); err == nil || !strings.Contains(err.Error(), "exit status") {
		t.Errorf("a listing without exit status: %v", err)
	}
	if d, err := c.ListDir(ctx, "huge", "/sandbox"); err != nil || len(d.Files) != maxFiles || !d.Truncated {
		t.Errorf("huge: %d files, truncated %v, %v", len(d.Files), d.Truncated, err)
	}
}
