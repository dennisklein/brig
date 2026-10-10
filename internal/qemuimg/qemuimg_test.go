// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

package qemuimg

import (
	"cmp"
	"context"
	"errors"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/GSI-HPC/go-clikit/progress/progresstest"

	"github.com/dennisklein/brig/internal/bytesize"
)

// fakeQemuImg installs a fake qemu-img that records each call as one line of
// space-separated arguments and then runs script. It returns a function that
// reports the recorded calls.
func fakeQemuImg(t *testing.T, script string) func() []string {
	t.Helper()
	dir := t.TempDir()
	calls := filepath.Join(dir, "calls")
	bin := filepath.Join(dir, "qemu-img")
	body := "#!/bin/sh\necho \"$*\" >>'" + calls + "'\n" + script + "\n"
	if err := os.WriteFile(bin, []byte(body), 0o700); err != nil { //nolint:gosec // G306: the fake must be executable
		t.Fatal(err)
	}
	old := binary
	binary = bin
	t.Cleanup(func() { binary = old })
	return func() []string {
		data, err := os.ReadFile(calls)
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		if err != nil {
			t.Fatal(err)
		}
		return strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
	}
}

// infoScript answers `qemu-img info` with a 4 GiB qcow2 image that has a
// snapshot called "taken".
const infoScript = `if [ "$1" = info ]; then
	echo '{"virtual-size": 4294967296, "filename": "x", "format": "qcow2", "snapshots": [{"id": "1", "name": "taken"}]}'
fi`

