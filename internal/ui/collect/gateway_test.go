// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

package collect

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/dennisklein/brig/internal/ui/store"
	"github.com/dennisklein/brig/internal/vm"
)

// fakeGateway is a gateway's state that fake clients read.
type fakeGateway struct {
	mu        sync.Mutex
	sandboxes []store.Sandbox
	// policyReads counts Policy calls by sandbox.
	policyReads map[string]int
	// fail makes every call fail.
	fail error
	// bundleChanged makes BundleChanged report a change once.
	bundleChanged atomic.Bool
	dials         atomic.Int32
}

type fakeClient struct{ g *fakeGateway }

func (c fakeClient) err() error {
	c.g.mu.Lock()
	defer c.g.mu.Unlock()
	return c.g.fail
}

func (c fakeClient) Version(context.Context) (string, error) {
	if err := c.err(); err != nil {
		return "", err
	}
	return "0.1.3", nil
}

func (c fakeClient) Sandboxes(context.Context) ([]store.Sandbox, error) {
	if err := c.err(); err != nil {
		return nil, err
	}
	c.g.mu.Lock()
	defer c.g.mu.Unlock()
	out := make([]store.Sandbox, len(c.g.sandboxes))
	copy(out, c.g.sandboxes)
	return out, nil
}

func (c fakeClient) Providers(context.Context) ([]store.Provider, error) {
	return []store.Provider{{Name: "github", Type: "github", Credentials: []string{"GH_TOKEN"}}}, c.err()
}

func (c fakeClient) Profiles(context.Context) ([]store.Profile, error) {
	return []store.Profile{{ID: "github"}}, c.err()
}

func (c fakeClient) Templates(context.Context) ([]store.Template, error) {
	return []store.Template{{Name: "pi"}}, c.err()
}

func (c fakeClient) Policy(_ context.Context, name string) (*store.Policy, error) {
	c.g.mu.Lock()
	defer c.g.mu.Unlock()
	c.g.policyReads[name]++
	if name == "gone" {
		return nil, status.Error(codes.NotFound, "sandbox gone not found")
	}
	return &store.Policy{Effective: "version: 1\n", Base: "version: 1\n"}, nil
}

func (c fakeClient) BundleChanged() bool { return c.g.bundleChanged.Swap(false) }
func (c fakeClient) Close() error        { return nil }

func (g *fakeGateway) reads(name string) int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.policyReads[name]
}

func (g *fakeGateway) set(f func(*fakeGateway)) {
	g.mu.Lock()
	defer g.mu.Unlock()
	f(g)
}

