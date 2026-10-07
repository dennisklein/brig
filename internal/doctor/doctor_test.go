// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

package doctor

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"testing"
	"time"
)

type fakeInfo struct{ dir bool }

func (f fakeInfo) Name() string       { return "x" }
func (f fakeInfo) Size() int64        { return 0 }
func (f fakeInfo) Mode() fs.FileMode  { return 0 }
func (f fakeInfo) ModTime() time.Time { return time.Time{} }
func (f fakeInfo) IsDir() bool        { return f.dir }
func (f fakeInfo) Sys() any           { return nil }

// healthy returns a host on which every check passes.
func healthy() Host {
	files := map[string]bool{
		"/dev/kvm": false, "/run/user/1000": true, "/usr/share/edk2/ovmf": true,
		"/usr/sbin/virtqemud": false, "/usr/lib/systemd/ukify": false,
		"/usr/libexec/virtiofsd": false, "/usr/sbin/nft": false,
	}
	return Host{
		LookPath: func(file string) (string, error) {
			switch file {
			case "virtqemud", "ukify", "virtiofsd", "nft":
				return "", errors.New("not in PATH")
			}
			return "/usr/bin/" + file, nil
		},
		Stat: func(name string) (os.FileInfo, error) {
			if dir, ok := files[name]; ok {
				return fakeInfo{dir: dir}, nil
			}
			return nil, fs.ErrNotExist
		},
		ReadFile: func(string) ([]byte, error) { return []byte("63359\n"), nil },
		Getenv:   func(string) string { return "/run/user/1000" },
		Getuid:   func() int { return 1000 },
		Output: func(_ context.Context, name string, _ ...string) ([]byte, error) {
			if name == "mkosi" {
				return []byte("mkosi 26.1\n"), nil
			}
			return []byte("openshell 0.1.2\n"), nil
		},
		Writable: func(string) bool { return true },
	}
}

func find(t *testing.T, results []Result, name string) Result {
	t.Helper()
	for _, r := range results {
		if r.Name == name {
			return r
		}
	}
	t.Fatalf("no result named %q", name)
	return Result{}
}

func TestHealthyHost(t *testing.T) {
	results := Run(context.Background(), healthy())
	for _, r := range results {
		if r.Status != OK {
			t.Errorf("%s: %v %s", r.Name, r.Status, r.Detail)
		}
	}
	if Worst(results) != OK {
		t.Fatal("Worst() != OK")
	}
	if got := find(t, results, "virtqemud").Detail; got != "/usr/sbin/virtqemud" {
		t.Errorf("virtqemud detail = %q, want fallback path", got)
	}
}

func TestProblems(t *testing.T) {
	for name, tc := range map[string]struct {
		mutate func(*Host)
		check  string
		want   Status
	}{
		"root":             {func(h *Host) { h.Getuid = func() int { return 0 } }, "user", Fail},
		"kvm not writable": {func(h *Host) { h.Writable = func(string) bool { return false } }, "kvm", Fail},
		"userns disabled": {func(h *Host) {
			h.ReadFile = func(string) ([]byte, error) { return []byte("0"), nil }
		}, "user namespaces", Fail},
		"old mkosi": {func(h *Host) {
			h.Output = func(context.Context, string, ...string) ([]byte, error) { return []byte("mkosi 24"), nil }
		}, "mkosi", Fail},
		"no openshell": {func(h *Host) {
			look := h.LookPath
			h.LookPath = func(f string) (string, error) {
				if f == "openshell" {
					return "", errors.New("missing")
				}
				return look(f)
			}
		}, "openshell", Warn},
		"no podman": {func(h *Host) {
			look := h.LookPath
			h.LookPath = func(f string) (string, error) {
				if f == "podman" {
					return "", errors.New("missing")
				}
				return look(f)
			}
		}, "podman", Warn},
		"no runtime dir": {func(h *Host) { h.Getenv = func(string) string { return "" } }, "XDG_RUNTIME_DIR", Fail},
	} {
		t.Run(name, func(t *testing.T) {
			h := healthy()
			tc.mutate(&h)
			r := find(t, Run(context.Background(), h), tc.check)
			if r.Status != tc.want {
				t.Fatalf("%s: status %v (%s), want %v", tc.check, r.Status, r.Detail, tc.want)
			}
			if r.Hint == "" && tc.check != "XDG_RUNTIME_DIR" {
				t.Errorf("%s: no hint", tc.check)
			}
		})
	}
}