func TestCommands(t *testing.T) {
	ctx := context.Background()
	for _, tt := range []struct {
		name    string
		do      func(dir string) error
		want    []string
		wantErr string
	}{
		{
			name: "create",
			do: func(dir string) error {
				return Create(ctx, filepath.Join(dir, "data.qcow2"), 40*bytesize.GiB)
			},
			want: []string{"create -f qcow2 -o nocow=on DIR/data.qcow2 42949672960"},
		},
		{
			name:    "create zero size",
			do:      func(dir string) error { return Create(ctx, filepath.Join(dir, "data.qcow2"), 0) },
			wantErr: "zero",
		},
		{
			name: "create with a size of part of a sector",
			do:   func(dir string) error { return Create(ctx, filepath.Join(dir, "data.qcow2"), 1000) },
			want: []string{"create -f qcow2 -o nocow=on DIR/data.qcow2 1024"},
		},
		{
			name:    "create too large",
			do:      func(dir string) error { return Create(ctx, filepath.Join(dir, "data.qcow2"), math.MaxUint64) },
			wantErr: "too large",
		},
		{
			name: "overlay",
			do: func(dir string) error {
				return CreateOverlay(ctx, filepath.Join(dir, "root.qcow2"), "/images/x/base.qcow2", 20*bytesize.GiB)
			},
			want: []string{
				"info --output=json -U /images/x/base.qcow2",
				"create -f qcow2 -o nocow=on -b /images/x/base.qcow2 -F qcow2 DIR/root.qcow2 21474836480",
			},
		},
		{
			name: "overlay with the size of its base",
			do: func(dir string) error {
				return CreateOverlay(ctx, filepath.Join(dir, "root.qcow2"), "/images/x/base.qcow2", 0)
			},
			want: []string{
				"info --output=json -U /images/x/base.qcow2",
				"create -f qcow2 -o nocow=on -b /images/x/base.qcow2 -F qcow2 DIR/root.qcow2 4294967296",
			},
		},
		{
			name: "overlay with a size of part of a sector",
			do: func(dir string) error {
				return CreateOverlay(ctx, filepath.Join(dir, "root.qcow2"), "/images/x/base.qcow2", 4*bytesize.GiB+100)
			},
			want: []string{
				"info --output=json -U /images/x/base.qcow2",
				"create -f qcow2 -o nocow=on -b /images/x/base.qcow2 -F qcow2 DIR/root.qcow2 4294967808",
			},
		},
		{
			name: "overlay smaller than its base",
			do: func(dir string) error {
				return CreateOverlay(ctx, filepath.Join(dir, "root.qcow2"), "/images/x/base.qcow2", bytesize.GiB)
			},
			want:    []string{"info --output=json -U /images/x/base.qcow2"},
			wantErr: "smaller than its base",
		},
		{
			name: "convert",
			do: func(dir string) error {
				return Convert(ctx, filepath.Join(dir, "base.raw"), filepath.Join(dir, "base.qcow2"))
			},
			want: []string{"convert -f raw -O qcow2 -o nocow=on DIR/base.raw DIR/base.qcow2"},
		},
		{
			name: "grow",
			do:   func(dir string) error { return Resize(ctx, filepath.Join(dir, "d.qcow2"), 8*bytesize.GiB) },
			want: []string{
				"info --output=json -U DIR/d.qcow2",
				"resize -f qcow2 DIR/d.qcow2 8589934592",
			},
		},
		{
			name: "grow by part of a sector",
			do:   func(dir string) error { return Resize(ctx, filepath.Join(dir, "d.qcow2"), 4*bytesize.GiB+1) },
			want: []string{
				"info --output=json -U DIR/d.qcow2",
				"resize -f qcow2 DIR/d.qcow2 4294967808",
			},
		},
		{
			name: "resize to the current size",
			do:   func(dir string) error { return Resize(ctx, filepath.Join(dir, "d.qcow2"), 4*bytesize.GiB) },
			want: []string{"info --output=json -U DIR/d.qcow2"},
		},
		{
			name: "resize to within the last sector",
			do:   func(dir string) error { return Resize(ctx, filepath.Join(dir, "d.qcow2"), 4*bytesize.GiB-1) },
			want: []string{"info --output=json -U DIR/d.qcow2"},
		},
		{
			name:    "shrink",
			do:      func(dir string) error { return Resize(ctx, filepath.Join(dir, "d.qcow2"), 2*bytesize.GiB) },
			want:    []string{"info --output=json -U DIR/d.qcow2"},
			wantErr: "cannot shrink",
		},
		{
			name: "snapshot",
			do:   func(dir string) error { return Snapshot(ctx, filepath.Join(dir, "d.qcow2"), "pre-upgrade") },
			want: []string{
				"info --output=json -U DIR/d.qcow2",
				"snapshot -f qcow2 -c pre-upgrade DIR/d.qcow2",
			},
		},
		{
			name:    "snapshot with a name in use",
			do:      func(dir string) error { return Snapshot(ctx, filepath.Join(dir, "d.qcow2"), "taken") },
			want:    []string{"info --output=json -U DIR/d.qcow2"},
			wantErr: `already has a snapshot called "taken"`,
		},
		{
			name: "apply snapshot",
			do:   func(dir string) error { return ApplySnapshot(ctx, filepath.Join(dir, "d.qcow2"), "pre-upgrade") },
			want: []string{"snapshot -f qcow2 -a pre-upgrade DIR/d.qcow2"},
		},
		{
			name: "delete snapshot",
			do:   func(dir string) error { return DeleteSnapshot(ctx, filepath.Join(dir, "d.qcow2"), "pre-upgrade") },
			want: []string{"snapshot -f qcow2 -d pre-upgrade DIR/d.qcow2"},
		},
		{
			name:    "empty snapshot name",
			do:      func(dir string) error { return Snapshot(ctx, filepath.Join(dir, "d.qcow2"), "") },
			wantErr: "empty snapshot name",
		},
		{
			name:    "numeric snapshot name",
			do:      func(dir string) error { return ApplySnapshot(ctx, filepath.Join(dir, "d.qcow2"), "17") },
			wantErr: "mistaken for a snapshot ID",
		},
		{
			name: "long snapshot name",
			do: func(dir string) error {
				return DeleteSnapshot(ctx, filepath.Join(dir, "d.qcow2"), strings.Repeat("x", 256))
			},
			wantErr: "longer than 255 bytes",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			calls := fakeQemuImg(t, infoScript)
			dir := t.TempDir()
			err := tt.do(dir)
			if tt.wantErr == "" && err != nil {
				t.Fatal(err)
			}
			if tt.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tt.wantErr)) {
				t.Fatalf("error = %v, want one containing %q", err, tt.wantErr)
			}
			var want []string
			for _, c := range tt.want {
				want = append(want, strings.ReplaceAll(c, "DIR", dir))
			}
			if got := calls(); !reflect.DeepEqual(got, want) {
				t.Errorf("calls:\n%q\nwant:\n%q", got, want)
			}
		})
	}
}

