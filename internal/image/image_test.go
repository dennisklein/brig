// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

package image

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/dennisklein/brig/internal/guest"
	"github.com/dennisklein/brig/internal/paths"
)

// manifestJSON is a manifest in the shape mkosi writes for ManifestFormat=json.
const manifestJSON = `{
  "manifest_version": 1,
  "config": {
    "name": "image",
    "distribution": "fedora",
    "architecture": "x86-64",
    "output_format": "disk",
    "release": "44"
  },
  "packages": [
    {"type": "rpm", "name": "bash", "version": "5.3.0-2.fc44", "architecture": "x86_64", "size": 8616030},
    {"type": "rpm", "name": "openshell", "version": "0.1.2-1.fc44", "architecture": "x86_64", "size": 41000000},
    {"type": "rpm", "name": "openshell-gateway", "version": "0.1.2-1.fc44", "architecture": "x86_64", "size": 52000000}
  ],
  "extension": {}
}`

// buildOK is the build step of a fake mkosi that succeeds.
const buildOK = `printf 'raw disk' >"$out/base.raw"
cat >"$out/base.manifest" <<'EOF'
` + manifestJSON + `
EOF`

type fixture struct {
	s Store
	// mkosi returns the recorded mkosi calls, one line of arguments each.
	mkosi func() []string
	// configs are the directories writeConfig wrote to and what it wrote.
	dirs    []string
	configs []guest.ImageConfig
}

// newFixture returns a store with a fake mkosi that prints version for
// --version and otherwise runs build with $out and $work set to its output
// and workspace directories.
func newFixture(t *testing.T, version, build string) *fixture {
	t.Helper()
	bin := t.TempDir()
	calls := filepath.Join(bin, "calls")
	script := `#!/bin/sh
echo "$*" >>'` + calls + `'
if [ "$1" = --version ]; then
	echo '` + version + `'
	exit 0
fi
for a in "$@"; do
	case "$a" in
	--output-directory=*) out="${a#*=}" ;;
	--workspace-directory=*) work="${a#*=}" ;;
	esac
done
` + build + "\n"
	mkosi := filepath.Join(bin, "mkosi")
	if err := os.WriteFile(mkosi, []byte(script), 0o700); err != nil { //nolint:gosec // G306: the fake must be executable
		t.Fatal(err)
	}
	f := &fixture{s: NewStore(paths.Dirs{Data: t.TempDir()})}
	f.s.mkosi = mkosi
	f.s.now = func() time.Time { return time.Date(2026, 10, 7, 14, 0, 0, 5e8, time.FixedZone("CEST", 2*3600)) }
	f.s.writeConfig = func(dir string, c guest.ImageConfig) error {
		f.dirs, f.configs = append(f.dirs, dir), append(f.configs, c)
		return os.WriteFile(filepath.Join(dir, "mkosi.conf"), []byte("[Config]\n"), 0o600)
	}
	f.s.convert = func(_ context.Context, src, dst string) error {
		data, err := os.ReadFile(src)
		if err != nil {
			return err
		}
		return os.WriteFile(dst, append([]byte("qcow2 of "), data...), 0o600) //nolint:gosec // G703: dst is in the test's temporary directory
	}
	f.mkosi = func() []string {
		data, err := os.ReadFile(calls)
		if err != nil {
			t.Fatal(err)
		}
		return strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
	}
	return f
}

// entries lists the names in dir.
func entries(t *testing.T, dir string) []string {
	t.Helper()
	des, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, de := range des {
		names = append(names, de.Name())
	}
	return names
}

var testOptions = BuildOptions{
	FedoraRelease: 44,
	RepoFile:      []byte("[brig]\n"),
	RepoKey:       []byte("-----BEGIN PGP PUBLIC KEY BLOCK-----\n"),
}

