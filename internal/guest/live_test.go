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

// runFakes runs script with sh in a PATH that has fakes for mount, umount and
// mountpoint before the system's commands. The fakes log their name, working
// directory and arguments on one line and succeed; mountpoint fails unless
// mounted is set. It returns the log, and the output and error of sh.
func runFakes(t *testing.T, script string, mounted bool) (log, out string, err error) {
	t.Helper()
	dir := t.TempDir()
	logFile := filepath.Join(dir, "log")
	fake := "#!/bin/sh\necho \"${0##*/} $(pwd -P) $*\" >>" + logFile + "\n"
	status := "exit 1\n"
	if mounted {
		status = "exit 0\n"
	}
	for name, body := range map[string]string{"mount": fake, "umount": fake, "mountpoint": fake + status} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o700); err != nil { //nolint:gosec // G306: scripts must be executable
			t.Fatal(err)
		}
	}
	sh, lerr := exec.LookPath("sh")
	if lerr != nil {
		t.Skip(lerr)
	}
	cmd := exec.Command(sh, "-c", script)
	cmd.Dir = "/"
	cmd.Env = []string{"PATH=" + dir + ":/usr/bin:/bin"}
	b, err := cmd.CombinedOutput()
	l, rerr := os.ReadFile(logFile)
	if rerr != nil && !os.IsNotExist(rerr) {
		t.Fatal(rerr)
	}
	return string(l), string(b), err
}

func TestMountDirsScript(t *testing.T) {
	mounts := testBootConfig().Mounts
	mounts = append(mounts,
		vm.Mount{Source: "/src/x", Target: `/home/agent/it's$x/y`, ReadOnly: true, Sandbox: true, Tag: "brig3"})
	script, err := MountDirsScript(mounts, mounts[3])
	if err != nil {
		t.Fatal(err)
	}
	want := "install\n-d\n-m\n0700\n/home/agent/it's$x\n\n" +
		"mkdir\n-p\n/home/agent/it's$x/y\n\n"
	if got := runScript(t, script, false); got != want {
		t.Errorf("dirs script ran\n%s\nwant\n%s", got, want)
	}
	// Root creates a target outside home.
	if script, err := MountDirsScript(mounts, mounts[0]); err != nil || script != "" {
		t.Errorf("MountDirsScript(%s) = %q, %v", mounts[0].Target, script, err)
	}
}

// liveMounts returns the test VM's mounts plus one at target.
func liveMounts(target string) ([]vm.Mount, vm.Mount) {
	m := vm.Mount{Source: "/src/x", Target: target, ReadOnly: true, Sandbox: true, Tag: "brig3"}
	return append(testBootConfig().Mounts, m), m
}

func TestMountScript(t *testing.T) {
	base := t.TempDir()
	mounts, m := liveMounts(base + "/a/it's")
	script, err := MountScript(mounts, m)
	if err != nil {
		t.Fatal(err)
	}
	log, out, err := runFakes(t, script, false)
	if err != nil {
		t.Fatalf("%v: %s\n%s", err, out, script)
	}
	want := "mountpoint " + m.Target + " -q .\n" +
		"mount " + m.Target + " -c -t virtiofs -o ro,context=system_u:object_r:container_file_t:s0 brig3 .\n"
	if log != want {
		t.Errorf("mount script ran\n%s\nwant\n%s", log, want)
	}
	if fi, err := os.Stat(m.Target); err != nil || !fi.IsDir() {
		t.Errorf("mount script made no target: %v", err)
	}
	// A target that is mounted already stays as it is.
	if log, out, err := runFakes(t, script, true); err != nil || log != "mountpoint "+m.Target+" -q .\n" {
		t.Errorf("mount script on a mount point ran\n%s(%v: %s)", log, err, out)
	}
}

