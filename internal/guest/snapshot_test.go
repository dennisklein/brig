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
)

// snapshotVM is a VM's home for the snapshot scripts: a workspace volume
// at mp, and fake podman, btrfs, stat and cp in front of the real tools.
// btrfs makes plain directories for subvolumes and copies for snapshots,
// stat says that everything is on btrfs and that a directory holding
// .subvol is a subvolume, and cp leaves out --reflink, which the test's
// filesystem may lack.
type snapshotVM struct {
	home, mp, bin string
	env           []string
}

func newSnapshotVM(t *testing.T) *snapshotVM {
	t.Helper()
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip(err)
	}
	stat, err := exec.LookPath("stat")
	if err != nil {
		t.Skip(err)
	}
	cp, err := exec.LookPath("cp")
	if err != nil {
		t.Skip(err)
	}
	dir := t.TempDir()
	vm := &snapshotVM{home: filepath.Join(dir, "home"), bin: filepath.Join(dir, "bin")}
	vm.mp = filepath.Join(vm.home, ".local/share/containers/storage/volumes/openshell-sandbox-1-workspace/_data")
	for _, d := range []string{vm.bin, vm.mp} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	fakes := map[string]string{
		"podman": `case "$1 $2" in
"volume inspect") [ "$5" = openshell-sandbox-1-workspace ] && echo "$FAKE_MP" ;;
"volume exists") [ "$3" = brig1 ] ;;
"image inspect") [ "$5" = localhost/new:1 ] && echo "sha256:1 /sandbox" ;;
unshare*) shift; exec "$@" ;;
*) exit 9 ;;
esac`,
		"btrfs": `case "$1 $2" in
"subvolume create") mkdir "$3" && touch "$3/.subvol" ;;
"subvolume snapshot") ` + cp + ` -a "$3" "$4" ;;
"filesystem du") shift 4; echo "     Total   Exclusive  Set shared  Filename"; for p; do echo "100 40 60 $p"; done ;;
*) exit 9 ;;
esac`,
		"stat": `if [ "$1" = -f ]; then echo "${FAKE_FS:-btrfs}"; exit; fi
if [ "$2" = %i ]; then if [ -e "$3/.subvol" ]; then echo 256; else echo 2; fi; exit; fi
exec ` + stat + ` "$@"`,
		"cp": `[ -n "${FAKE_CP_FAIL:-}" ] && exit 1
for a; do shift; [ "$a" = --reflink=always ] || set -- "$@" "$a"; done
exec ` + cp + ` "$@"`,
	}
	for name, body := range fakes {
		if err := os.WriteFile(filepath.Join(vm.bin, name), []byte("#!/bin/sh\n"+body+"\n"), 0o700); err != nil { //nolint:gosec // G306: scripts must be executable
			t.Fatal(err)
		}
	}
	vm.env = []string{"HOME=" + vm.home, "PATH=" + vm.bin + ":" + os.Getenv("PATH"), "FAKE_MP=" + vm.mp}
	return vm
}

// run runs script as User would, with the extra environment env, and
// returns its standard output, failing the test if it fails unless fail.
func (vm *snapshotVM) run(t *testing.T, script string, fail bool, env ...string) string {
	t.Helper()
	cmd := exec.Command("sh", "-c", script)
	cmd.Env = append(slices.Clone(vm.env), env...)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if (err != nil) != fail {
		t.Fatalf("script: %v, want failure %t: %s\n%s", err, fail, out, stderr.String())
	}
	return string(out) + stderr.String()
}

