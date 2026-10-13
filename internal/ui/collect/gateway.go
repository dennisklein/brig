// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

package collect

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"reflect"
	"slices"
	"strings"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/dennisklein/brig/internal/ocsync"
	"github.com/dennisklein/brig/internal/openshell"
	"github.com/dennisklein/brig/internal/osclient"
	"github.com/dennisklein/brig/internal/ui/store"
)

const (
	// sandboxesEvery is how often a gateway's sandboxes are listed:
	// OpenShell has no call that streams changes to the list.
	sandboxesEvery = 5 * time.Second
	// restEvery is how often providers, profiles, templates, policies and
	// the config directories are read again.
	restEvery = 30 * time.Second
	// maxBackoff is the longest wait before dialing a gateway again.
	maxBackoff = 30 * time.Second
	// maxPolicyFile bounds the default policy file that is read to show.
	maxPolicyFile = 256 << 10
)

// GatewayClient reads one gateway; osclient.Client is one.
type GatewayClient interface {
	Version(ctx context.Context) (string, error)
	Workspaces(ctx context.Context) ([]string, error)
	// Sandboxes, Providers and Templates list every workspace.
	Sandboxes(ctx context.Context) ([]store.Sandbox, error)
	Providers(ctx context.Context) ([]store.Provider, error)
	Profiles(ctx context.Context, workspaces []string) ([]store.Profile, error)
	Templates(ctx context.Context) ([]store.Template, error)
	Policy(ctx context.Context, workspace, sandbox string) (*store.Policy, error)
	// Watch follows a sandbox's log until the stream or ctx ends.
	Watch(ctx context.Context, workspace, sandbox string, from osclient.WatchFrom, each func(osclient.WatchEvent)) error
	// Drafts lists the rules OpenShell drafted for a sandbox.
	Drafts(ctx context.Context, workspace, sandbox string) ([]store.Draft, error)
	BundleChanged() bool
	Close() error
}

// Gateways keeps the store's OpenShell gateways up to date: it reads each
// running VM's gateway over gRPC, and every VM's config directories.
type Gateways struct {
	// Dial returns a client of a VM's gateway.
	Dial func(v store.VM) (GatewayClient, error)
	// SandboxesEvery and RestEvery are the polling intervals; zero picks
	// the defaults.
	SandboxesEvery, RestEvery time.Duration
	// Backoff is the first wait before dialing a gateway again; zero picks
	// a second.
	Backoff time.Duration
	// FlushEvery, DraftsAfter and DraftsEvery time the traffic feed; zero
	// picks the defaults.
	FlushEvery, DraftsAfter, DraftsEvery time.Duration
	// Log tells why a sandbox's log or drafts cannot be read; nil logs
	// nothing.
	Log *slog.Logger
}

// NewGateways returns a collector that reaches each gateway with the mTLS
// bundle that brig keeps in the openshell CLI's config directory.
func NewGateways(configHome string) *Gateways {
	return &Gateways{Dial: func(v store.VM) (GatewayClient, error) {
		return osclient.Dial(v.Ports.Gateway, openshell.MTLSDir(configHome, v.GatewayName()))
	}}
}

// worker reads one VM's gateway until stopped.
type worker struct {
	port   int
	cancel context.CancelFunc
	done   chan struct{}
}

func (w *worker) stop() {
	w.cancel()
	<-w.done
}

// Run keeps the store's gateways up to date until ctx ends.
func (g *Gateways) Run(ctx context.Context, st *store.Store) {
	changes, unsubscribe := st.Subscribe()
	defer unsubscribe()
	workers := map[string]*worker{}
	defer func() {
		for _, w := range workers {
			w.stop()
		}
	}()
	dirs := map[string][]string{}
	tick := time.NewTicker(g.restEvery())
	defer tick.Stop()
	g.reconcile(ctx, st, workers, dirs, true)
	for {
		select {
		case <-ctx.Done():
			return
		case t := <-changes:
			if t != store.TopicVMs {
				continue
			}
			g.reconcile(ctx, st, workers, dirs, false)
		case <-tick.C:
			g.reconcile(ctx, st, workers, dirs, true)
		}
	}
}

