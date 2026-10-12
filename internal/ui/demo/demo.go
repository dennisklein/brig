// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

// Package demo holds the made-up host that brig ui --demo shows: three VMs,
// their images and OpenShell gateways, a few runs and the host checks, the
// data of the design mock.
// Nothing in it touches the real host.
package demo

import (
	"context"
	"math/rand/v2"
	"sync"
	"sync/atomic"
	"time"

	"github.com/dennisklein/brig/internal/bytesize"
	"github.com/dennisklein/brig/internal/config"
	"github.com/dennisklein/brig/internal/doctor"
	"github.com/dennisklein/brig/internal/ui/store"
	"github.com/dennisklein/brig/internal/version"
	"github.com/dennisklein/brig/internal/vm"
)

// tickEvery is how often the demo's CPU and memory meters move.
const tickEvery = 3 * time.Second

// Host is the demo host. It serves the fixtures, moves the meters, and
// plays out the commands of dialogs instead of running them.
type Host struct {
	st *store.Store

	mu    sync.Mutex
	state store.VMState
	// runs are the demo's activity log, newest first.
	runs []store.Run
	// keyringLocked makes the first sync of review fail, as a locked
	// keyring would, so that the demo shows a failed task.
	keyringLocked bool

	quit    chan struct{}
	wg      sync.WaitGroup
	running atomic.Int32
}

// New returns a demo host that writes to st.
func New(st *store.Store) *Host {
	return &Host{st: st, quit: make(chan struct{}), keyringLocked: true}
}

// Run fills the store with the demo host and moves its meters until ctx ends.
func (h *Host) Run(ctx context.Context, addr string) {
	now := time.Now()
	h.st.SetHost(store.Host{Name: "zeta", BrigVersion: version.Version(), OpenShellCLI: "0.1.2", Addr: addr})
	for _, g := range Gateways(now) {
		h.st.UpdateGateway(g.VM, func(gw *store.Gateway) bool { *gw = g; return true })
	}
	h.mu.Lock()
	h.state = store.VMState{VMs: VMs(now), Images: Images(now), Profiles: Profiles(), Config: Config()}
	h.runs = Activity(now)
	h.publish()
	h.mu.Unlock()
	h.st.SetDoctor(&store.DoctorReport{Time: now.Add(-3 * time.Minute), Results: Checks()})
	tick := time.NewTicker(tickEvery)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		h.mu.Lock()
		h.state.VMs = wiggle(h.state.VMs)
		h.publish()
		h.mu.Unlock()
	}
}

// publish writes the state and the runs to the store, and makes the
// gateways follow the VMs. h.mu is held.
func (h *Host) publish() {
	h.st.SetVMs(h.state)
	h.st.SetActivity(h.runs)
	followVMs(h.st, h.state.VMs)
}

// CheckHost pretends to run the host checks again.
func CheckHost(ctx context.Context, st *store.Store) {
	select {
	case <-ctx.Done():
	case <-time.After(1200 * time.Millisecond):
	}
	st.SetDoctor(&store.DoctorReport{Time: time.Now(), Results: Checks()})
}

// wiggle returns vms with the CPU and memory use of the running ones moved
// a little, as a live host would.
func wiggle(vms []store.VM) []store.VM {
	out := make([]store.VM, len(vms))
	for i, v := range vms {
		if v.Running() && v.Stats != nil {
			s := *v.Stats
			s.CPU = pct(clamp(*s.CPU+float64(rand.IntN(15)-7), 3, 95))       //nolint:gosec // G404: made-up numbers
			s.Memory = pct(clamp(*s.Memory+float64(rand.IntN(5)-2), 10, 90)) //nolint:gosec // G404: made-up numbers
			v.Stats = &s
		}
		out[i] = v
	}
	return out
}

func clamp(x, lo, hi float64) float64 { return min(hi, max(lo, x)) }

func pct(x float64) *float64 { return &x }

// Home is the home directory of the demo's user.
const Home = "/home/dklein"