func TestMountScriptRefusesSymlinks(t *testing.T) {
	base := t.TempDir()
	for _, d := range []string{"etc", "real"} {
		if err := os.Mkdir(filepath.Join(base, d), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	for link, dest := range map[string]string{"parent": "etc", "target": "etc"} {
		if err := os.Symlink(filepath.Join(base, dest), filepath.Join(base, link)); err != nil {
			t.Fatal(err)
		}
	}
	for _, target := range []string{base + "/parent/x", base + "/target"} {
		mounts, m := liveMounts(target)
		script, err := MountScript(mounts, m)
		if err != nil {
			t.Fatal(err)
		}
		log, out, err := runFakes(t, script, false)
		if err == nil || !strings.Contains(out, "brig: refusing") || log != "" {
			t.Errorf("mount script at %s: %v: %s\nran\n%s", target, err, out, log)
		}
		if entries, _ := os.ReadDir(filepath.Join(base, "etc")); len(entries) != 0 {
			t.Errorf("mount script at %s created %v", target, entries)
		}
	}
	// Below home, User makes the directories, so root must not.
	mounts, m := liveMounts("/home/agent/a/b")
	script, err := MountScript(mounts, m)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(script, "mkdir") || !strings.Contains(script, "cd -P '/home/agent/a/b'") {
		t.Errorf("mount script below home:\n%s", script)
	}
}

func TestEnterDir(t *testing.T) {
	base := t.TempDir()
	if err := os.Mkdir(filepath.Join(base, "real"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("real", filepath.Join(base, "link")); err != nil {
		t.Fatal(err)
	}
	enter := func(dir string) (string, error) {
		_, out, err := runFakes(t, "set -e\n"+refuseLink+enterDir(dir)+"pwd -P\n", false)
		return out, err
	}
	if out, err := enter(base + "/real"); err != nil || out != base+"/real\n" {
		t.Errorf("enter real: %v: %s", err, out)
	}
	if out, err := enter(base + "/link"); err == nil || !strings.Contains(out, "brig: refusing "+base+"/link") {
		t.Errorf("enter link: %v: %s", err, out)
	}
}

func TestMountScriptRejects(t *testing.T) {
	mounts := testBootConfig().Mounts
	if _, err := MountScript(mounts, vm.Mount{Source: "/src/d", Target: "/mnt/d", Tag: "brig9"}); err == nil {
		t.Error("MountScript accepted a mount that is not the VM's")
	}
	if _, err := MountDirsScript(mounts, vm.Mount{Source: "/src/d", Target: "/home/agent/d", Tag: "brig9"}); err == nil {
		t.Error("MountDirsScript accepted a mount that is not the VM's")
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
	base := t.TempDir()
	if err := os.MkdirAll(filepath.Join(base, "p", "it's"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("p", filepath.Join(base, "link")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/", filepath.Join(base, "p", "root")); err != nil {
		t.Fatal(err)
	}
	unmount := func(target string, mounted bool) (string, string, error) {
		script, err := UnmountScript(vm.Mount{Source: "/src/x", Target: target, Tag: "brig3"})
		if err != nil {
			t.Fatal(err)
		}
		return runFakes(t, script, mounted)
	}
	p := base + "/p"
	if log, out, err := unmount(p+"/it's", true); err != nil || log != "mountpoint "+p+" -q it's\numount "+p+" -c it's\n" {
		t.Errorf("unmount ran\n%s(%v: %s)", log, err, out)
	}
	if log, out, err := unmount(p+"/it's", false); err != nil || log != "mountpoint "+p+" -q it's\n" {
		t.Errorf("unmount without a mount ran\n%s(%v: %s)", log, err, out)
	}
	if log, out, err := unmount(base+"/gone/x", true); err != nil || log != "" {
		t.Errorf("unmount below a missing directory ran\n%s(%v: %s)", log, err, out)
	}
	if log, out, err := unmount(p+"/root", true); err != nil || log != "" {
		t.Errorf("unmount of a symbolic link ran\n%s(%v: %s)", log, err, out)
	}
	if log, out, err := unmount(base+"/link/it's", true); err == nil || !strings.Contains(out, "brig: refusing") || log != "" {
		t.Errorf("unmount below a symbolic link ran\n%s(%v: %s)", log, err, out)
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