func TestBuild(t *testing.T) {
	f := newFixture(t, "mkosi 26.1", buildOK)
	root := f.s.root
	// A build that died without cleaning up.
	if err := os.MkdirAll(filepath.Join(root, ".build-123", "workspace"), 0o700); err != nil {
		t.Fatal(err)
	}
	cache := t.TempDir()
	o := testOptions
	o.OpenShellVersion = "0.1.2"
	img, _, err := f.s.Build(context.Background(), cache, o)
	if err != nil {
		t.Fatal(err)
	}

	want := Image{
		ID:               "f44-openshell0.1.2-20261007T120000Z",
		FedoraRelease:    44,
		OpenShellVersion: "0.1.2",
		CreatedAt:        time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC),
	}
	if !reflect.DeepEqual(img, want) {
		t.Fatalf("Build() = %+v, want %+v", img, want)
	}
	if got, err := f.s.Get(img.ID); err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("Get() = %+v, %v; want %+v", got, err, want)
	}
	if got := entries(t, root); !reflect.DeepEqual(got, []string{want.ID}) {
		t.Fatalf("images directory holds %q, want only the new image", got)
	}
	if got := entries(t, f.s.dir(want.ID)); !reflect.DeepEqual(got, []string{"base.qcow2", "image.json", "manifest.json"}) {
		t.Fatalf("image directory holds %q", got)
	}
	fi, err := os.Stat(f.s.Path(want.ID))
	if err != nil || fi.Mode().Perm() != 0o444 {
		t.Fatalf("base.qcow2: %v, %v; want mode 0444", fi, err)
	}
	if data, err := os.ReadFile(f.s.Path(want.ID)); err != nil || string(data) != "qcow2 of raw disk" {
		t.Fatalf("base.qcow2 = %q, %v; want the converted base.raw", data, err)
	}

	wantConfig := guest.ImageConfig{
		FedoraRelease: 44, OpenShellVersion: "0.1.2", RepoFile: o.RepoFile, RepoKey: o.RepoKey,
	}
	if len(f.configs) != 1 || !reflect.DeepEqual(f.configs[0], wantConfig) {
		t.Fatalf("writeConfig got %+v, want %+v", f.configs, wantConfig)
	}
	tmp := filepath.Dir(f.dirs[0])
	if filepath.Dir(tmp) != root || !strings.HasPrefix(filepath.Base(tmp), ".build-") {
		t.Fatalf("mkosi configuration written to %s, want a build directory in %s", f.dirs[0], root)
	}
	wantCalls := []string{
		"--version",
		"-C " + tmp + "/mkosi --output-directory=" + tmp + "/output --package-cache-dir=" + cache +
			" --workspace-directory=" + tmp + "/workspace --cache-only=never -f build",
	}
	if got := f.mkosi(); !reflect.DeepEqual(got, wantCalls) {
		t.Fatalf("mkosi calls:\n%q\nwant:\n%q", got, wantCalls)
	}
}

func TestBuildFailureCleansUp(t *testing.T) {
	f := newFixture(t, "mkosi 26", `mkdir -p "$work/mkosi-workspace-x/root/usr/bin"
chmod 555 "$work/mkosi-workspace-x/root/usr" "$work/mkosi-workspace-x/root"
echo 'mkosi: build failed' >&2
exit 1`)
	var log bytes.Buffer
	o := testOptions
	o.Log = &log
	_, _, err := f.s.Build(context.Background(), t.TempDir(), o)
	if err == nil || !strings.Contains(err.Error(), "mkosi build: exit status 1") {
		t.Fatalf("Build() error = %v", err)
	}
	if !strings.Contains(log.String(), "mkosi: build failed") {
		t.Errorf("log = %q, want mkosi's output", log.String())
	}
	if got := entries(t, f.s.root); len(got) != 0 {
		t.Fatalf("images directory holds %q after a failed build", got)
	}
}

func TestBuildCancelled(t *testing.T) {
	f := newFixture(t, "mkosi 26", "exec sleep 30")
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, _, err := f.s.Build(ctx, t.TempDir(), testOptions)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Build() error = %v, want context.DeadlineExceeded", err)
	}
	if d := time.Since(start); d > 10*time.Second {
		t.Fatalf("Build() took %v to stop", d)
	}
	if got := entries(t, f.s.root); len(got) != 0 {
		t.Fatalf("images directory holds %q after a cancelled build", got)
	}
}

