// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

package llmproxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dennisklein/brig/internal/osclient"
	"github.com/dennisklein/brig/internal/service"
)

const testMaster = "0123456789abcdef0123456789abcdef"

// fakeGateway is the service VM's gateway with or without the LiteLLM
// sandbox.
type fakeGateway struct {
	mu        sync.Mutex
	rec       *osclient.SandboxRecord
	providers []string
	created   *osclient.CreateRequest
	calls     []string
	stdin     []string
	// healthy fails the health check this many times first.
	unhealthy int
	loadOut   string
	closed    bool
	// keys is the address of LiteLLM's key API, which ForwardTCP reaches.
	keys string
}

func (g *fakeGateway) Sandbox(_ context.Context, ws, name string) (osclient.SandboxRecord, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.rec == nil || ws != Workspace || name != Sandbox {
		return osclient.SandboxRecord{}, osclient.ErrNoSandbox
	}
	spec, _ := json.Marshal(map[string]any{"providers": g.providers})
	r := *g.rec
	r.Spec = spec
	// Starting and stopping take one look.
	switch r.Phase {
	case "Starting", "Provisioning":
		g.rec.Phase = "Ready"
	case "Stopping":
		g.rec.Phase = "Stopped"
	}
	return r, nil
}

func (g *fakeGateway) CreateSandbox(_ context.Context, r osclient.CreateRequest) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.calls = append(g.calls, "create")
	g.created = &r
	var spec struct {
		Template  struct{ Image string } `json:"template"`
		Providers []string               `json:"providers"`
	}
	if err := json.Unmarshal(r.Spec, &spec); err != nil {
		return err
	}
	g.rec = &osclient.SandboxRecord{Workspace: r.Workspace, Name: r.Name, Phase: "Provisioning", Image: spec.Template.Image}
	g.providers = spec.Providers
	return nil
}

func (g *fakeGateway) StartSandbox(context.Context, string, string) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.calls = append(g.calls, "start")
	g.rec.Phase = "Starting"
	return nil
}

func (g *fakeGateway) StopSandbox(context.Context, string, string) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.calls = append(g.calls, "stop")
	g.rec.Phase = "Stopping"
	return nil
}

func (g *fakeGateway) AttachProvider(_ context.Context, _, _, p string) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.calls = append(g.calls, "attach "+p)
	g.providers = append(g.providers, p)
	return nil
}

func (g *fakeGateway) DetachProvider(_ context.Context, _, _, p string) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.calls = append(g.calls, "detach "+p)
	g.providers = slices.DeleteFunc(g.providers, func(q string) bool { return q == p })
	return nil
}

func (g *fakeGateway) Exec(_ context.Context, r osclient.ExecRequest, stdout, _ io.Writer) (int, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.rec == nil || g.rec.Phase != "Ready" {
		return 0, errors.New("sandbox not ready")
	}
	switch r.Command[0] {
	case "llm-proxy-load":
		g.calls = append(g.calls, "load")
		g.stdin = append(g.stdin, string(r.Stdin))
		_, _ = io.WriteString(stdout, g.loadOut)
		return 0, nil
	case "python3":
		g.calls = append(g.calls, "health")
		if g.unhealthy > 0 {
			g.unhealthy--
			return 1, nil
		}
		return 0, nil
	}
	return 127, nil
}

func (g *fakeGateway) ForwardTCP(ctx context.Context, ws, sandbox string, port uint32) (io.ReadWriteCloser, error) {
	if g.keys == "" || ws != Workspace || sandbox != Sandbox || port != Port {
		return nil, errors.New("connection refused")
	}
	var d net.Dialer
	return d.DialContext(ctx, "tcp", g.keys)
}

func (g *fakeGateway) Close() error { g.closed = true; return nil }

func testEnv(secrets map[string]string) service.Env {
	return service.Env{
		Service: service.Service{Name: "llm-proxy", Type: "llm-proxy", Placement: service.PlacementVM, Port: 41000},
		Secret: func(_ context.Context, name string) (string, error) {
			if s, ok := secrets[name]; ok {
				return s, nil
			}
			return "", fmt.Errorf("no secret %s", name)
		},
	}
}