func TestCreateOverlayRecordsAbsoluteBase(t *testing.T) {
	calls := fakeQemuImg(t, infoScript)
	t.Chdir(t.TempDir())
	if err := CreateOverlay(context.Background(), "root.qcow2", "base.qcow2", 0); err != nil {
		t.Fatal(err)
	}
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if got := calls()[1]; !strings.Contains(got, " -b "+filepath.Join(wd, "base.qcow2")+" ") {
		t.Fatalf("create call %q does not name the absolute base", got)
	}
}

// TestRelativePaths checks that qemu-img only gets absolute paths: it would
// take relative ones like these for a protocol or an option.
func TestRelativePaths(t *testing.T) {
	ctx := context.Background()
	calls := fakeQemuImg(t, infoScript)
	t.Chdir(t.TempDir())
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for _, err := range []error{
		Create(ctx, "-data.qcow2", bytesize.GiB),
		CreateOverlay(ctx, "nbd:root.qcow2", "json:{}", 0),
		Convert(ctx, "-f", "nbd:base.qcow2"),
		Resize(ctx, "-d.qcow2", 8*bytesize.GiB),
		Snapshot(ctx, "nbd:d.qcow2", "s"),
	} {
		if err != nil {
			t.Fatal(err)
		}
	}
	want := []string{
		"create -f qcow2 -o nocow=on WD/-data.qcow2 1073741824",
		"info --output=json -U WD/json:{}",
		"create -f qcow2 -o nocow=on -b WD/json:{} -F qcow2 WD/nbd:root.qcow2 4294967296",
		"convert -f raw -O qcow2 -o nocow=on WD/-f WD/nbd:base.qcow2",
		"info --output=json -U WD/-d.qcow2",
		"resize -f qcow2 WD/-d.qcow2 8589934592",
		"info --output=json -U WD/nbd:d.qcow2",
		"snapshot -f qcow2 -c s WD/nbd:d.qcow2",
	}
	for i := range want {
		want[i] = strings.ReplaceAll(want[i], "WD", wd)
	}
	if got := calls(); !reflect.DeepEqual(got, want) {
		t.Errorf("calls:\n%q\nwant:\n%q", got, want)
	}

	for name, err := range map[string]error{
		"Create":        Create(ctx, "", bytesize.GiB),
		"CreateOverlay": CreateOverlay(ctx, "root.qcow2", "", 0),
		"Convert":       Convert(ctx, "", "base.qcow2"),
		"Inspect":       func() error { _, err := Inspect(ctx, ""); return err }(),
	} {
		if err == nil || !strings.Contains(err.Error(), "empty image path") {
			t.Errorf("%s with an empty path: error = %v", name, err)
		}
	}
}

func TestCreateRefusesToOverwrite(t *testing.T) {
	calls := fakeQemuImg(t, infoScript)
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "data.qcow2")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	for name, err := range map[string]error{
		"Create":        Create(ctx, path, bytesize.GiB),
		"CreateOverlay": CreateOverlay(ctx, path, "/base.qcow2", 0),
		"Convert":       Convert(ctx, "/base.raw", path),
	} {
		if !errors.Is(err, fs.ErrExist) {
			t.Errorf("%s over an existing file: error = %v, want fs.ErrExist", name, err)
		}
	}
	if got := calls(); got != nil {
		t.Errorf("qemu-img ran: %q", got)
	}

	dangling := filepath.Join(t.TempDir(), "data.qcow2")
	if err := os.Symlink(filepath.Join(t.TempDir(), "elsewhere"), dangling); err != nil {
		t.Fatal(err)
	}
	if err := Create(ctx, dangling, bytesize.GiB); !errors.Is(err, fs.ErrExist) {
		t.Errorf("Create over a dangling symbolic link: error = %v, want fs.ErrExist", err)
	}
}

