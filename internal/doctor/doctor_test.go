// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

package doctor

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/dennisklein/brig/internal/deps"
	"github.com/dennisklein/brig/internal/vmnet"
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
		Writable:     func(string) bool { return true },
		ProbeNetwork: func(context.Context) error { return nil },
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
		"no runtime dir":   {func(h *Host) { h.Getenv = func(string) string { return "" } }, "XDG_RUNTIME_DIR", Fail},
		"runtime dir gone": {func(h *Host) { h.Getenv = func(string) string { return "/run/user/1" } }, "XDG_RUNTIME_DIR", Fail},
		"no user manager": {func(h *Host) {
			h.Output = func(_ context.Context, name string, _ ...string) ([]byte, error) {
				if name == "systemctl" {
					return nil, errors.New("exit status 1")
				}
				return []byte("mkosi 26.1"), nil
			}
		}, "systemd --user", Fail},
		"no kvm": {func(h *Host) {
			stat := h.Stat
			h.Stat = func(n string) (os.FileInfo, error) {
				if n == "/dev/kvm" {
					return nil, fs.ErrNotExist
				}
				return stat(n)
			}
		}, "kvm", Fail},
		"no tun": {func(h *Host) {
			h.Writable = func(n string) bool { return n != "/dev/net/tun" }
			stat := h.Stat
			h.Stat = func(n string) (os.FileInfo, error) {
				if n == "/dev/net/tun" {
					return nil, fs.ErrNotExist
				}
				return stat(n)
			}
		}, "/dev/net/tun", Fail},
		"tun not writable": {func(h *Host) { h.Writable = func(n string) bool { return n != "/dev/net/tun" } }, "/dev/net/tun", Fail},
		"unreadable userns": {func(h *Host) {
			h.ReadFile = func(string) ([]byte, error) { return nil, errors.New("no /proc") }
		}, "user namespaces", Warn},
		"no firmware": {func(h *Host) {
			stat := h.Stat
			h.Stat = func(n string) (os.FileInfo, error) {
				if n == "/usr/share/edk2/ovmf" {
					return nil, fs.ErrNotExist
				}
				return stat(n)
			}
		}, "UEFI firmware", Fail},
	} {
		t.Run(name, func(t *testing.T) {
			h := healthy()
			tc.mutate(&h)
			r := find(t, Run(context.Background(), h), tc.check)
			if r.Status != tc.want {
				t.Fatalf("%s: status %v (%s), want %v", tc.check, r.Status, r.Detail, tc.want)
			}
			if r.Hint == "" {
				t.Errorf("%s: no hint", tc.check)
			}
		})
	}
}

func TestNetworkProbe(t *testing.T) {
	h := healthy()
	h.ProbeNetwork = func(context.Context) error {
		return errors.New("brig-net-_probe-1.service exited after it created /run/user/1000/brig/probe-1/net.sock: Couldn't write to /proc/14/uid_map")
	}
	r := find(t, Run(context.Background(), h), "VM network")
	if r.Status != Fail || !strings.HasPrefix(r.Hint, "sudo dnf install brig-selinux") || !strings.Contains(r.Hint, "exited after it created") {
		t.Errorf("failing probe: %+v", r)
	}
	if strings.Contains(r.Detail, "\n") {
		t.Errorf("detail spans lines: %q", r.Detail)
	}

	h.ProbeNetwork = func(context.Context) error { return fmt.Errorf("starting: %w", vmnet.ErrNoIPv4Gateway) }
	if r := find(t, Run(context.Background(), h), "VM network"); r.Status != Warn || !strings.Contains(r.Detail, "no IPv4 default gateway") {
		t.Errorf("offline host: %+v", r)
	}

	// The probe needs pasta, so a host without it is not probed.
	h.ProbeNetwork = func(context.Context) error {
		t.Error("probed a host without pasta")
		return nil
	}
	lookPath := h.LookPath
	h.LookPath = func(file string) (string, error) {
		if file == "pasta" {
			return "", errors.New("not in PATH")
		}
		return lookPath(file)
	}
	if r := find(t, Run(context.Background(), h), "VM network"); r.Status != Warn || !strings.Contains(r.Detail, "pasta did not pass") {
		t.Errorf("host without pasta: %+v", r)
	}
}