func testDriver(t *testing.T, gw *fakeGateway, providers ...string) (*Driver, *bytes.Buffer, *[]string) {
	t.Helper()
	if gw.keys == "" {
		_, gw.keys = serveKeys(t)
	}
	cfg, err := ParseConfig([]byte(`
models:
  - model_name: claude-*
    litellm_params: {model: anthropic/claude-*}
keys: {models: [claude-*], max_budget: 5, budget_duration: 30d}
`), []string{"ANTHROPIC_AUTH_TOKEN"})
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	var images []string
	return &Driver{
		Open:  func(context.Context, service.Env) (Gateway, error) { return gw, nil },
		Setup: func(service.Env) (Setup, error) { return Setup{Config: cfg, Providers: providers}, nil },
		Image: func(_ context.Context, _ service.Env, ref string) error { images = append(images, ref); return nil },
		Attachments: func(service.Env) ([]service.Attachment, error) {
			return nil, nil
		},
		Out:     &out,
		Timeout: 2 * time.Second,
		Poll:    time.Millisecond,
	}, &out, &images
}

var secrets = map[string]string{MasterKeySecret: testMaster, SaltKeySecret: "fedcba9876543210fedcba9876543210"}

func TestStartedCreates(t *testing.T) {
	gw := &fakeGateway{unhealthy: 2}
	d, out, images := testDriver(t, gw, "anthropic", "claude-code")
	if err := d.Started(context.Background(), testEnv(secrets)); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(*images, []string{ImageRef()}) {
		t.Errorf("images = %q", *images)
	}
	if want := []string{"create", "load", "health", "health", "health"}; !slices.Equal(gw.calls, want) {
		t.Errorf("calls = %q, want %q", gw.calls, want)
	}
	if gw.rec.Image != ImageRef() || !slices.Equal(gw.providers, []string{"anthropic", "claude-code"}) {
		t.Errorf("sandbox = %+v with %q", gw.rec, gw.providers)
	}
	if gw.created.Policy == nil || gw.created.Labels["brig.service"] != "llm-proxy" {
		t.Errorf("create request = %+v", gw.created)
	}
	in := gw.stdin[0]
	keys, config, ok := strings.Cut(in, "---\n")
	if !ok || keys != "LITELLM_MASTER_KEY="+testMaster+"\nLITELLM_SALT_KEY="+secrets[SaltKeySecret]+"\n" {
		t.Errorf("keys part = %q", keys)
	}
	if !strings.Contains(config, "model_list:") || !strings.Contains(config, "master_key: os.environ/LITELLM_MASTER_KEY") {
		t.Errorf("config part:\n%s", config)
	}
	if !strings.Contains(out.String(), "Creating sandbox litellm") || !gw.closed {
		t.Errorf("out = %q, closed = %v", out, gw.closed)
	}
}

func TestStartedReconciles(t *testing.T) {
	gw := &fakeGateway{
		rec:       &osclient.SandboxRecord{Workspace: Workspace, Name: Sandbox, Phase: "Ready", Image: "localhost/brig-llm-proxy:old"},
		providers: []string{"anthropic", "openrouter"}, loadOut: "reloaded\n",
	}
	d, out, _ := testDriver(t, gw, "anthropic", "claude-code")
	if err := d.Started(context.Background(), testEnv(secrets)); err != nil {
		t.Fatal(err)
	}
	if want := []string{"attach claude-code", "detach openrouter", "stop", "start", "load", "health"}; !slices.Equal(gw.calls, want) {
		t.Errorf("calls = %q, want %q", gw.calls, want)
	}
	for _, want := range []string{"runs localhost/brig-llm-proxy:old", "Restarting sandbox", "loads its new config"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("out lacks %q:\n%s", want, out)
		}
	}

	// Nothing to change, but a stopped sandbox starts.
	gw.calls = nil
	gw.rec.Phase = "Stopped"
	if err := d.Started(context.Background(), testEnv(secrets)); err != nil {
		t.Fatal(err)
	}
	if want := []string{"start", "load", "health"}; !slices.Equal(gw.calls, want) {
		t.Errorf("calls = %q, want %q", gw.calls, want)
	}
}

func TestStartedRefusesBadSecrets(t *testing.T) {
	for name, s := range map[string]map[string]string{
		"missing": {MasterKeySecret: testMaster},
		"short":   {MasterKeySecret: "abc", SaltKeySecret: testMaster},
		"newline": {MasterKeySecret: testMaster + "\nEVIL=1", SaltKeySecret: testMaster},
	} {
		gw := &fakeGateway{}
		d, _, _ := testDriver(t, gw)
		err := d.Started(context.Background(), testEnv(s))
		if err == nil || len(gw.calls) != 0 {
			t.Errorf("%s: err = %v, calls = %q", name, err, gw.calls)
		}
		if err != nil && strings.Contains(err.Error(), testMaster) {
			t.Errorf("%s: error repeats the secret: %v", name, err)
		}
	}
}