// reconcile starts a worker for each running VM and stops the others, and
// reads the config directories of the VMs whose directories changed, or of
// all with reread.
func (g *Gateways) reconcile(ctx context.Context, st *store.Store, workers map[string]*worker, dirs map[string][]string, reread bool) {
	sn := st.Snapshot()
	names := map[string]bool{}
	for _, v := range sn.VMs {
		names[v.Name] = true
		if prev, ok := dirs[v.Name]; reread || !ok || !slices.Equal(prev, v.OpenShellConfigs) {
			dirs[v.Name] = slices.Clone(v.OpenShellConfigs)
			cfg := readConfig(v.OpenShellConfigs)
			st.UpdateGateway(v.Name, func(gw *store.Gateway) bool {
				if reflect.DeepEqual(gw.Config, cfg) {
					return false
				}
				gw.Config = cfg
				return true
			})
		}
		w := workers[v.Name]
		if w != nil && (!v.Running() || w.port != v.Ports.Gateway) {
			w.stop()
			delete(workers, v.Name)
			w = nil
			st.UpdateGateway(v.Name, func(gw *store.Gateway) bool {
				gw.State, gw.Err = store.GatewayStopped, ""
				return true
			})
		}
		if w == nil && v.Running() {
			workers[v.Name] = g.start(ctx, st, v)
		}
	}
	for name, w := range workers {
		if !names[name] {
			w.stop()
			delete(workers, name)
		}
	}
	for name := range dirs {
		if !names[name] {
			delete(dirs, name)
		}
	}
	st.RemoveGateways(func(vm string) bool { return names[vm] })
	st.RemoveTraffic(func(vm string) bool { return names[vm] })
}

// start starts a worker for the gateway of v.
func (g *Gateways) start(ctx context.Context, st *store.Store, v store.VM) *worker {
	ctx, cancel := context.WithCancel(ctx)
	w := &worker{port: v.Ports.Gateway, cancel: cancel, done: make(chan struct{})}
	go func() {
		defer close(w.done)
		g.work(ctx, st, v)
	}()
	return w
}

// errNoWorkspaces ends a connection to a gateway that predates workspaces.
var errNoWorkspaces = errors.New("the gateway is too old for brig ui: it has no workspaces")

// errBundleChanged ends a connection whose client bundle was replaced.
var errBundleChanged = errors.New("the mTLS client bundle changed")

// work reads the gateway of v until ctx ends, dialing again after an error,
// with a growing wait.
func (g *Gateways) work(ctx context.Context, st *store.Store, v store.VM) {
	st.UpdateGateway(v.Name, func(gw *store.Gateway) bool {
		gw.State, gw.Err = store.GatewayConnecting, ""
		return true
	})
	backoff := g.backoff()
	// positions are where the traffic feed is in each sandbox's log.
	positions := map[sandboxKey]*position{}
	for {
		c, err := g.Dial(v)
		if err == nil {
			var answered bool
			answered, err = g.serve(ctx, st, v.Name, c, positions)
			_ = c.Close()
			if answered {
				backoff = g.backoff()
			}
		}
		if ctx.Err() != nil {
			return
		}
		if errors.Is(err, errBundleChanged) {
			st.UpdateGateway(v.Name, func(gw *store.Gateway) bool {
				gw.State, gw.Err = store.GatewayConnecting, ""
				return true
			})
			continue
		}
		msg := describe(v, err)
		st.UpdateGateway(v.Name, func(gw *store.Gateway) bool {
			if gw.State == store.GatewayUnreachable && gw.Err == msg {
				return false
			}
			gw.State, gw.Err = store.GatewayUnreachable, msg
			return true
		})
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff = min(2*backoff, maxBackoff)
	}
}