// Image IDs of the demo host, newest first.
const (
	imageNew   = "f44-openshell0.1.2-20261010T091500Z"
	imageOld   = "f44-openshell0.1.1-20260921T143000Z"
	imageOlder = "f43-openshell0.1.0-20260830T101200Z"
)

// Images returns the demo's base images, newest first.
func Images(now time.Time) []store.Image {
	return []store.Image{
		{ID: imageNew, Fedora: 44, OpenShell: "0.1.2", CreatedAt: now.Add(-48 * time.Hour), UsedBy: []string{"dev", "review"}},
		{ID: imageOld, Fedora: 44, OpenShell: "0.1.1", CreatedAt: now.Add(-21 * 24 * time.Hour), UsedBy: []string{"airgap"}},
		{ID: imageOlder, Fedora: 43, OpenShell: "0.1.0", CreatedAt: now.Add(-43 * 24 * time.Hour)},
	}
}

// Config returns the demo's config.yaml settings.
func Config() store.Config {
	const gib = bytesize.Size(1 << 30)
	return store.Config{
		Defaults:         config.Defaults{CPUs: 4, Memory: 8 * gib, RootDisk: 20 * gib, DataDisk: 40 * gib, NetworkProfile: "default", FedoraRelease: 44},
		OpenShellConfigs: []string{Home + "/src/agent-openshell"},
	}
}

// Profiles returns the demo's network profiles.
func Profiles() map[string]config.NetworkProfile {
	return map[string]config.NetworkProfile{
		"default":  {Internet: true},
		"open":     {Internet: true, Host: true, LAN: true, IPv6: true},
		"isolated": {},
		"ollama":   {Internet: true, HostPorts: []uint16{11434}},
	}
}

// VMs returns the demo's VMs as of now.
func VMs(now time.Time) []store.VM {
	ago := func(d time.Duration) time.Time { return now.Add(-d) }
	const gib = bytesize.Size(1 << 30)
	return []store.VM{
		{
			VM: vm.VM{
				Name: "dev", CreatedAt: ago(21 * 24 * time.Hour), Image: imageNew,
				CPUs: 4, Memory: 8 * gib, RootDisk: 20 * gib, DataDisk: 40 * gib,
				NetworkProfile: "ollama", Ports: vm.Ports{SSH: 42201, Gateway: 42443},
				OpenShellConfigs: []string{Home + "/src/agent-openshell"},
				Mounts: []vm.Mount{
					{Source: Home + "/src/pi-kit", Target: "/mnt/pi-kit", ReadOnly: true, Sandbox: true, Tag: "brig0"},
					{Source: Home + "/src/webapp", Target: "/home/agent/webapp", Tag: "brig2"},
				},
				NextMountTag: 3,
			},
			State: "running", OpenShell: "0.1.2", Up: ago(7*time.Hour + 34*time.Minute),
			LastSync: &store.SyncRecord{Time: ago(7*time.Hour + 33*time.Minute)},
			Stats:    &store.Stats{CPU: pct(38), Memory: pct(63), Data: pct(61)},
		},
		{
			VM: vm.VM{
				Name: "review", CreatedAt: ago(12 * 24 * time.Hour), Image: imageNew,
				CPUs: 2, Memory: 4 * gib, RootDisk: 20 * gib, DataDisk: 40 * gib,
				NetworkProfile: "default", Ports: vm.Ports{SSH: 42202, Gateway: 42444},
				OpenShellConfigs: []string{Home + "/src/agent-openshell"},
			},
			State: "running", OpenShell: "0.1.2", Up: ago(26 * time.Hour),
			LastSync: &store.SyncRecord{Time: ago(26 * time.Hour), Error: "provider gitlab-work: secret-tool found no secret for service=brig provider=gitlab-work. Unlock the keyring or store the token again, then sync."},
			Stats:    &store.Stats{CPU: pct(9), Memory: pct(41), Data: pct(22)},
		},
		{
			VM: vm.VM{
				Name: "airgap", CreatedAt: ago(40 * 24 * time.Hour), Image: imageOld,
				CPUs: 2, Memory: 4 * gib, RootDisk: 20 * gib, DataDisk: 40 * gib,
				NetworkProfile: "isolated", Ports: vm.Ports{SSH: 42203, Gateway: 42445},
				Mounts: []vm.Mount{
					{Source: Home + "/data/corpus", Target: "/mnt/corpus", ReadOnly: true, Sandbox: true, Tag: "brig0"},
				},
				NextMountTag: 1,
			},
			State: "shut off", OpenShell: "0.1.1",
			LastSync: &store.SyncRecord{Time: ago(6 * 24 * time.Hour)},
			Stats:    &store.Stats{Data: pct(34)},
		},
	}
}

