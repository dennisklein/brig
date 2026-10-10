// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

// Package doctor checks whether the host can build images and run VMs.
package doctor

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/dennisklein/brig/internal/image"
	"github.com/dennisklein/brig/internal/openshell"
	"github.com/dennisklein/brig/internal/vmnet"
)

// Status is the outcome of a check.
type Status int

// Check outcomes, from best to worst.
const (
	OK Status = iota
	Warn
	Fail
)

func (s Status) String() string {
	switch s {
	case OK:
		return "ok"
	case Warn:
		return "warn"
	default:
		return "FAIL"
	}
}

// Result is the outcome of one check.
type Result struct {
	Name   string
	Status Status
	Detail string
	// Hint says how to fix a warning or failure.
	Hint string
}

// Host is the view of the host the checks use; tests replace it.
type Host struct {
	LookPath func(file string) (string, error)
	Stat     func(name string) (os.FileInfo, error)
	ReadFile func(name string) ([]byte, error)
	Getenv   func(key string) string
	Getuid   func() int
	// Output runs a command and returns its standard output.
	Output func(ctx context.Context, name string, args ...string) ([]byte, error)
	// Writable reports whether the current user may open name read-write.
	Writable func(name string) bool
	// ProbeNetwork starts and stops a VM network without a VM.
	ProbeNetwork func(ctx context.Context) error
}

// System is the real host.
func System() Host {
	return Host{
		LookPath: exec.LookPath,
		Stat:     os.Stat,
		ReadFile: os.ReadFile,
		Getenv:   os.Getenv,
		Getuid:   os.Getuid,
		Output: func(ctx context.Context, name string, args ...string) ([]byte, error) {
			return exec.CommandContext(ctx, name, args...).Output()
		},
		Writable: func(name string) bool {
			f, err := os.OpenFile(name, os.O_RDWR, 0)
			if err != nil {
				return false
			}
			_ = f.Close()
			return true
		},
		ProbeNetwork: func(ctx context.Context) error {
			return vmnet.Probe(ctx, os.Getenv("XDG_RUNTIME_DIR"))
		},
	}
}

const depsHint = "sudo dnf install $(brig print-fedora-deps)"

// Run performs all checks.
func Run(ctx context.Context, h Host) []Result {
	results := []Result{
		checkUser(h),
		checkKVM(h),
		checkUserNamespaces(h),
		checkRuntimeDir(h),
		checkBinary(h, "virtqemud", "libvirt's QEMU driver daemon", Fail, "/usr/sbin/virtqemud"),
		checkBinary(h, "qemu-system-x86_64", "QEMU", Fail),
		checkBinary(h, "qemu-img", "disk image tool", Fail),
		checkBinary(h, "passt", "user-mode networking", Fail),
		checkBinary(h, "pasta", "connects VM network namespaces to the host", Fail),
		checkBinary(h, "nft", "enforces network profiles", Fail, "/usr/sbin/nft"),
		checkTun(h),
		checkBinary(h, "ssh", "OpenSSH client", Fail),
		checkBinary(h, "virsh", "libvirt client, used by brig console", Warn),
		checkBinary(h, "virtiofsd", "needed only for --mount", Warn, "/usr/libexec/virtiofsd"),
		checkBinary(h, "podman", "needed only for brig image push", Warn),
		checkBinary(h, "secret-tool", "needed only for providers in OpenShell config directories", Warn),
		checkBinary(h, "ukify", "unified kernel image builder, used by mkosi", Fail, "/usr/lib/systemd/ukify"),
		checkFirmware(h),
		checkMkosi(ctx, h),
		checkOpenShell(ctx, h),
	}
	return append(results, checkNetwork(ctx, h, results))
}

// Worst returns the worst status among results.
func Worst(results []Result) Status {
	worst := OK
	for _, r := range results {
		worst = max(worst, r.Status)
	}
	return worst
}

func checkUser(h Host) Result {
	r := Result{Name: "user", Status: OK, Detail: "running unprivileged"}
	if h.Getuid() == 0 {
		r.Status, r.Detail, r.Hint = Fail, "running as root", "run brig as your normal user; VMs live in your libvirt session"
	}
	return r
}

func checkKVM(h Host) Result {
	r := Result{Name: "kvm", Status: OK, Detail: "/dev/kvm is usable"}
	if _, err := h.Stat("/dev/kvm"); err != nil {
		r.Status, r.Detail, r.Hint = Fail, "/dev/kvm is missing", "enable virtualization (VT-x/AMD-V) in the firmware settings"
	} else if !h.Writable("/dev/kvm") {
		r.Status, r.Detail, r.Hint = Fail, "/dev/kvm is not accessible", "add your user to the kvm group: sudo usermod -aG kvm $USER"
	}
	return r
}

func checkUserNamespaces(h Host) Result {
	r := Result{Name: "user namespaces", Status: OK, Detail: "enabled"}
	data, err := h.ReadFile("/proc/sys/user/max_user_namespaces")
	n, perr := strconv.Atoi(strings.TrimSpace(string(data)))
	switch {
	case err != nil || perr != nil:
		r.Status, r.Detail = Warn, "cannot read /proc/sys/user/max_user_namespaces"
	case n == 0:
		r.Status, r.Detail, r.Hint = Fail, "disabled", "mkosi builds images in user namespaces: sudo sysctl user.max_user_namespaces=63359"
	}
	return r
}