// waitFor waits until cond holds for the store's gateway of dev.
func waitFor(t *testing.T, st *store.Store, what string, cond func(store.Gateway) bool) store.Gateway {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		gw, ok := st.Snapshot().Gateway("dev")
		if ok && cond(gw) {
			return gw
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s; gateway: %+v", what, gw)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestGateways(t *testing.T) {
	cfgDir := t.TempDir()
	for path, data := range map[string]string{
		"providers/github.yaml": "name: github\ntype: github\ncredentials:\n  GH_TOKEN:\n    secret_tool:\n      lookup: [service, github.com]\n",
		"policies/default.yaml": "version: 1\nnetwork_policies: {}\n",
		"policies/webapp.yaml":  "version: 1\n",
	} {
		p := filepath.Join(cfgDir, path)
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	fake := &fakeGateway{
		policyReads: map[string]int{},
		sandboxes:   []store.Sandbox{{Name: "gone", PolicyVersion: 1}, {Name: "webapp", Phase: "Ready", PolicyVersion: 1}},
	}
	g := &Gateways{
		Dial: func(v store.VM) (GatewayClient, error) {
			if v.Ports.Gateway != 42443 {
				t.Errorf("dialed port %d", v.Ports.Gateway)
			}
			fake.dials.Add(1)
			return fakeClient{fake}, nil
		},
		SandboxesEvery: 5 * time.Millisecond, RestEvery: time.Hour, Backoff: 5 * time.Millisecond,
	}
	st := store.New()
	dev := store.VM{VM: vm.VM{Name: "dev", Ports: vm.Ports{Gateway: 42443}, OpenShellConfigs: []string{cfgDir}}, State: "running"}
	st.SetVMs(store.VMState{VMs: []store.VM{dev}})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { g.Run(ctx, st); close(done) }()
	defer func() { cancel(); <-done }()

	gw := waitFor(t, st, "the gateway to be read", func(gw store.Gateway) bool { return gw.State == store.GatewayReady })
	if gw.Version != "0.1.3" || len(gw.Providers) != 1 || len(gw.Profiles) != 1 || len(gw.Templates) != 1 || gw.Seen.IsZero() {
		t.Errorf("gateway: %+v", gw)
	}
	if len(gw.Sandboxes) != 2 || gw.Sandboxes[1].Policy == nil || gw.Sandboxes[1].Policy.Effective != "version: 1\n" {
		t.Errorf("sandboxes: %+v", gw.Sandboxes)
	}
	if p := gw.Sandboxes[0].Policy; p == nil || p.Err != "sandbox gone not found" {
		t.Errorf("policy of a sandbox gone meanwhile: %+v", p)
	}
	cfg := gw.Config
	if cfg.Err != "" || cfg.Providers["github"] != filepath.Join(cfgDir, "providers/github.yaml") ||
		cfg.DefaultPolicy != filepath.Join(cfgDir, "policies/default.yaml") || cfg.DefaultPolicyText != "version: 1\nnetwork_policies: {}\n" ||
		len(cfg.Policies) != 2 {
		t.Errorf("config: %+v", cfg)
	}

	// A policy is read again only when its version changes.
	time.Sleep(30 * time.Millisecond)
	if n := fake.reads("webapp"); n != 1 {
		t.Errorf("read the unchanged policy %d times", n)
	}
	fake.set(func(f *fakeGateway) { f.sandboxes[1].PolicyVersion = 2 })
	waitFor(t, st, "the new policy version", func(gw store.Gateway) bool {
		return len(gw.Sandboxes) == 2 && gw.Sandboxes[1].PolicyVersion == 2
	})
	if n := fake.reads("webapp"); n != 2 {
		t.Errorf("read the changed policy %d times in all", n)
	}

	// A new client bundle means dialing again.
	dials := fake.dials.Load()
	fake.bundleChanged.Store(true)
	deadline := time.Now().Add(5 * time.Second)
	for fake.dials.Load() == dials && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if fake.dials.Load() == dials {
		t.Error("did not dial again after the bundle changed")
	}

	// A gateway that stops answering is unreachable, and back when it
	// answers again.
	fake.set(func(f *fakeGateway) {
		f.fail = status.Error(codes.Unavailable, "connection error: connect: connection refused")
	})
	gw = waitFor(t, st, "the gateway to be unreachable", func(gw store.Gateway) bool { return gw.State == store.GatewayUnreachable })
	if want := "brig-dev does not answer on 127.0.0.1:42443: connection error: connect: connection refused"; gw.Err != want {
		t.Errorf("Err = %q, want %q", gw.Err, want)
	}
	if len(gw.Sandboxes) != 2 {
		t.Error("an unreachable gateway lost what it held")
	}
	fake.set(func(f *fakeGateway) { f.fail = nil })
	waitFor(t, st, "the gateway to answer again", func(gw store.Gateway) bool { return gw.State == store.GatewayReady })

	// Stopping the VM keeps what its gateway held.
	dev.State = "shut off"
	st.SetVMs(store.VMState{VMs: []store.VM{dev}})
	gw = waitFor(t, st, "the gateway to stop", func(gw store.Gateway) bool { return gw.State == store.GatewayStopped })
	if len(gw.Sandboxes) != 2 || gw.Seen.IsZero() {
		t.Errorf("stopped gateway: %+v", gw)
	}

	// Deleting the VM drops its gateway.
	st.SetVMs(store.VMState{})
	deadline = time.Now().Add(5 * time.Second)
	for len(st.Snapshot().Gateways) != 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if n := len(st.Snapshot().Gateways); n != 0 {
		t.Errorf("%d gateways left", n)
	}
}

func TestGatewaysDialError(t *testing.T) {
	g := &Gateways{
		Dial: func(store.VM) (GatewayClient, error) {
			return nil, errors.New("no mTLS client bundle yet; brig start copies it from the VM")
		},
		Backoff: time.Millisecond,
	}
	st := store.New()
	st.SetVMs(store.VMState{VMs: []store.VM{{VM: vm.VM{Name: "dev", Ports: vm.Ports{Gateway: 42443}}, State: "running"}}})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { g.Run(ctx, st); close(done) }()
	defer func() { cancel(); <-done }()
	gw := waitFor(t, st, "the dial error", func(gw store.Gateway) bool { return gw.State == store.GatewayUnreachable })
	if !strings.Contains(gw.Err, "brig start copies it") || gw.Answered() {
		t.Errorf("gateway: %+v", gw)
	}
}

func TestDescribe(t *testing.T) {
	v := store.VM{VM: vm.VM{Name: "dev", Ports: vm.Ports{Gateway: 42443}}}
	err := status.Error(codes.Unavailable, "connection error: desc = \"transport: authentication handshake failed: tls: failed to verify certificate\"")
	if got := describe(v, err); !strings.HasPrefix(got, "brig ui and brig-dev do not trust each other") || !strings.HasSuffix(got, "brig start dev fetches the gateway's client bundle again.") {
		t.Errorf("handshake: %q", got)
	}
	if got := describe(v, status.Error(codes.PermissionDenied, "no")); got != "no" {
		t.Errorf("denied: %q", got)
	}
}

func TestReadConfig(t *testing.T) {
	if cfg := readConfig(nil); cfg.Err != "" || cfg.Providers != nil {
		t.Errorf("no dirs: %+v", cfg)
	}
	if cfg := readConfig([]string{"/nonexistent"}); cfg.Err == "" {
		t.Error("a missing directory is no error")
	}
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "policies"), 0o700); err != nil {
		t.Fatal(err)
	}
	big := strings.Repeat("#\n", maxPolicyFile)
	if err := os.WriteFile(filepath.Join(dir, "policies", "default.yaml"), []byte(big), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := readConfig([]string{dir})
	if len(cfg.DefaultPolicyText) != maxPolicyFile || !strings.Contains(cfg.Err, "showing its start") {
		t.Errorf("large default policy: %d bytes, %q", len(cfg.DefaultPolicyText), cfg.Err)
	}
}