func TestStartedTimesOut(t *testing.T) {
	gw := &fakeGateway{unhealthy: 1 << 30}
	d, _, _ := testDriver(t, gw)
	d.Timeout = 20 * time.Millisecond
	if err := d.Started(context.Background(), testEnv(secrets)); err == nil || !strings.Contains(err.Error(), "waiting for LiteLLM") {
		t.Fatalf("err = %v", err)
	}
}

// keyServer is LiteLLM's key API, as far as the driver uses it.
type keyServer struct {
	mu   sync.Mutex
	keys map[string]map[string]any
}

func (k *keyServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if r.Header.Get("Authorization") != "Bearer "+testMaster {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	switch r.URL.Path {
	case "/key/generate":
		var req map[string]any
		_ = json.NewDecoder(r.Body).Decode(&req)
		alias := req["key_alias"].(string)
		if _, ok := k.keys[alias]; ok {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		k.keys[alias] = req
		_ = json.NewEncoder(w).Encode(map[string]string{"key": "sk-" + strings.ReplaceAll(alias, "/", "-")})
	case "/key/delete":
		var req struct {
			KeyAliases []string `json:"key_aliases"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		n := 0
		for _, a := range req.KeyAliases {
			if _, ok := k.keys[a]; ok {
				delete(k.keys, a)
				n++
			}
		}
		if n == 0 {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_, _ = w.Write([]byte(`{}`))
	case "/key/list":
		var keys []map[string]string
		for a := range k.keys {
			keys = append(keys, map[string]string{"key_alias": a})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"keys": keys, "total_pages": 1})
	default:
		http.NotFound(w, r)
	}
}

func serveKeys(t *testing.T) (*keyServer, string) {
	k := &keyServer{keys: map[string]map[string]any{}}
	srv := httptest.NewServer(k)
	t.Cleanup(srv.Close)
	return k, srv.Listener.Addr().String()
}

func TestIssueRevokePrune(t *testing.T) {
	k, addr := serveKeys(t)
	gw := &fakeGateway{keys: addr, rec: &osclient.SandboxRecord{Workspace: Workspace, Name: Sandbox, Phase: "Ready", Image: ImageRef()}}
	d, out, _ := testDriver(t, gw)
	env := testEnv(secrets)
	ctx := context.Background()
	a := service.Attachment{Gateway: "dev", Workspace: "default", Sandbox: "webapp"}
	tok, err := d.Issue(ctx, env, &a)
	if err != nil || tok != "sk-brig-dev-default-webapp" {
		t.Fatalf("Issue = %q, %v", tok, err)
	}
	req := k.keys["brig/dev/default/webapp"]
	if req["max_budget"] != 5.0 || req["budget_duration"] != "30d" || fmt.Sprint(req["models"]) != "[claude-*]" {
		t.Errorf("key request = %v", req)
	}
	if md, _ := req["metadata"].(map[string]any); md["brig_sandbox"] != "webapp" {
		t.Errorf("metadata = %v", req["metadata"])
	}
	if a.SHA256 != "" {
		t.Errorf("SHA256 = %q; LiteLLM checks its own keys", a.SHA256)
	}
	// Again: the old key goes, a new one comes.
	if _, err := d.Issue(ctx, env, &a); err != nil {
		t.Fatal(err)
	}
	b := service.Attachment{Gateway: "dev", Workspace: "team", Sandbox: "api"}
	if _, err := d.Issue(ctx, env, &b); err != nil {
		t.Fatal(err)
	}
	k.keys["brig/old/default/gone"] = map[string]any{}
	k.keys["made-in-the-ui"] = map[string]any{}
	d.Attachments = func(service.Env) ([]service.Attachment, error) { return []service.Attachment{a, b}, nil }
	if err := d.Started(ctx, env); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "Deleted LiteLLM key brig/old/default/gone") || len(k.keys) != 3 {
		t.Fatalf("Started pruned to %v:\n%s", k.keys, out)
	}
	if err := d.Revoke(ctx, env, a); err != nil {
		t.Fatal(err)
	}
	if err := d.Revoke(ctx, env, a); err != nil {
		t.Fatalf("Revoke of a gone key: %v", err)
	}
	if _, ok := k.keys["made-in-the-ui"]; !ok || len(k.keys) != 2 {
		t.Errorf("keys left = %v", k.keys)
	}
	if _, err := d.Issue(ctx, testEnv(map[string]string{MasterKeySecret: strings.Repeat("x", 32)}), &a); err == nil || !strings.Contains(err.Error(), "master key") {
		t.Errorf("Issue with a wrong master key: %v", err)
	}
}