func TestBuildWaitsForLock(t *testing.T) {
	f := newFixture(t, "mkosi 26", buildOK)
	if err := paths.EnsurePrivate(f.s.root); err != nil {
		t.Fatal(err)
	}
	held, err := os.Open(f.s.root)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = held.Close() }()
	if err := unix.Flock(int(held.Fd()), unix.LOCK_EX); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	var log bytes.Buffer
	o := testOptions
	o.Log = &log
	if _, _, err := f.s.Build(ctx, t.TempDir(), o); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Build() error = %v, want context.DeadlineExceeded", err)
	}
	if !strings.Contains(log.String(), "Waiting for another image build") {
		t.Errorf("log = %q, want a note about waiting", log.String())
	}
	if f.configs != nil {
		t.Error("Build() started without the lock")
	}
}

func TestBuildIfNone(t *testing.T) {
	f := newFixture(t, "mkosi 26", buildOK)
	o := testOptions
	o.IfNone = true
	first, built, err := f.s.Build(context.Background(), t.TempDir(), o)
	if err != nil {
		t.Fatal(err)
	}
	if !built {
		t.Fatal("Build() did not report building the first image")
	}
	// As if this build had waited for the first one.
	f.s.now = func() time.Time { return time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC) }
	again, built, err := f.s.Build(context.Background(), t.TempDir(), o)
	if err != nil || built || again != first {
		t.Fatalf("Build() = %+v, built %t, %v; want the existing image %+v, not built", again, built, err, first)
	}
	if len(f.configs) != 1 {
		t.Fatalf("mkosi ran %d times, want once", len(f.configs))
	}
	if imgs, err := f.s.List(); err != nil || len(imgs) != 1 {
		t.Fatalf("List() = %+v, %v; want one image", imgs, err)
	}
}

func TestBuildChecksMkosiVersion(t *testing.T) {
	for _, tt := range []struct{ version, wantErr string }{
		{"mkosi 26", ""},
		{"mkosi 28~devel", ""},
		{"mkosi 25.3", "mkosi 25.3 is too old: brig needs mkosi 26 or newer"},
		{"mkosi devel", `cannot parse mkosi version "mkosi devel"`},
	} {
		f := newFixture(t, tt.version, "exit 1")
		_, _, err := f.s.Build(context.Background(), t.TempDir(), testOptions)
		if tt.wantErr == "" {
			if err == nil || !strings.HasPrefix(err.Error(), "mkosi build:") {
				t.Errorf("%s: error = %v, want a failing build", tt.version, err)
			}
			continue
		}
		if err == nil || err.Error() != tt.wantErr {
			t.Errorf("%s: error = %v, want %q", tt.version, err, tt.wantErr)
		}
		if f.configs != nil {
			t.Errorf("%s: Build() went ahead", tt.version)
		}
	}

	s := NewStore(paths.Dirs{Data: t.TempDir()})
	s.mkosi = "brig-test-no-such-mkosi"
	if _, _, err := s.Build(context.Background(), t.TempDir(), testOptions); err == nil || !strings.Contains(err.Error(), "not installed") {
		t.Errorf("Build() without mkosi: error = %v", err)
	}
}

func TestBuildRejectsInvalidOptions(t *testing.T) {
	f := newFixture(t, "mkosi 26", buildOK)
	if _, _, err := f.s.Build(context.Background(), t.TempDir(), BuildOptions{}); err == nil {
		t.Fatal("Build() without a Fedora release succeeded")
	}
	t.Chdir(t.TempDir())
	if _, _, err := f.s.Build(context.Background(), "", testOptions); err == nil || !strings.Contains(err.Error(), "no package cache directory") {
		t.Fatalf("Build() without a package cache directory: error = %v", err)
	}
	if f.configs != nil {
		t.Fatal("Build() went ahead with invalid options")
	}
}

