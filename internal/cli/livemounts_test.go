// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"libvirt.org/go/libvirtxml"

	"github.com/dennisklein/brig/internal/guest"
	"github.com/dennisklein/brig/internal/libvirt"
	"github.com/dennisklein/brig/internal/vm"
)

// fakeDevices records the devices attached to and detached from running
// domains.
type fakeDevices struct {
	calls []string
	err   error
}

func (f *fakeDevices) AttachDevice(name, x string) error {
	f.calls = append(f.calls, "attach "+name+" "+x)
	return f.err
}

func (f *fakeDevices) DetachDevice(name, x string) error {
	f.calls = append(f.calls, "detach "+name+" "+x)
	return f.err
}

// fakeSSH puts a fake ssh on PATH that logs the user and the script of each
// login to the returned file, as "USER@HOST SCRIPT", and prints out.
func fakeSSH(t *testing.T, out string) string {
	t.Helper()
	dir := t.TempDir()
	log := filepath.Join(dir, "log")
	body := "#!/bin/sh\nfor a; do dest=$script; script=$a; done\nprintf '%s %s\\n' \"$dest\" \"$script\" >>" + log +
		"\nprintf '%s' '" + out + "'\n"
	if err := os.WriteFile(filepath.Join(dir, "ssh"), []byte(body), 0o700); err != nil { //nolint:gosec // G306: scripts must be executable
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	orig := hostMountIDMap
	hostMountIDMap = func() (*libvirtxml.DomainFilesystemIDMap, error) {
		return &libvirtxml.DomainFilesystemIDMap{
			UID: []libvirtxml.DomainFilesystemIDMapEntry{{Start: 1000, Target: 4242, Count: 1}},
			GID: []libvirtxml.DomainFilesystemIDMapEntry{{Start: 1000, Target: 4242, Count: 1}},
		}, nil
	}
	t.Cleanup(func() { hostMountIDMap = orig })
	return log
}

func readLog(t *testing.T, log string) string {
	t.Helper()
	b, err := os.ReadFile(log)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	return string(b)
}

func TestApplyMountsAddsASandboxMount(t *testing.T) {
	a := testApp(t)
	v := testVM(t, a, "dev")
	plain := vm.Mount{Source: "/src/a", Target: "/mnt/a", ReadOnly: true, Tag: "brig0"}
	box := vm.Mount{Source: "/src/b", Target: "/home/agent/kit/b", ReadOnly: true, Sandbox: true, Tag: "brig1"}
	old := []vm.Mount{plain}
	v.Mounts = []vm.Mount{plain, box}
	log := fakeSSH(t, "")
	devs := &fakeDevices{}
	restart, err := a.applyMounts(context.Background(), devs, v, old)
	if err != nil || restart {
		t.Fatalf("applyMounts = %v, %v", restart, err)
	}
	want, err := libvirt.FilesystemXML(box, &libvirtxml.DomainFilesystemIDMap{
		UID: []libvirtxml.DomainFilesystemIDMapEntry{{Start: 1000, Target: 4242, Count: 1}},
		GID: []libvirtxml.DomainFilesystemIDMapEntry{{Start: 1000, Target: 4242, Count: 1}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(devs.calls) != 1 || devs.calls[0] != "attach brig-dev "+want {
		t.Errorf("device calls = %q", devs.calls)
	}
	dirs, _ := guest.MountDirsScript(v.Mounts, box)
	mount, _ := guest.MountScript(v.Mounts, box)
	volumes, _ := guest.SandboxVolumesScript(v.Mounts)
	if got, want := readLog(t, log), "agent@127.0.0.1 "+dirs+"\nroot@127.0.0.1 "+mount+"\nagent@127.0.0.1 "+volumes+"\n"; got != want {
		t.Errorf("guest ran\n%s\nwant\n%s", got, want)
	}
}

func TestApplyMountsReportsAGatewayWithoutVolumes(t *testing.T) {
	a := testApp(t)
	v := testVM(t, a, "dev")
	v.Mounts = []vm.Mount{{Source: "/src/b", Target: "/mnt/b", ReadOnly: true, Sandbox: true, Tag: "brig0"}}
	fakeSSH(t, guest.GatewayRestartNeeded+"\n")
	restart, err := a.applyMounts(context.Background(), &fakeDevices{}, v, nil)
	if err != nil || !restart {
		t.Fatalf("applyMounts = %v, %v; want a gateway restart", restart, err)
	}
}

func TestApplyMountsRemovesAMountBeforeDetachingIt(t *testing.T) {
	a := testApp(t)
	v := testVM(t, a, "dev")
	plain := vm.Mount{Source: "/src/a", Target: "/mnt/a", Tag: "brig0"}
	log := fakeSSH(t, "")
	devs := &fakeDevices{}
	if _, err := a.applyMounts(context.Background(), devs, v, []vm.Mount{plain}); err != nil {
		t.Fatal(err)
	}
	unmount, _ := guest.UnmountScript(plain)
	if got, want := readLog(t, log), "root@127.0.0.1 "+unmount+"\n"; got != want {
		t.Errorf("guest ran\n%s\nwant\n%s", got, want)
	}
	if len(devs.calls) != 1 || !strings.HasPrefix(devs.calls[0], "detach brig-dev <filesystem") {
		t.Errorf("device calls = %q", devs.calls)
	}
}

// TestApplyMountsLeavesAVolumeInUse has a sandbox attach the volume of a
// sandbox mount after brig update checked that none uses it.
func TestApplyMountsLeavesAVolumeInUse(t *testing.T) {
	a := testApp(t)
	v := testVM(t, a, "dev")
	box := vm.Mount{Source: "/src/b", Target: "/mnt/b", ReadOnly: true, Sandbox: true, Tag: "brig1"}
	log := fakeSSH(t, "openshell-default--kit\n")
	devs := &fakeDevices{}
	if _, err := a.applyMounts(context.Background(), devs, v, []vm.Mount{box}); err == nil || !strings.Contains(err.Error(), "in use as volume brig1") {
		t.Fatalf("applyMounts = %v", err)
	}
	users, _ := guest.VolumeUsersScript(box)
	if got, want := readLog(t, log), "agent@127.0.0.1 "+users+"\n"; got != want {
		t.Errorf("guest ran\n%s\nwant\n%s", got, want)
	}
	if len(devs.calls) != 0 {
		t.Errorf("device calls = %q", devs.calls)
	}
}

func TestApplyMountsStopsAtAFailedAttach(t *testing.T) {
	a := testApp(t)
	v := testVM(t, a, "dev")
	v.Mounts = []vm.Mount{{Source: "/src/a", Target: "/mnt/a", Tag: "brig0"}}
	log := fakeSSH(t, "")
	devs := &fakeDevices{err: errors.New("no free PCI slot")}
	if _, err := a.applyMounts(context.Background(), devs, v, nil); err == nil || !strings.Contains(err.Error(), "no free PCI slot") {
		t.Fatalf("applyMounts = %v", err)
	}
	if got := readLog(t, log); got != "" {
		t.Errorf("guest ran %q after the attach failed", got)
	}
}

func TestApplyMountsWithoutChanges(t *testing.T) {
	a := testApp(t)
	v := testVM(t, a, "dev")
	v.Mounts = []vm.Mount{{Source: "/src/a", Target: "/mnt/a", Tag: "brig0"}}
	log := fakeSSH(t, "")
	devs := &fakeDevices{}
	if _, err := a.applyMounts(context.Background(), devs, v, v.Mounts); err != nil {
		t.Fatal(err)
	}
	if len(devs.calls) != 0 || readLog(t, log) != "" {
		t.Errorf("applyMounts changed something: %q", devs.calls)
	}
}

func TestCheckVolumesUnused(t *testing.T) {
	a := testApp(t)
	v := testVM(t, a, "dev")
	box := vm.Mount{Source: "/src/b", Target: "/mnt/b", ReadOnly: true, Sandbox: true, Tag: "brig1"}
	plain := vm.Mount{Source: "/src/a", Target: "/mnt/a", Tag: "brig0"}
	log := fakeSSH(t, "openshell-default--kit\n")
	if err := a.checkVolumesUnused(context.Background(), v, []vm.Mount{plain}); err != nil {
		t.Errorf("a plain mount: %v", err)
	}
	if got := readLog(t, log); got != "" {
		t.Errorf("checked the volume of a plain mount: %q", got)
	}
	err := a.checkVolumesUnused(context.Background(), v, []vm.Mount{box})
	if err == nil || !strings.Contains(err.Error(), "in use as volume brig1 by openshell-default--kit") {
		t.Errorf("a used sandbox mount: %v", err)
	}
	fakeSSH(t, "")
	if err := a.checkVolumesUnused(context.Background(), v, []vm.Mount{box}); err != nil {
		t.Errorf("an unused sandbox mount: %v", err)
	}
}