func TestCreatePrivate(t *testing.T) {
	fakeQemuImg(t, infoScript)
	path := filepath.Join(t.TempDir(), "data.qcow2")
	if err := Create(context.Background(), path, bytesize.GiB); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Stat(path); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("new image: %v, %v; want mode 0600", fi, err)
	}
}

func TestCreateRemovesPartialImage(t *testing.T) {
	// failScript writes to every .qcow2 argument but the base, then fails.
	const failScript = infoScript + `
[ "$1" = info ] && exit 0
for a in "$@"; do
	case "$a" in
	*/base.qcow2) ;;
	*.qcow2) printf partial >"$a" ;;
	esac
done
echo 'qemu-img: write failed' >&2
exit 1`
	for _, tt := range []struct {
		name    string
		script  string
		timeout time.Duration
		do      func(ctx context.Context, path string) error
		wantErr error
	}{
		{
			name:   "create",
			script: failScript,
			do:     func(ctx context.Context, path string) error { return Create(ctx, path, bytesize.GiB) },
		},
		{
			name:   "overlay",
			script: failScript,
			do: func(ctx context.Context, path string) error {
				return CreateOverlay(ctx, path, "/images/x/base.qcow2", 0)
			},
		},
		{
			name:   "overlay smaller than its base",
			script: infoScript,
			do: func(ctx context.Context, path string) error {
				return CreateOverlay(ctx, path, "/images/x/base.qcow2", bytesize.GiB)
			},
		},
		{
			name:   "convert",
			script: failScript,
			do:     func(ctx context.Context, path string) error { return Convert(ctx, "/base.raw", path) },
		},
		{
			name:    "cancelled convert",
			script:  `for a; do dst="$a"; done; printf partial >"$dst"; exec sleep 30`,
			timeout: 100 * time.Millisecond,
			do:      func(ctx context.Context, path string) error { return Convert(ctx, "/base.raw", path) },
			wantErr: context.DeadlineExceeded,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			fakeQemuImg(t, tt.script)
			ctx, cancel := context.WithTimeout(context.Background(), cmp.Or(tt.timeout, time.Minute))
			defer cancel()
			path := filepath.Join(t.TempDir(), "new.qcow2")
			err := tt.do(ctx, path)
			if err == nil || (tt.wantErr != nil && !errors.Is(err, tt.wantErr)) {
				t.Fatalf("error = %v, want %v", err, tt.wantErr)
			}
			if _, err := os.Lstat(path); !errors.Is(err, fs.ErrNotExist) {
				t.Fatalf("partial image left behind: %v", err)
			}
		})
	}
}