func TestGatewayVersion(t *testing.T) {
	for _, tt := range []struct{ manifest, want, wantErr string }{
		{manifest: manifestJSON, want: "0.1.2"},
		{
			manifest: `{"manifest_version": 1, "packages": [{"name": "openshell-gateway", "version": "1:0.2.0~rc1-3.fc45"}]}`,
			want:     "0.2.0~rc1",
		},
		{
			manifest: `{"manifest_version": 1, "packages": [{"name": "openshell", "version": "0.1.2-1.fc44"}]}`,
			wantErr:  "openshell-gateway is not installed",
		},
		{manifest: `{"manifest_version": 2, "packages": []}`, wantErr: "unsupported manifest version 2"},
		{manifest: `Packages: 1`, wantErr: "invalid character"},
	} {
		file := filepath.Join(t.TempDir(), "base.manifest")
		if err := os.WriteFile(file, []byte(tt.manifest), 0o600); err != nil {
			t.Fatal(err)
		}
		got, err := gatewayVersion(file)
		if tt.wantErr != "" {
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("gatewayVersion(%s) error = %v, want %q", tt.manifest, err, tt.wantErr)
			}
			continue
		}
		if err != nil || got != tt.want {
			t.Errorf("gatewayVersion(%s) = %q, %v; want %q", tt.manifest, got, err, tt.want)
		}
	}
	if _, err := gatewayVersion(filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Error("gatewayVersion() of a missing manifest succeeded")
	}
}

// put stores a record for img as Build would.
func put(t *testing.T, s Store, img Image) {
	t.Helper()
	if err := os.MkdirAll(s.dir(img.ID), 0o700); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(img)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(s.dir(img.ID), "image.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(s.Path(img.ID), nil, 0o400); err != nil {
		t.Fatal(err)
	}
}

func TestStore(t *testing.T) {
	s := NewStore(paths.Dirs{Data: t.TempDir()})
	if images, err := s.List(); err != nil || len(images) != 0 {
		t.Fatalf("List() on a missing directory = %v, %v", images, err)
	}
	if _, err := s.Newest(); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Newest() without images: error = %v, want ErrNotFound", err)
	}

	day := func(d int) time.Time { return time.Date(2026, 10, d, 12, 0, 0, 0, time.UTC) }
	older := Image{ID: "f44-openshell0.1.1-20261001T120000Z", FedoraRelease: 44, OpenShellVersion: "0.1.1", CreatedAt: day(1)}
	newer := Image{ID: "f44-openshell0.1.2-20261007T120000Z", FedoraRelease: 44, OpenShellVersion: "0.1.2", CreatedAt: day(7)}
	put(t, s, older)
	put(t, s, newer)
	// Skipped: a build directory, a foreign directory, a file and an image
	// without a record.
	for _, d := range []string{".build-1", "not-an-image", "f44-openshell0.1.3-20261008T120000Z"} {
		if err := os.MkdirAll(s.dir(d), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(s.root, "f44-openshell0.1.4-20261009T120000Z"), nil, 0o600); err != nil {
		t.Fatal(err)
	}

	images, err := s.List()
	if err != nil || !reflect.DeepEqual(images, []Image{newer, older}) {
		t.Fatalf("List() = %+v, %v; want newest first", images, err)
	}
	if img, err := s.Newest(); err != nil || !reflect.DeepEqual(img, newer) {
		t.Fatalf("Newest() = %+v, %v", img, err)
	}
	if img, err := s.Get(older.ID); err != nil || !reflect.DeepEqual(img, older) {
		t.Fatalf("Get() = %+v, %v", img, err)
	}
	if want := filepath.Join(s.root, newer.ID, "base.qcow2"); s.Path(newer.ID) != want {
		t.Fatalf("Path() = %s, want %s", s.Path(newer.ID), want)
	}

	if err := s.Remove(newer.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(s.dir(newer.ID)); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("image directory still exists after Remove: %v", err)
	}
	if _, err := s.Get(newer.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get() after Remove: error = %v, want ErrNotFound", err)
	}
	if err := s.Remove(newer.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Remove() of a missing image: error = %v, want ErrNotFound", err)
	}
	// An image whose removal was interrupted after its record went.
	if err := s.Remove("f44-openshell0.1.3-20261008T120000Z"); err != nil {
		t.Fatal(err)
	}
}