func write(t *testing.T, files map[string]string) {
	t.Helper()
	for p, content := range files {
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

func read(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestSnapshotCreateAndRestore(t *testing.T) {
	vm := newSnapshotVM(t)
	write(t, map[string]string{
		vm.mp + "/work/main.go":              "package main\n",
		vm.mp + "/.claude/.credentials.json": "{}",
		vm.mp + "/.pi/agent/settings.json":   "old",
	})
	if err := os.Symlink("/etc/hostname", vm.mp+"/link"); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(vm.mp, 0o700); err != nil { //nolint:gosec // G302: a directory, which needs x
		t.Fatal(err)
	}
	script, err := SnapshotCreateScript("openshell-sandbox-1-workspace", "default/webapp/2026-10-14T15:02:11Z", `{"name":"it's"}`)
	if err != nil {
		t.Fatal(err)
	}
	out := vm.run(t, script, false)
	snap := filepath.Join(vm.home, SnapshotDir, "default/webapp/2026-10-14T15:02:11Z")
	if out != "credential .claude/.credentials.json\n" {
		t.Errorf("create printed %q", out)
	}
	if got := read(t, snap+"/manifest.json"); got != `{"name":"it's"}`+"\n" {
		t.Errorf("manifest %q", got)
	}
	if got := read(t, snap+"/data/work/main.go"); got != "package main\n" {
		t.Errorf("copy %q", got)
	}
	if l, err := os.Readlink(snap + "/data/link"); err != nil || l != "/etc/hostname" {
		t.Errorf("link %q, %v", l, err)
	}
	if fi, err := os.Stat(snap + "/data"); err != nil || fi.Mode().Perm() != 0o700 {
		t.Errorf("the copy's root: %v, %v", fi, err)
	}
	if _, err := os.Stat(snap + ".partial"); !os.IsNotExist(err) {
		t.Errorf("partial snapshot left: %v", err)
	}
	// A snapshot of that name exists already.
	if out := vm.run(t, script, true); !strings.Contains(out, "exists") {
		t.Errorf("second create printed %q", out)
	}

	// The sandbox moves on; a restore brings back the snapshot, but not
	// the reset path, and swaps in a subvolume.
	write(t, map[string]string{vm.mp + "/work/new.go": "new", vm.mp + "/work/main.go": "changed"})
	script, err = SnapshotRestoreScript("openshell-sandbox-1-workspace", "default/webapp/2026-10-14T15:02:11Z", []string{".pi/agent/settings.json"})
	if err != nil {
		t.Fatal(err)
	}
	if out := vm.run(t, script, false); out != "" {
		t.Errorf("restore printed %q", out)
	}
	if got := read(t, vm.mp+"/work/main.go"); got != "package main\n" {
		t.Errorf("restored %q", got)
	}
	for _, gone := range []string{"/work/new.go", "/.pi/agent/settings.json", "/../_data.brig-new", "/../_data.brig-old"} {
		if _, err := os.Lstat(vm.mp + gone); !os.IsNotExist(err) {
			t.Errorf("%s is there: %v", gone, err)
		}
	}
	if _, err := os.Stat(vm.mp + "/.subvol"); err != nil {
		t.Errorf("the restored /sandbox is no subvolume: %v", err)
	}

	// The next snapshot of a subvolume is a btrfs snapshot.
	script, _ = SnapshotCreateScript("openshell-sandbox-1-workspace", "default/webapp/2026-10-14T16:00:00Z", "{}")
	vm.run(t, script, false, "FAKE_CP_FAIL=1")
}

func TestSnapshotCreateRemovesAPartialSnapshot(t *testing.T) {
	vm := newSnapshotVM(t)
	write(t, map[string]string{vm.mp + "/f": "x"})
	script, _ := SnapshotCreateScript("openshell-sandbox-1-workspace", "team/ci/2026-10-14T15:02:11Z", "{}")
	vm.run(t, script, true, "FAKE_CP_FAIL=1")
	entries, err := os.ReadDir(filepath.Join(vm.home, SnapshotDir, "team/ci"))
	if err != nil || len(entries) != 0 {
		t.Errorf("left %v, %v", entries, err)
	}
	if out := vm.run(t, script, true, "FAKE_FS=ext2/ext3"); !strings.Contains(out, NotBtrfs) {
		t.Errorf("on ext4: %q", out)
	}
}

func TestSnapshotRestoreNeverFollowsLinksOut(t *testing.T) {
	vm := newSnapshotVM(t)
	outside := filepath.Join(vm.home, "precious")
	write(t, map[string]string{vm.mp + "/f": "x", outside + "/agent/settings.json": "keep"})
	if err := os.Symlink(outside, vm.mp+"/.pi"); err != nil {
		t.Fatal(err)
	}
	script, _ := SnapshotCreateScript("openshell-sandbox-1-workspace", "default/webapp/t1", "{}")
	vm.run(t, script, false)
	write(t, map[string]string{vm.mp + "/g": "y"})
	script, _ = SnapshotRestoreScript("openshell-sandbox-1-workspace", "default/webapp/t1", []string{".pi/agent/settings.json"})
	if out := vm.run(t, script, true); !strings.Contains(out, "leads out of /sandbox") {
		t.Errorf("restore printed %q", out)
	}
	if got := read(t, outside+"/agent/settings.json"); got != "keep" {
		t.Errorf("the file outside is %q", got)
	}
	// A failed restore leaves /sandbox as it was.
	if got := read(t, vm.mp+"/g"); got != "y" {
		t.Errorf("/sandbox changed: %q", got)
	}
	if _, err := os.Stat(filepath.Dir(vm.mp) + "/_data.brig-new"); !os.IsNotExist(err) {
		t.Errorf("the new copy is left: %v", err)
	}
	if _, err := SnapshotRestoreScript("v", "default/webapp/t1", []string{"../x"}); err == nil {
		t.Error("a reset path out of /sandbox was accepted")
	}
}

func TestSnapshotRestoreChangesTheOwner(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("needs root to change owners")
	}
	vm := newSnapshotVM(t)
	write(t, map[string]string{vm.mp + "/a": "a", vm.mp + "/b": "b"})
	for _, p := range []string{vm.mp, vm.mp + "/a"} {
		if err := os.Chown(p, 1000, 1000); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Chown(vm.mp+"/b", 1234, 1234); err != nil {
		t.Fatal(err)
	}
	script, _ := SnapshotCreateScript("openshell-sandbox-1-workspace", "default/webapp/t1", "{}")
	vm.run(t, script, false)
	// The new image's user is 1001.
	if err := os.Chown(vm.mp, 1001, 1001); err != nil {
		t.Fatal(err)
	}
	script, _ = SnapshotRestoreScript("openshell-sandbox-1-workspace", "default/webapp/t1", nil)
	// b, and the fake's .subvol, which root made.
	if out := vm.run(t, script, false); out != "others 2\n" {
		t.Errorf("restore printed %q", out)
	}
	owner := func(p string) string { return strings.TrimSpace(vm.run(t, "stat -c %u:%g "+p, false)) }
	if owner(vm.mp) != "1001:1001" || owner(vm.mp+"/a") != "1001:1001" || owner(vm.mp+"/b") != "1234:1234" {
		t.Errorf("owners %s %s %s", owner(vm.mp), owner(vm.mp+"/a"), owner(vm.mp+"/b"))
	}
}

func TestSnapshotListAndDelete(t *testing.T) {
	vm := newSnapshotVM(t)
	for _, rel := range []string{"default/webapp/t1", "default/webapp/t2", "team/ci/t1"} {
		script, _ := SnapshotCreateScript("openshell-sandbox-1-workspace", rel, `{"sandbox":"`+rel+`"}`)
		vm.run(t, script, false)
	}
	if err := os.MkdirAll(filepath.Join(vm.home, SnapshotDir, "team/ci/t2.partial/data"), 0o700); err != nil {
		t.Fatal(err)
	}
	want := `snapshot default/webapp/t1
{"sandbox":"default/webapp/t1"}
snapshot default/webapp/t2
{"sandbox":"default/webapp/t2"}
snapshot team/ci/t1
{"sandbox":"team/ci/t1"}
`
	if got := vm.run(t, SnapshotListScript(false), false); got != want {
		t.Errorf("list printed\n%s\nwant\n%s", got, want)
	}
	if got := vm.run(t, SnapshotListScript(true), false); !strings.HasSuffix(got, "100 40 60 team/ci/t1/data\n") {
		t.Errorf("list with sizes printed\n%s", got)
	}
	script, err := SnapshotDeleteScript([]string{"team/ci/t1", "default/webapp/t1"})
	if err != nil {
		t.Fatal(err)
	}
	vm.run(t, script, false)
	got := strings.TrimSpace(vm.run(t, "cd \"$HOME\"/"+SnapshotDir+" && find . -maxdepth 3 | sort", false))
	if got != ".\n./default\n./default/webapp\n./default/webapp/t2\n./team\n./team/ci\n./team/ci/t2.partial" {
		t.Errorf("left\n%s", got)
	}
	if _, err := SnapshotDeleteScript([]string{"../x/y"}); err == nil {
		t.Error("a path out of the snapshots was accepted")
	}
}

func TestSnapshotPlanScript(t *testing.T) {
	vm := newSnapshotVM(t)
	write(t, map[string]string{vm.mp + "/f": "1234"})
	got := vm.run(t, SnapshotPlanScript("openshell-sandbox-1-workspace", []string{"localhost/new:1", "localhost/gone:1"}, []string{"brig1", "brig2"}, false), false)
	want := "fs btrfs\nimage localhost/new:1 sha256:1 /sandbox\nimage localhost/gone:1 - -\nmount brig1 yes\nmount brig2 no\nvolume yes\n"
	if got != want {
		t.Errorf("plan printed\n%s\nwant\n%s", got, want)
	}
	if got := vm.run(t, SnapshotPlanScript("openshell-sandbox-2-workspace", nil, nil, true), false); got != "fs btrfs\nvolume no\n" {
		t.Errorf("plan printed\n%s", got)
	}
}

func TestCheckSnapshotPath(t *testing.T) {
	for _, ok := range []string{"default/webapp/2026-10-14T15:02:11Z", "team-a/ci/t1"} {
		if err := CheckSnapshotPath(ok); err != nil {
			t.Errorf("%s: %v", ok, err)
		}
	}
	for _, bad := range []string{"", "a/b", "a/b/c/d", "../b/c", "a/./c", "a/b/c.partial", "a/b/-c", "a /b/c"} {
		if err := CheckSnapshotPath(bad); err == nil {
			t.Errorf("%q was accepted", bad)
		}
	}
}