func TestInspect(t *testing.T) {
	// An ImageInfo object as qemu-img prints it for an overlay with one
	// snapshot.
	fakeQemuImg(t, `cat <<'EOF'
{
    "children": [
        {
            "name": "file",
            "info": {
                "children": [],
                "virtual-size": 393216,
                "filename": "root.qcow2",
                "format": "file",
                "actual-size": 397312,
                "format-specific": {"type": "file", "data": {}},
                "dirty-flag": false
            }
        }
    ],
    "snapshots": [
        {
            "icount": 0,
            "vm-clock-nsec": 0,
            "name": "pre-upgrade",
            "date-sec": 1759838400,
            "date-nsec": 0,
            "vm-clock-sec": 0,
            "id": "1",
            "vm-state-size": 0
        }
    ],
    "backing-filename-format": "qcow2",
    "virtual-size": 21474836480,
    "filename": "root.qcow2",
    "cluster-size": 65536,
    "format": "qcow2",
    "actual-size": 397312,
    "full-backing-filename": "/images/x/base.qcow2",
    "backing-filename": "/images/x/base.qcow2",
    "dirty-flag": false
}
EOF`)
	got, err := Inspect(context.Background(), "root.qcow2")
	if err != nil {
		t.Fatal(err)
	}
	want := Info{
		VirtualSize: 20 * bytesize.GiB,
		Format:      "qcow2",
		BackingFile: "/images/x/base.qcow2",
		Snapshots:   []string{"pre-upgrade"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Inspect() = %+v, want %+v", got, want)
	}
}

func TestInspectBadOutput(t *testing.T) {
	fakeQemuImg(t, "echo 'image: x'")
	if _, err := Inspect(context.Background(), "x"); err == nil || !strings.Contains(err.Error(), "parsing qemu-img info") {
		t.Fatalf("Inspect() error = %v", err)
	}
}

func TestErrors(t *testing.T) {
	fakeQemuImg(t, `echo "qemu-img: Could not open 'd.qcow2': No such file or directory" >&2; exit 1`)
	err := DeleteSnapshot(context.Background(), "d.qcow2", "s")
	want := "qemu-img snapshot: Could not open 'd.qcow2': No such file or directory (exit status 1)"
	if err == nil || err.Error() != want {
		t.Fatalf("error = %v, want %q", err, want)
	}

	fakeQemuImg(t, "exit 3")
	if err := DeleteSnapshot(context.Background(), "d.qcow2", "s"); err == nil || err.Error() != "qemu-img snapshot: exit status 3" {
		t.Fatalf("error without message = %v", err)
	}

	binary = filepath.Join(t.TempDir(), "missing", "qemu-img")
	if err := DeleteSnapshot(context.Background(), "d.qcow2", "s"); err == nil || !strings.Contains(err.Error(), "qemu-img snapshot") {
		t.Fatalf("error for a missing binary = %v", err)
	}
	binary = "brig-test-no-such-qemu-img"
	if err := DeleteSnapshot(context.Background(), "d.qcow2", "s"); err == nil || !strings.Contains(err.Error(), "not installed") {
		t.Fatalf("error for qemu-img missing from PATH = %v", err)
	}
}

func TestSnapshotNameInUse(t *testing.T) {
	fakeQemuImg(t, infoScript)
	if err := Snapshot(context.Background(), "d.qcow2", "taken"); !errors.Is(err, fs.ErrExist) {
		t.Fatalf("error = %v, want fs.ErrExist", err)
	}
}

func TestCancel(t *testing.T) {
	fakeQemuImg(t, "exec sleep 30")
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := Inspect(ctx, "d.qcow2")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error = %v, want context.DeadlineExceeded", err)
	}
	if d := time.Since(start); d > 10*time.Second {
		t.Fatalf("qemu-img ran for %v after the context ended", d)
	}
}

func TestConvertProgress(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "base.raw")
	if err := os.WriteFile(src, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	fakeQemuImg(t, "exit 0")
	ctx, w := progresstest.Watch(t.Context(), t)
	if err := Convert(ctx, src, filepath.Join(dir, "base.qcow2")); err != nil {
		t.Fatal(err)
	}
	if got, want := w.Finish(), "call qemu-img convert: ok\n"; got != want {
		t.Errorf("progress\n%s\nwant\n%s", got, want)
	}

	fakeQemuImg(t, "echo 'qemu-img: no space left' >&2; exit 1")
	ctx, w = progresstest.Watch(t.Context(), t)
	if err := Convert(ctx, src, filepath.Join(dir, "other.qcow2")); err == nil {
		t.Fatal("Convert succeeded")
	}
	if got, want := w.Finish(), "call qemu-img convert: failed (target): qemu-img convert: no space left (exit status 1)\n"; got != want {
		t.Errorf("progress\n%s\nwant\n%s", got, want)
	}
}