func TestInvalidIDs(t *testing.T) {
	s := NewStore(paths.Dirs{Data: t.TempDir()})
	for _, id := range []string{"", "..", "../f44-openshell0.1.2-20261007T120000Z", "f44-openshell0.1.2", "f44-openshell/x-20261007T120000Z", "f0-openshell0.1.2-20261007T120000Z"} {
		if _, err := s.Get(id); err == nil || errors.Is(err, ErrNotFound) {
			t.Errorf("Get(%q) error = %v, want an invalid ID", id, err)
		}
		if err := s.Remove(id); err == nil || errors.Is(err, ErrNotFound) {
			t.Errorf("Remove(%q) error = %v, want an invalid ID", id, err)
		}
	}
}

func TestGetRejectsMismatchedRecord(t *testing.T) {
	s := NewStore(paths.Dirs{Data: t.TempDir()})
	img := Image{ID: "f44-openshell0.1.2-20261007T120000Z"}
	put(t, s, img)
	other := "f44-openshell0.1.2-20261007T130000Z"
	if err := os.Rename(s.dir(img.ID), s.dir(other)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Get(other); err == nil || !strings.Contains(err.Error(), "record is for image") {
		t.Fatalf("Get() error = %v", err)
	}
}

func TestRemoveAllReadOnlyTree(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "tree")
	if err := os.MkdirAll(filepath.Join(dir, "a", "b", "c"), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, d := range []string{filepath.Join(dir, "a", "b"), filepath.Join(dir, "a")} {
		if err := os.Chmod(d, 0o500); err != nil { //nolint:gosec // G302 targets files; directories need the execute bit
			t.Fatal(err)
		}
	}
	if os.RemoveAll(dir) == nil {
		t.Skip("directory permissions are not enforced, e.g. for root")
	}
	if err := removeAll(dir); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dir); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("tree still exists: %v", err)
	}
}

func TestClearPackageCache(t *testing.T) {
	s := NewStore(paths.Dirs{Data: t.TempDir()})
	cache := filepath.Join(t.TempDir(), "mkosi")
	if n, err := s.ClearPackageCache(context.Background(), cache, nil); n != 0 || err != nil {
		t.Fatalf("missing cache: %d, %v", n, err)
	}
	pkgs := filepath.Join(cache, "fedora", "packages")
	if err := os.MkdirAll(pkgs, 0o700); err != nil {
		t.Fatal(err)
	}
	for name, size := range map[string]int{"kernel-core.rpm": 3000, "systemd.rpm": 1000} {
		if err := os.WriteFile(filepath.Join(pkgs, name), make([]byte, size), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// mkosi may leave read-only directories behind.
	if err := os.Chmod(pkgs, 0o500); err != nil { //nolint:gosec // G302 targets files; directories need the execute bit
		t.Fatal(err)
	}
	n, err := s.ClearPackageCache(context.Background(), cache, nil)
	if err != nil || n != 4000 {
		t.Fatalf("ClearPackageCache = %d, %v; want 4000", n, err)
	}
	if entries, err := os.ReadDir(cache); err != nil || len(entries) != 0 {
		t.Errorf("cache after clearing: %v, %v", entries, err)
	}
}

func TestClearPackageCacheWaitsForBuilds(t *testing.T) {
	s := NewStore(paths.Dirs{Data: t.TempDir()})
	cache := t.TempDir()
	if err := os.WriteFile(filepath.Join(cache, "x.rpm"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(s.root, 0o700); err != nil {
		t.Fatal(err)
	}
	unlock, err := lock(context.Background(), s.root, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if _, err := s.ClearPackageCache(ctx, cache, nil); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("ClearPackageCache during a build = %v, want it to wait", err)
	}
	if _, err := os.Stat(filepath.Join(cache, "x.rpm")); err != nil {
		t.Errorf("the cache changed during a build: %v", err)
	}
}
