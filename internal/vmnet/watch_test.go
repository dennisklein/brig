// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

package vmnet

import (
	"context"
	"encoding/json"
	"errors"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dennisklein/brig/internal/config"
)

var (
	wifi = HostNet{Addrs: []netip.Prefix{netip.MustParsePrefix("192.168.1.10/24")}}
	// The host moved to another network and connected a VPN.
	moved = HostNet{
		Addrs:  []netip.Prefix{netip.MustParsePrefix("203.0.113.5/24"), netip.MustParsePrefix("2001:db8:9::5/64")},
		Routes: []netip.Prefix{netip.MustParsePrefix("131.169.0.0/16")},
	}
)

func TestReloader(t *testing.T) {
	reloadEvery = 5 * time.Millisecond
	t.Cleanup(func() { reloadEvery = 500 * time.Millisecond })
	path := filepath.Join(t.TempDir(), hostNetFile)
	loads := make(chan HostNet, 10)
	var fail, gone atomic.Bool
	r := reloader{
		path:    path,
		current: wifi,
		load: func(h HostNet, _ []uint16) error {
			if fail.Load() {
				return errors.New("nft failed")
			}
			loads <- h
			return nil
		},
		parentGone: gone.Load,
	}
	done := make(chan error)
	go func() { done <- r.run(context.Background()) }()
	write := func(h HostNet) {
		t.Helper()
		if err := writeHostNet(path, h); err != nil {
			t.Fatal(err)
		}
	}
	loaded := func(want HostNet) {
		t.Helper()
		select {
		case h := <-loads:
			if !h.equal(want) {
				t.Fatalf("loaded %+v, want %+v", h, want)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("no reload after a change")
		}
	}
	quiet := func() {
		t.Helper()
		select {
		case h := <-loads:
			t.Fatalf("loaded %+v for an unchanged network", h)
		case <-time.After(50 * time.Millisecond):
		}
	}

	// The watcher's first write: what the firewall was loaded with.
	write(wifi)
	quiet()
	write(moved)
	loaded(moved)
	write(moved)
	quiet()
	// After a failed load, the next change tries again.
	fail.Store(true)
	write(wifi)
	quiet()
	fail.Store(false)
	write(wifi)
	loaded(wifi)

	gone.Store(true)
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the reloader outlived its parent")
	}
}

func TestReloaderServicePorts(t *testing.T) {
	reloadEvery = 5 * time.Millisecond
	t.Cleanup(func() { reloadEvery = 500 * time.Millisecond })
	dir := t.TempDir()
	hostNet, ports := filepath.Join(dir, hostNetFile), ServicePortsPath(filepath.Join(dir, "net.sock"))
	if err := writeHostNet(hostNet, wifi); err != nil {
		t.Fatal(err)
	}
	type load struct {
		h     HostNet
		ports []uint16
	}
	loads := make(chan load, 10)
	var gone atomic.Bool
	r := reloader{
		path: hostNet, current: wifi, portsPath: ports, ports: []uint16{4000},
		load: func(h HostNet, p []uint16) error {
			loads <- load{h, p}
			return nil
		},
		parentGone: gone.Load,
	}
	done := make(chan error)
	go func() { done <- r.run(context.Background()) }()
	defer func() {
		gone.Store(true)
		<-done
	}()
	loaded := func(want []uint16) {
		t.Helper()
		select {
		case l := <-loads:
			if !slices.Equal(l.ports, want) || !l.h.equal(wifi) {
				t.Fatalf("loaded %+v, want ports %v and the unchanged host network", l, want)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("no reload after a change")
		}
	}
	quiet := func() {
		t.Helper()
		select {
		case l := <-loads:
			t.Fatalf("loaded %+v without a change", l)
		case <-time.After(50 * time.Millisecond):
		}
	}
	write := func(p ...uint16) {
		t.Helper()
		if err := WriteServicePorts(ports, p); err != nil {
			t.Fatal(err)
		}
	}

	// The file as the firewall was loaded with it.
	write(4000)
	quiet()
	write(4001, 4000, 4001)
	loaded([]uint16{4000, 4001})
	// No service left removes the file, which closes the ports.
	write()
	if _, err := os.Stat(ports); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("no ports left the file: %v", err)
	}
	loaded(nil)
	quiet()
}

func TestServicePortsFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "run", servicePortsFile)
	if got, err := ReadServicePorts(path); err != nil || got != nil {
		t.Fatalf("missing file: %v, %v", got, err)
	}
	if err := WriteServicePorts(path, []uint16{4001, 4000}); err != nil {
		t.Fatal(err)
	}
	if got, err := ReadServicePorts(path); err != nil || !slices.Equal(got, []uint16{4000, 4001}) {
		t.Fatalf("ReadServicePorts() = %v, %v", got, err)
	}
	if err := WriteServicePorts(path, []uint16{0}); err == nil {
		t.Fatal("port 0 was written")
	}
	p := withServicePorts(config.NetworkProfile{Internet: true, HostPorts: []uint16{11434, 4000}}, []uint16{4000, 4001})
	if !slices.Equal(p.HostPorts, []uint16{4000, 4001, 11434}) || !p.Internet {
		t.Fatalf("withServicePorts() = %+v", p)
	}
}

// TestReloadRulesetLoads replaces a loaded ruleset with nft in a throwaway
// namespace when the host allows unprivileged user namespaces.
func TestReloadRulesetLoads(t *testing.T) {
	if _, err := exec.LookPath("nft"); err != nil {
		t.Skip("nft not installed")
	}
	if exec.Command("unshare", "-Urn", "true").Run() != nil {
		t.Skip("unprivileged user and network namespaces unavailable")
	}
	dir := t.TempDir()
	p := config.NetworkProfile{Internet: true, IPv6: true}
	first := Ruleset(p, testGateways, wifi.Addrs, wifi.Routes)
	second := reloadRuleset(Ruleset(p, testGateways, moved.Addrs, moved.Routes))
	for name, s := range map[string]string{"first.nft": first, "second.nft": second} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(s), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	cmd := exec.Command("unshare", "-Urn", "sh", "-ec", `nft -f "$0/first.nft"; nft -f "$0/second.nft"; nft list ruleset`, dir)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	got := string(out)
	for _, s := range []string{"203.0.113.5 drop", "2001:db8:9::5 drop", "131.169.0.0/16"} {
		if !strings.Contains(got, s) {
			t.Errorf("missing %q in\n%s", s, got)
		}
	}
	if strings.Contains(got, "192.168.1.10") || strings.Count(got, "chain output") != 1 {
		t.Errorf("the old rules are left:\n%s", got)
	}
}

func TestRunWatcher(t *testing.T) {
	read := readHostNet
	readHostNet = func() (HostNet, error) { return moved, nil }
	t.Cleanup(func() { readHostNet = read })
	path := filepath.Join(t.TempDir(), hostNetFile)

	err := RunWatcher(context.Background(), path, []string{"sh", "-c", "exit 3"})
	if exit, ok := errors.AsType[*exec.ExitError](err); !ok || exit.ExitCode() != 3 {
		t.Errorf("pasta's exit: %v", err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var h HostNet
	if err := json.Unmarshal(b, &h); err != nil || !h.equal(moved) {
		t.Errorf("written: %s, %v", b, err)
	}

	// A stop is passed on to pasta, which then exits as it likes.
	ctx, cancel := context.WithCancel(context.Background())
	ready := filepath.Join(t.TempDir(), "ready")
	go func() {
		for {
			if _, err := os.Stat(ready); err == nil {
				cancel()
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
	}()
	err = RunWatcher(ctx, path, []string{"sh", "-c", `trap "exit 7" TERM; touch "$0"; while :; do sleep 0.01; done`, ready})
	if exit, ok := errors.AsType[*exec.ExitError](err); !ok || exit.ExitCode() != 7 {
		t.Errorf("pasta's exit after a stop: %v", err)
	}
}
