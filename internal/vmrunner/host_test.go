// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

package vmrunner

import (
	"bytes"
	"context"
	"encoding/base64"
	"image/png"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dennisklein/brig/internal/config"
	"github.com/dennisklein/brig/internal/libvirt"
)

// TestHost drives a real VM through libvirt on this host, from the qcow2
// image that $BRIG_TEST_VM_IMAGE names and that runs qemu-guest-agent:
//
//	BRIG_TEST_VM_IMAGE=~/vm-images/Fedora-Cloud-Base-Generic-44.x86_64.qcow2 \
//	  go test ./internal/vmrunner -run TestHost -v -timeout 15m
//
// It checks what the fake hypervisor of the other tests cannot: the domain
// boots from brig's template, the screen dump, input events, the guest
// agent and internal snapshots with memory.
func TestHost(t *testing.T) {
	image := os.Getenv("BRIG_TEST_VM_IMAGE")
	if image == "" {
		t.Skip("set BRIG_TEST_VM_IMAGE to a qcow2 image with qemu-guest-agent to drive a real VM")
	}
	if rest, ok := strings.CutPrefix(image, "~/"); ok {
		home, _ := os.UserHomeDir()
		image = filepath.Join(home, rest)
	}
	dir := t.TempDir()
	writeTokens(t, filepath.Join(dir, "tokens.json"), map[string]Owner{"tok-alice": alice})
	cfg := config.Builtin().VMRunner
	cfg.Images = map[string]config.RunnerImage{"test": {File: image}}
	c, err := New(Options{
		Dir: dir, Config: cfg, Tokens: NewTokens(filepath.Join(dir, "tokens.json")),
		Connect: func(ctx context.Context) (Hypervisor, error) { return libvirt.Connect(ctx) },
		Log:     Logger(os.Stderr),
	})
	if err != nil {
		t.Fatal(err)
	}
	env := &testEnv{t: t, c: c, dir: dir, srv: httptest.NewServer(c.Server(nil))}
	t.Cleanup(env.srv.Close)

	start := time.Now()
	r := env.ok("tok-alice", "vm_create", map[string]any{"image": "test", "memory_mib": 2048, "lifetime_minutes": 30})
	id := r.data["id"].(string)
	t.Cleanup(func() {
		if err := c.DestroyAny(context.Background(), id); err != nil {
			t.Errorf("destroy %s: %v", id, err)
		}
	})
	if r.data["guest_agent"] != "ready" {
		t.Fatalf("vm_create = %s", r.raw)
	}
	t.Logf("VM %s booted and its guest agent answered in %s", id, time.Since(start).Round(time.Second))

	r = env.ok("tok-alice", "vm_screenshot", map[string]any{"vm": id})
	img, err := png.Decode(bytes.NewReader(r.image))
	if err != nil {
		t.Fatalf("screenshot: %v", err)
	}
	t.Logf("screenshot of %s", r.text)
	b := img.Bounds()
	env.ok("tok-alice", "vm_click", map[string]any{"vm": id, "x": b.Dx() / 2, "y": b.Dy() / 2})
	env.ok("tok-alice", "vm_type", map[string]any{"vm": id, "text": "root\n"})
	env.ok("tok-alice", "vm_key", map[string]any{"vm": id, "keys": []string{"ctrl+c", "enter"}})

	r = env.ok("tok-alice", "vm_exec", map[string]any{"vm": id, "argv": []string{"sh", "-c", "uname -sm; echo err >&2"}})
	if !strings.HasPrefix(r.data["stdout"].(string), "Linux ") || r.data["stderr"] != "err\n" {
		t.Errorf("vm_exec = %s", r.raw)
	}
	r = env.call("tok-alice", "vm_exec", map[string]any{"vm": id, "argv": []string{"sleep", "60"}, "timeout_seconds": 2})
	if !r.isError || r.data["timed_out"] != true {
		t.Errorf("vm_exec past its timeout = %s", r.raw)
	}

	big := bytes.Repeat([]byte("0123456789abcdef"), 3<<16) // 3 MiB: several agent calls
	env.ok("tok-alice", "vm_copy_in", map[string]any{"vm": id, "path": "/root/big", "content_base64": base64.StdEncoding.EncodeToString(big)})
	r = env.ok("tok-alice", "vm_copy_out", map[string]any{"vm": id, "path": "/root/big"})
	if got, _ := base64.StdEncoding.DecodeString(r.data["content_base64"].(string)); !bytes.Equal(got, big) {
		t.Errorf("vm_copy_out returned %d bytes, want the %d written", len(got), len(big))
	}

	put := func(s string) {
		env.ok("tok-alice", "vm_copy_in", map[string]any{"vm": id, "path": "/root/state", "content_base64": base64.StdEncoding.EncodeToString([]byte(s))})
	}
	put("before")
	start = time.Now()
	env.ok("tok-alice", "vm_snapshot", map[string]any{"vm": id, "name": "clean"})
	t.Logf("snapshot took %s", time.Since(start).Round(time.Millisecond))
	put("after")
	start = time.Now()
	env.ok("tok-alice", "vm_restore", map[string]any{"vm": id, "name": "clean"})
	t.Logf("restore took %s", time.Since(start).Round(time.Millisecond))
	r = env.ok("tok-alice", "vm_copy_out", map[string]any{"vm": id, "path": "/root/state"})
	if got, _ := base64.StdEncoding.DecodeString(r.data["content_base64"].(string)); string(got) != "before" {
		t.Errorf("after vm_restore the file holds %q, want before", got)
	}
	env.ok("tok-alice", "vm_snapshot_delete", map[string]any{"vm": id, "name": "clean"})

	// The VM has no network card.
	r = env.ok("tok-alice", "vm_exec", map[string]any{"vm": id, "argv": []string{"ls", "/sys/class/net"}})
	if out := strings.Fields(r.data["stdout"].(string)); len(out) != 1 || out[0] != "lo" {
		t.Errorf("network interfaces in the VM: %v, want only lo", out)
	}
}