// serve reads the gateway through c until an error or ctx ends. It reports
// whether the gateway answered.
func (g *Gateways) serve(ctx context.Context, st *store.Store, vm string, c GatewayClient, positions map[sandboxKey]*position) (bool, error) {
	version, err := c.Version(ctx)
	if err != nil {
		return false, err
	}
	tctx, stopTraffic := context.WithCancel(ctx)
	tr := newTraffic(g, st, vm, c, positions)
	trafficDone := make(chan struct{})
	go func() {
		defer close(trafficDone)
		tr.run(tctx)
	}()
	defer func() {
		stopTraffic()
		<-trafficDone
	}()
	var (
		workspaces []string
		providers  []store.Provider
		profiles   []store.Profile
		templates  []store.Template
		lastRest   time.Time
		// policies caches each sandbox's policy, with the version and
		// providers it was read for.
		policies = map[sandboxKey]store.Sandbox{}
		// lastRead is when the gateway last answered every call.
		lastRead time.Time
	)
	// Seen changes only with what the gateway holds, so that an unchanged
	// gateway costs browsers nothing; when it stops answering, or its VM
	// stops, what it held is as of the last read.
	defer st.UpdateGateway(vm, func(gw *store.Gateway) bool {
		if lastRead.IsZero() || gw.Seen.Equal(lastRead) {
			return false
		}
		gw.Seen = lastRead
		return true
	})
	tick := time.NewTicker(g.sandboxesEvery())
	defer tick.Stop()
	for {
		if c.BundleChanged() {
			return true, errBundleChanged
		}
		sandboxes, err := c.Sandboxes(ctx)
		if err != nil {
			return true, err
		}
		if time.Since(lastRest) >= g.restEvery() {
			if workspaces, err = c.Workspaces(ctx); err != nil {
				if status.Code(err) == codes.Unimplemented {
					err = errNoWorkspaces
				}
				return true, err
			}
			if providers, err = c.Providers(ctx); err != nil {
				return true, err
			}
			if profiles, err = c.Profiles(ctx, workspaces); err != nil {
				return true, err
			}
			if templates, err = c.Templates(ctx); err != nil {
				return true, err
			}
			lastRest = time.Now()
			clear(policies)
		}
		g.addPolicies(ctx, c, sandboxes, policies)
		tr.follow(tctx, sandboxes)
		tr.readDrafts(ctx, sandboxes)
		lastRead = time.Now()
		st.UpdateGateway(vm, func(gw *store.Gateway) bool {
			if gw.State == store.GatewayReady && gw.Version == version &&
				reflect.DeepEqual(gw.Workspaces, workspaces) && reflect.DeepEqual(gw.Sandboxes, sandboxes) && reflect.DeepEqual(gw.Providers, providers) &&
				reflect.DeepEqual(gw.Profiles, profiles) && reflect.DeepEqual(gw.Templates, templates) {
				return false
			}
			gw.State, gw.Err, gw.Version, gw.Seen = store.GatewayReady, "", version, lastRead
			gw.Workspaces, gw.Sandboxes, gw.Providers, gw.Profiles, gw.Templates = workspaces, sandboxes, providers, profiles, templates
			return true
		})
		select {
		case <-ctx.Done():
			return true, ctx.Err()
		case <-tick.C:
		}
	}
}

