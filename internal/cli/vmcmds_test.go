// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/dennisklein/brig/internal/bytesize"
	"github.com/dennisklein/brig/internal/ports"
	"github.com/dennisklein/brig/internal/vm"
)

// testApp points brig's XDG directories at temporary ones and returns an app
// using them.
func testApp(t *testing.T) *app {
	t.Helper()
	for _, env := range []string{"XDG_CONFIG_HOME", "XDG_DATA_HOME", "XDG_CACHE_HOME", "XDG_RUNTIME_DIR"} {
		t.Setenv(env, t.TempDir())
	}
	a, err := newApp()
	if err != nil {
		t.Fatal(err)
	}
	return a
}

// testVM saves a VM record called name.
func testVM(t *testing.T, a *app, name string) *vm.VM {
	t.Helper()
	v := &vm.VM{
		Name: name, UUID: "8a0e3f4b-1d2c-4e5f-9a8b-7c6d5e4f3a2b", CreatedAt: time.Now().UTC(),
		Image: "f44-openshell0.1.2-20261007T120000Z", CPUs: 2, Memory: 2 * bytesize.GiB,
		RootDisk: 20 * bytesize.GiB, DataDisk: 10 * bytesize.GiB, NetworkProfile: "default",
		Ports: vm.Ports{SSH: 40022, Gateway: 40670},
	}
	if err := a.vms.Save(v); err != nil {
		t.Fatal(err)
	}
	return v
}

// captureExec replaces execve for the test and returns what it was called
// with.
func captureExec(t *testing.T) *[]string {
	t.Helper()
	var got []string
	orig := execve
	execve = func(name string, argv []string) error {
		got = append([]string{name}, argv...)
		return nil
	}
	t.Cleanup(func() { execve = orig })
	return &got
}

func TestSSHExecsOneSSH(t *testing.T) {
	a := testApp(t)
	testVM(t, a, "dev")
	got := captureExec(t)
	if out, err := run(t, "ssh", "dev", "--", "journalctl", "-b"); err != nil {
		t.Fatalf("brig ssh: %v: %s", err, out)
	}
	// name, then argv: ssh ... -p PORT ... -- agent@127.0.0.1 journalctl -b,
	// with "ssh" only as the program name.
	argv := *got
	if len(argv) < 3 || argv[0] != "ssh" || argv[1] != "ssh" || slices.Contains(argv[2:], "ssh") {
		t.Fatalf("exec %q", argv)
	}
	if i := slices.Index(argv, "-p"); i < 0 || i+1 >= len(argv) || argv[i+1] != "40022" {
		t.Fatalf("exec %q: want -p 40022", argv)
	}
	dash := slices.Index(argv, "--")
	if dash < 0 || !slices.Equal(argv[dash+1:], []string{"agent@127.0.0.1", "journalctl", "-b"}) {
		t.Fatalf("exec %q: want destination agent@127.0.0.1 and the command after --", argv)
	}
}

func TestCreateChecksSettingsBeforeBuildingAnImage(t *testing.T) {
	src := t.TempDir()
	for _, tc := range []struct {
		args    []string
		wantErr string
	}{
		{[]string{"--mount", filepath.Join(src, "missing")}, "is not a directory"},
		{[]string{"--mount", src + ":/home/agent/.ssh"}, "must not hide"},
		{[]string{"--mount", src + ":/a", "--mount", src + ":/a"}, "duplicate"},
		{[]string{"--cpus", "0"}, "at least 1 CPU"},
		{[]string{"--memory", "256MiB"}, "512MiB of memory"},
		{[]string{"--root-disk", "1GiB"}, "root disk of at least 8GiB"},
	} {
		testApp(t)
		out, err := run(t, append([]string{"create", "dev"}, tc.args...)...)
		if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
			t.Errorf("create %q: %v, want an error containing %q", tc.args, err, tc.wantErr)
		}
		if strings.Contains(out, "building") {
			t.Errorf("create %q started to build an image:\n%s", tc.args, out)
		}
	}
}