func checkTun(h Host) Result {
	r := Result{Name: "/dev/net/tun", Status: OK, Detail: "usable"}
	if !h.Writable("/dev/net/tun") {
		r.Status, r.Detail, r.Hint = Fail, "not accessible", "pasta needs /dev/net/tun; Fedora makes it accessible to all users by default"
	}
	return r
}

func checkRuntimeDir(h Host) Result {
	r := Result{Name: "XDG_RUNTIME_DIR", Status: OK}
	dir := h.Getenv("XDG_RUNTIME_DIR")
	if dir == "" {
		r.Status, r.Detail, r.Hint = Fail, "not set", "run brig from a regular login session"
		return r
	}
	if _, err := h.Stat(dir); err != nil {
		r.Status, r.Detail = Fail, dir+" does not exist"
		return r
	}
	r.Detail = dir
	return r
}

// checkBinary looks for a command in PATH and then in fallback locations.
func checkBinary(h Host, name, purpose string, missing Status, fallbacks ...string) Result {
	r := Result{Name: name, Status: OK}
	if p, err := h.LookPath(name); err == nil {
		r.Detail = p
		return r
	}
	for _, p := range fallbacks {
		if _, err := h.Stat(p); err == nil {
			r.Detail = p
			return r
		}
	}
	r.Status, r.Detail = missing, "not found ("+purpose+")"
	switch name {
	case "virtiofsd":
		r.Hint = "sudo dnf install $(brig print-fedora-deps --with mounts)"
	case "podman":
		r.Hint = "sudo dnf install $(brig print-fedora-deps --with push)"
	case "secret-tool":
		r.Hint = "sudo dnf install $(brig print-fedora-deps --with secrets)"
	default:
		r.Hint = depsHint
	}
	return r
}

func checkFirmware(h Host) Result {
	r := Result{Name: "UEFI firmware", Status: OK}
	const dir = "/usr/share/edk2/ovmf"
	if fi, err := h.Stat(dir); err == nil && fi.IsDir() {
		r.Detail = dir
		return r
	}
	r.Status, r.Detail, r.Hint = Fail, dir+" is missing", depsHint
	return r
}

var versionRE = regexp.MustCompile(`(\d+)(?:\.(\d+))?`)

func checkMkosi(ctx context.Context, h Host) Result {
	r := Result{Name: "mkosi", Status: OK}
	if _, err := h.LookPath("mkosi"); err != nil {
		r.Status, r.Detail, r.Hint = Fail, "not found (builds VM images)", depsHint
		return r
	}
	out, err := h.Output(ctx, "mkosi", "--version")
	if err != nil {
		r.Status, r.Detail = Fail, "mkosi --version failed: "+err.Error()
		return r
	}
	m := versionRE.FindStringSubmatch(string(out))
	if m == nil {
		r.Status, r.Detail = Warn, "cannot parse mkosi version "+strings.TrimSpace(string(out))
		return r
	}
	major, _ := strconv.Atoi(m[1])
	r.Detail = "version " + m[0]
	if major < image.MinMkosiVersion {
		r.Status = Fail
		r.Detail += fmt.Sprintf(" is older than %d", image.MinMkosiVersion)
		r.Hint = "update mkosi: sudo dnf upgrade mkosi"
	}
	return r
}

func checkOpenShell(ctx context.Context, h Host) Result {
	r := Result{Name: "openshell", Status: OK}
	if _, err := h.LookPath("openshell"); err != nil {
		r.Status, r.Detail, r.Hint = Warn, "not found (drives the VMs' gateways)", openshell.InstallHint
		return r
	}
	out, err := h.Output(ctx, "openshell", "--version")
	if err != nil {
		r.Status, r.Detail = Warn, "openshell --version failed: "+err.Error()
		return r
	}
	r.Detail = strings.TrimSpace(string(out))
	return r
}

// networkNeeds are the checks that a VM network needs to pass.
var networkNeeds = []string{"user namespaces", "XDG_RUNTIME_DIR", "passt", "pasta", "nft", "/dev/net/tun"}

// checkNetwork starts the network of a VM, without the VM, as brig start
// would, once the checks it depends on have passed.
func checkNetwork(ctx context.Context, h Host, prior []Result) Result {
	r := Result{Name: "VM network", Status: OK, Detail: "pasta, nftables and passt start"}
	for _, p := range prior {
		if slices.Contains(networkNeeds, p.Name) && p.Status != OK {
			r.Status, r.Detail = Warn, "not tried, since "+p.Name+" did not pass"
			return r
		}
	}
	err := h.ProbeNetwork(ctx)
	switch {
	case err == nil:
	case errors.Is(err, vmnet.ErrNoIPv4Gateway):
		r.Status, r.Detail, r.Hint = Warn, "not tried: the host has no IPv4 default gateway", "connect the host to a network with a default gateway, or take down interfaces such as container bridges while offline"
	default:
		r.Status, r.Detail, r.Hint = Fail, "does not start", err.Error()
		// passt binds its socket and then sandboxes itself in a user
		// namespace, which SELinux may deny it.
		if strings.Contains(err.Error(), "uid_map") {
			r.Hint += "\nIf SELinux denies passt setfcap (sudo ausearch -m AVC -c passt), see Troubleshooting in brig's README."
		}
	}
	return r
}