// addPolicies sets the policy of each sandbox, reading those that the cache
// does not hold for the sandbox's policy version and providers. A sandbox
// whose policy could not be read gets the error in its place.
func (g *Gateways) addPolicies(ctx context.Context, c GatewayClient, sandboxes []store.Sandbox, cache map[sandboxKey]store.Sandbox) {
	seen := map[sandboxKey]bool{}
	for i := range sandboxes {
		s := &sandboxes[i]
		k := keyOf(s.Workspace, s.Name)
		seen[k] = true
		if old, ok := cache[k]; ok && old.PolicyVersion == s.PolicyVersion && slices.Equal(old.Providers, s.Providers) {
			s.Policy = old.Policy
			continue
		}
		p, err := c.Policy(ctx, s.Workspace, s.Name)
		if err != nil {
			s.Policy = &store.Policy{Err: describeCall(err)}
			continue
		}
		s.Policy = p
		cache[k] = *s
	}
	for k := range cache {
		if !seen[k] {
			delete(cache, k)
		}
	}
}

// describe says why the gateway of v could not be read.
func describe(v store.VM, err error) string {
	msg := describeCall(err)
	switch {
	case strings.Contains(msg, "handshake"), strings.Contains(msg, "certificate"):
		return fmt.Sprintf("brig ui and %s do not trust each other: %s. brig start %s fetches the gateway's client bundle again.", v.GatewayName(), msg, v.Name)
	case status.Code(err) == codes.Unavailable:
		return fmt.Sprintf("%s does not answer on 127.0.0.1:%d: %s", v.GatewayName(), v.Ports.Gateway, msg)
	}
	return msg
}

// describeCall returns a call error's message without gRPC's framing.
func describeCall(err error) string {
	if s, ok := status.FromError(err); ok && s.Code() != codes.OK {
		return s.Message()
	}
	return err.Error()
}

// readConfig reads what the config directories dirs define.
func readConfig(dirs []string) store.GatewayConfig {
	cfg := store.GatewayConfig{Dirs: slices.Clone(dirs)}
	if len(dirs) == 0 {
		return cfg
	}
	set, err := ocsync.Load(dirs)
	if err != nil {
		cfg.Err = err.Error()
		return cfg
	}
	cfg.Workspaces = map[string]store.WorkspaceConfig{}
	for _, ws := range set.WorkspaceNames() {
		wc := store.WorkspaceConfig{Providers: map[string]string{}, Profiles: map[string]string{}}
		for _, p := range set.Workspaces[ws].Providers {
			wc.Providers[p.Name] = p.Path
		}
		for _, p := range set.Workspaces[ws].Profiles {
			wc.Profiles[p.ID] = p.Path
		}
		cfg.Workspaces[ws] = wc
	}
	for _, p := range set.Policies {
		cfg.Policies = append(cfg.Policies, p.Path)
	}
	if set.DefaultPolicy != "" {
		cfg.DefaultPolicy = set.DefaultPolicy
		text, err := readCapped(set.DefaultPolicy, maxPolicyFile)
		if err != nil {
			cfg.Err = err.Error()
		}
		cfg.DefaultPolicyText = text
	}
	return cfg
}

// readCapped reads at most limit bytes of the file at path.
func readCapped(path string, limit int64) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	data, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return "", err
	}
	if int64(len(data)) > limit {
		return string(data[:limit]), fmt.Errorf("%s is larger than %d KiB; showing its start", path, limit>>10)
	}
	return string(data), nil
}

func (g *Gateways) sandboxesEvery() time.Duration { return cmp.Or(g.SandboxesEvery, sandboxesEvery) }
func (g *Gateways) restEvery() time.Duration      { return cmp.Or(g.RestEvery, restEvery) }
func (g *Gateways) backoff() time.Duration        { return cmp.Or(g.Backoff, time.Second) }
func (g *Gateways) flushEvery() time.Duration     { return cmp.Or(g.FlushEvery, flushEvery) }
func (g *Gateways) draftsAfter() time.Duration    { return cmp.Or(g.DraftsAfter, draftsAfter) }
func (g *Gateways) draftsEvery() time.Duration    { return cmp.Or(g.DraftsEvery, draftsEvery) }

func (g *Gateways) log() *slog.Logger {
	if g.Log == nil {
		return slog.New(slog.DiscardHandler)
	}
	return g.Log
}