func TestClassifyNetwork(t *testing.T) {
	const unit = "brig-net-_probe-1.service"
	for name, tc := range map[string]struct {
		err      string
		cause    string
		hintWith string // the hint starts with it
		also     string // and contains it
	}{
		"selinux after socket": {
			unit + " exited after it created /run/user/1000/brig/probe-1/net.sock: Couldn't write to /proc/14/uid_map: Operation not permitted",
			"SELinux", "sudo dnf install brig-selinux", "ausearch -m AVC -c passt",
		},
		"selinux without a message": {
			unit + " exited after it created /run/user/1000/brig/probe-1/net.sock: ",
			"SELinux", "sudo dnf install brig-selinux", "README",
		},
		"nft kernel support": {
			"starting: " + unit + " exited before it created x: loading the firewall: exit status 1: Error: Could not process rule: Operation not supported",
			"kernel lacks nftables", "load the kernel's nftables modules", "modprobe",
		},
		"nft syntax": {
			unit + " exited before it created x: loading the firewall: exit status 1: /dev/stdin:5:3-10: Error: syntax error, unexpected counter",
			"nft rejects", "sudo dnf upgrade nftables", "nft --version",
		},
		"nft denied": {
			unit + " exited before it created x: loading the firewall: exit status 1: Error: Could not process rule: Operation not permitted",
			"nft is not allowed", "check for SELinux denials", "ausearch",
		},
		"nft other": {
			unit + " exited before it created x: loading the firewall: exit status 1: boom",
			"nft fails", "sudo dnf reinstall nftables", "nft --version",
		},
		"nft missing": {
			unit + ` exited before it created x: exec: "/usr/sbin/nft": stat /usr/sbin/nft: no such file or directory`,
			"program is missing", "sudo dnf install nftables", "",
		},
		"pasta missing": {
			"pasta is not installed (package passt): exec: \"pasta\": executable file not found in $PATH",
			"program is missing", "sudo dnf install passt", "",
		},
		"ip missing": {
			"ip is not installed (package iproute): exec: \"ip\": executable file not found in $PATH",
			"program is missing", "sudo dnf install iproute", "",
		},
		"systemd-run missing": {
			`starting x: exec: "systemd-run": executable file not found in $PATH`,
			"program is missing", "sudo dnf install systemd", "",
		},
		"no user manager": {
			"starting " + unit + ": exit status 1: Failed to connect to bus: No medium found",
			"no systemd user manager", "log in through a session", "systemctl --user status",
		},
		"no runtime dir": {
			"XDG_RUNTIME_DIR must be set to an absolute path",
			"no systemd user manager", "log in through a session", "",
		},
		"old passt": {
			unit + " exited before it created x: passt: unrecognized option '--vhost-user'",
			"passt is too old", "sudo dnf upgrade passt", "--vhost-user",
		},
		"no route": {
			unit + " exited before it created x: No external routable interface for IPv4",
			"finds no network", "connect the host to a network", "ip route",
		},
		"pasta namespace": {
			unit + " exited before it created x: Failed to create user namespace: Operation not permitted",
			"cannot set up its namespace", "check that user namespaces are enabled", "user.max_user_namespaces",
		},
		"timeout": {
			unit + " did not create x: context deadline exceeded: ",
			"timed out", "run brig doctor again", "ausearch",
		},
		"early exit": {
			unit + " exited before it created x: something odd",
			"exited early", "read the log below", "ausearch",
		},
		"unknown": {
			"mkdir /run/user/1000/brig: no space left on device",
			"", "read the error below", "README",
		},
	} {
		t.Run(name, func(t *testing.T) {
			cause, hint := classifyNetwork(errors.New(tc.err))
			if !strings.Contains(cause, tc.cause) {
				t.Errorf("cause = %q, want %q", cause, tc.cause)
			}
			if !strings.HasPrefix(hint, tc.hintWith) || !strings.Contains(hint, tc.also) {
				t.Errorf("hint = %q, want prefix %q and %q", hint, tc.hintWith, tc.also)
			}
		})
	}
}

func TestNetworkProbeKeepsError(t *testing.T) {
	h := healthy()
	h.ProbeNetwork = func(context.Context) error { return errors.New("x exited before it created y: line one\nline two") }
	r := find(t, Run(context.Background(), h), "VM network")
	if !strings.Contains(r.Hint, "The error was:\nx exited before it created y: line one\nline two") {
		t.Errorf("hint hides the error: %q", r.Hint)
	}
}

func TestPackageHints(t *testing.T) {
	missing := func(names ...string) Host {
		h := healthy()
		look := h.LookPath
		h.LookPath = func(f string) (string, error) {
			if slices.Contains(names, f) {
				return "", errors.New("missing")
			}
			return look(f)
		}
		return h
	}
	results := Run(context.Background(), missing("passt"))
	if got := find(t, results, "passt").Hint; got != "sudo dnf install passt" {
		t.Errorf("one missing package: hint %q", got)
	}
	results = Run(context.Background(), missing("passt", "pasta", "qemu-img"))
	for _, n := range []string{"passt", "pasta", "qemu-img"} {
		if got := find(t, results, n).Hint; got != depsHint {
			t.Errorf("%s with several missing: hint %q", n, got)
		}
	}
	// Optional packages are named alone.
	results = Run(context.Background(), missing("passt", "pasta", "podman"))
	if got := find(t, results, "podman").Hint; got != "sudo dnf install podman" {
		t.Errorf("podman: hint %q", got)
	}
}

func TestPackagesExist(t *testing.T) {
	all, err := deps.Packages("all")
	if err != nil {
		t.Fatal(err)
	}
	for _, pkg := range []string{"libvirt-daemon-driver-qemu", "qemu-kvm-core", "qemu-img", "passt", "nftables", "iproute", "openssh-clients", "libvirt-client", "virtiofsd", "podman", "libsecret", "systemd-ukify", "edk2-ovmf", "mkosi"} {
		if !slices.Contains(all, pkg) {
			t.Errorf("%s is not in internal/deps", pkg)
		}
	}
}

func TestHintsStartWithAction(t *testing.T) {
	h := healthy()
	h.Writable = func(string) bool { return false }
	h.Getenv = func(string) string { return "" }
	h.ReadFile = func(string) ([]byte, error) { return []byte("0"), nil }
	for _, r := range Run(context.Background(), h) {
		if r.Status != OK && r.Hint != "" && (r.Hint[0] == '\n' || r.Hint[0] == ' ') {
			t.Errorf("%s: hint %q does not start with the action", r.Name, r.Hint)
		}
	}
}