// Activity returns the demo's runs, newest first.
func Activity(now time.Time) []store.Run {
	run := func(id, title string, ago time.Duration, steps ...store.Span) store.Run {
		start := now.Add(-ago)
		end := start.Add(time.Duration(20+len(steps)*9) * time.Second)
		r := store.Run{ID: id, Title: title, Start: start, End: &end, Status: "ok", Program: "brig", Steps: steps}
		for _, s := range steps {
			if s.Status != "ok" {
				r.Status = s.Status
			}
		}
		return r
	}
	ok := func(name string, sub ...store.Span) store.Span {
		return store.Span{Kind: "step", Name: name, Status: "ok", Done: true, Steps: sub}
	}
	return []store.Run{
		run("a1", "brig start dev", 453*time.Minute,
			ok("Start network (passt, nftables profile ollama)"), ok("Boot domain brig-dev"),
			ok("Wait for gateway"), ok("Register gateway brig-dev"),
			ok("Apply OpenShell config directories",
				store.Span{Kind: "target", Name: "provider llm", Status: "ok", Done: true},
				store.Span{Kind: "target", Name: "provider github", Status: "ok", Done: true})),
		run("a2", "brig start review", 26*time.Hour,
			ok("Start network (passt, nftables profile default)"), ok("Boot domain brig-review"),
			ok("Wait for gateway"), ok("Register gateway brig-review"),
			store.Span{
				Kind: "step", Name: "Apply OpenShell config directories", Status: "skipped", Done: true,
				Err:   "1 provider skipped",
				Steps: []store.Span{{Kind: "target", Name: "provider gitlab-work", Status: "skipped", Done: true, Err: "no secret in keyring"}},
			}),
		run("a3", "brig sandbox create dev --mount "+Home+"/src/pi-kit:/usr/local/share/pi-kit -- --name kit-dev", 26*time.Hour+14*time.Minute,
			ok("Add sandbox mount brig0 (hot-plug)"), ok("openshell sandbox create")),
		run("a4", "brig image build", 48*time.Hour,
			ok("Resolve OpenShell 0.1.2"), ok("mkosi build (Fedora 44)"), ok("Convert to qcow2")),
	}
}

// Checks returns the demo's host checks.
func Checks() []doctor.Result {
	return []doctor.Result{
		{Name: "user", Status: doctor.OK, Detail: "running unprivileged"},
		{Name: "kvm", Status: doctor.OK, Detail: "/dev/kvm is usable"},
		{Name: "user namespaces", Status: doctor.OK, Detail: "enabled"},
		{Name: "libvirt session", Status: doctor.OK, Detail: "qemu:///session answers"},
		{Name: "virtiofsd", Status: doctor.OK, Detail: "/usr/libexec/virtiofsd"},
		{Name: "podman", Status: doctor.OK, Detail: "for brig image push"},
		{Name: "secret-tool", Status: doctor.OK, Detail: "keyring unlocked"},
		{Name: "openshell", Status: doctor.OK, Detail: "0.1.2, matches the gateways' minor version"},
		{Name: "mkosi", Status: doctor.OK, Detail: "26.1"},
		{
			Name: "subordinate IDs", Status: doctor.Warn, Detail: "dklein has 65536 in /etc/subuid; 2 VMs share the range",
			Hint: "give dklein a larger range in /etc/subuid and /etc/subgid",
		},
		{Name: "VM network", Status: doctor.OK, Detail: "passt can sandbox itself (brig-selinux loaded)"},
	}
}
