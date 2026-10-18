// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

package guest

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/dennisklein/brig/internal/vm"
)

// runScript runs script with sh, with fake commands on PATH that log their
// arguments one per line, a command per paragraph, and succeed; mountpoint
// fails, as for a directory that nothing is mounted on, unless mounted is
// set. It returns the log.
func runScript(t *testing.T, script string, mounted bool) string {
	t.Helper()
	dir := t.TempDir()
	log := filepath.Join(dir, "log")
	fake := "#!/bin/sh\n{ echo \"${0##*/}\"; for a; do echo \"$a\"; done; echo; } >>" + log + "\n"
	for _, name := range []string{"install", "mkdir", "mount", "umount", "systemctl", "grep", "podman"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(fake), 0o700); err != nil { //nolint:gosec // G306: scripts must be executable
			t.Fatal(err)
		}
	}
	status := "exit 1"
	if mounted {
		status = "exit 0"
	}
	if err := os.WriteFile(filepath.Join(dir, "mountpoint"), []byte(fake+status+"\n"), 0o700); err != nil { //nolint:gosec // G306: scripts must be executable
		t.Fatal(err)
	}
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip(err)
	}
	cmd := exec.Command(sh, "-c", script)
	cmd.Env = []string{"PATH=" + dir}
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%v: %s\n%s", err, out, script)
	}
	b, err := os.ReadFile(log)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	return string(b)
}

func TestMountScript(t *testing.T) {
	mounts := testBootConfig().Mounts
	mounts = append(mounts,
		vm.Mount{Source: "/src/x", Target: `/home/agent/it's$x/y`, ReadOnly: true, Sandbox: true, Tag: "brig3"})
	script, err := MountScript(mounts, mounts[3])
	if err != nil {
		t.Fatal(err)
	}
	want := "install\n-d\n-m\n0700\n-o\nagent\n-g\nagent\n/home/agent/it's$x\n\n" +
		"mkdir\n-p\n/home/agent/it's$x/y\n\n" +
		"mountpoint\n-q\n/home/agent/it's$x/y\n\n" +
		"mount\n-t\nvirtiofs\n-o\nro,context=system_u:object_r:container_file_t:s0\nbrig3\n/home/agent/it's$x/y\n\n"
	if got := runScript(t, script, false); got != want {
		t.Errorf("mount script ran\n%s\nwant\n%s", got, want)
	}
	// A target that is mounted already stays as it is.
	script, err = MountScript(mounts, mounts[1])
	if err != nil {
		t.Fatal(err)
	}
	if got := runScript(t, script, true); strings.Contains(got, "\nmount\n") || strings.Contains(got, "install") {
		t.Errorf("mount script ran\n%s", got)
	}
}

func TestMountScriptRejects(t *testing.T) {
	mounts := testBootConfig().Mounts
	if _, err := MountScript(mounts, vm.Mount{Source: "/src/d", Target: "/mnt/d", Tag: "brig9"}); err == nil {
		t.Error("MountScript accepted a mount that is not the VM's")
	}
	bad := append(slices.Clone(mounts), vm.Mount{Source: "/src/d", Target: "/home/agent/.ssh", Tag: "brig9"})
	if _, err := MountScript(bad, bad[3]); err == nil {
		t.Error("MountScript accepted a mount over a provisioned directory")
	}
}

func TestUnmountScript(t *testing.T) {
	m := vm.Mount{Source: "/src/x", Target: "/mnt/a b", Tag: "brig3"}
	if _, err := UnmountScript(m); err == nil {
		t.Error("UnmountScript accepted a target with a space")
	}
	m.Target = "/mnt/it's"
	script, err := UnmountScript(m)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := runScript(t, script, true), "mountpoint\n-q\n/mnt/it's\n\numount\n/mnt/it's\n\n"; got != want {
		t.Errorf("unmount script ran\n%s\nwant\n%s", got, want)
	}
	if got, want := runScript(t, script, false), "mountpoint\n-q\n/mnt/it's\n\n"; got != want {
		t.Errorf("unmount script without a mount ran\n%s\nwant\n%s", got, want)
	}
}

func TestSandboxVolumesScript(t *testing.T) {
	script, err := SandboxVolumesScript(testBootConfig().Mounts)
	if err != nil {
		t.Fatal(err)
	}
	// The list goes where tmpfiles writes it at boot, as there.
	if !strings.Contains(script, "printf %s 'brig2 /home/agent/c\n' >/home/agent/.config/brig/sandbox-volumes\n") {
		t.Errorf("script does not write the volume list:\n%s", script)
	}
	want := "systemctl\n--user\nrestart\nbrig-sandbox-volumes.service\n\n" +
		"grep\n-qsF\n/usr/share/brig/gateway-mounts.toml\n/home/agent/.config/brig/gateway.env\n\n"
	// The script writes to the real path, so run it without that line.
	run := strings.Replace(script, ">/home/agent/.config/brig/sandbox-volumes", ">/dev/null", 1)
	if got := runScript(t, run, false); got != want {
		t.Errorf("sandbox volumes script ran\n%s\nwant\n%s", got, want)
	}
}

func TestVolumeUsersScript(t *testing.T) {
	script, err := VolumeUsersScript(testBootConfig().Mounts[2])
	if err != nil {
		t.Fatal(err)
	}
	if got, want := runScript(t, script, false), "podman\nps\n-a\n--filter\nvolume=brig2\n--format\n{{.Names}}\n\n"; got != want {
		t.Errorf("volume users script ran\n%s\nwant\n%s", got, want)
	}
}