func TestCompleteMountTargets(t *testing.T) {
	a := testApp(t)
	v := testVM(t, a, "dev")
	v.Mounts = []vm.Mount{{Source: "/src", Target: "/mnt/src", ReadOnly: true, Tag: "brig0"}}
	if err := a.vms.Save(v); err != nil {
		t.Fatal(err)
	}
	out, err := run(t, "__complete", "update", "dev", "--remove-mount", "")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(out, "/mnt/src\n:4\n") {
		t.Fatalf("completion = %q, want the mount target without file completion", out)
	}
	if i := mountIndex(v.Mounts, "/mnt/src/"); i != 0 {
		t.Fatalf("mountIndex(/mnt/src/) = %d", i)
	}
}

func TestListShowsVMsBesideABrokenRecord(t *testing.T) {
	a := testApp(t)
	testVM(t, a, "dev")
	broken := filepath.Join(a.vms.Dir("web"), "vm.json")
	if err := os.MkdirAll(filepath.Dir(broken), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(broken, []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := run(t, "list", "-o", "json")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, `"name": "dev"`) || !strings.Contains(out, "Warning:") || !strings.Contains(out, "web") {
		t.Fatalf("brig list -o json:\n%s", out)
	}
}

func TestHelpShowsNoZeroSizeDefaults(t *testing.T) {
	for _, command := range []string{"create", "update"} {
		out, err := run(t, command, "--help")
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(out, "--memory") || strings.Contains(out, "default 0B") {
			t.Errorf("brig %s --help:\n%s", command, out)
		}
	}
}

func TestReservePortsKeepsPortsDistinct(t *testing.T) {
	a := testApp(t)
	v := testVM(t, a, "web")
	port, err := ports.Free()
	if err != nil {
		t.Fatal(err)
	}
	// As saved by an earlier start whose two new ports coincided.
	v.Ports = vm.Ports{SSH: port, Gateway: port}
	if err := a.reservePorts(v); err != nil {
		t.Fatal(err)
	}
	if v.Ports.SSH == v.Ports.Gateway || v.Ports.SSH == 0 || v.Ports.Gateway == 0 {
		t.Fatalf("ports = %+v", v.Ports)
	}
	saved, err := a.vms.Load("web")
	if err != nil || saved.Ports != v.Ports {
		t.Fatalf("saved ports = %+v, %v; want %+v", saved.Ports, err, v.Ports)
	}
}

// testImage adds an image record called id to the store.
func testImage(t *testing.T, a *app, id string) {
	t.Helper()
	dir := filepath.Dir(a.images.Path(id))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	rec := `{"id":"` + id + `","fedora_release":44,"openshell_version":"0.1.2","created_at":"2026-10-07T12:00:00Z"}`
	if err := os.WriteFile(filepath.Join(dir, "image.json"), []byte(rec), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestCreateLeavesAVMCreatedMeanwhileAlone(t *testing.T) {
	a := testApp(t)
	old := testVM(t, a, "dev")
	testImage(t, a, old.Image)
	key := a.vmFile(old, clientKeyFile)
	if err := os.WriteFile(key, []byte("key of the existing VM"), 0o600); err != nil {
		t.Fatal(err)
	}
	// As if the VM had been created while this create waited for an image
	// build, after its check that the name is free.
	v := &vm.VM{Name: "dev", Image: old.Image, CPUs: 1, Memory: bytesize.GiB, RootDisk: 20 * bytesize.GiB, DataDisk: bytesize.GiB}
	cmd := newCreateCmd()
	cmd.SetOut(io.Discard)
	err := a.create(t.Context(), cmd, v, false)
	if err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("create() = %v, want an error saying that the VM exists", err)
	}
	if data, err := os.ReadFile(key); err != nil || string(data) != "key of the existing VM" {
		t.Fatalf("existing VM's key = %q, %v", data, err)
	}
	if !a.vms.Exists("dev") {
		t.Fatal("existing VM was removed")
	}
}
