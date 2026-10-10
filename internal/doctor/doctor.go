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

	"github.com/dennisklein/brig/internal/deps"
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
	// Hint says how to fix a warning or failure. It starts with the action
	// and may continue on further lines.
	Hint string
	// pkg is the package of the required group whose binary is missing.
	pkg string
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

// sessionHint fixes a missing systemd user manager, which brig's VM networks
// run in: su and sudo keep the old session, without XDG_RUNTIME_DIR.
const sessionHint = "log in through a session that starts your systemd user manager (a console, graphical or ssh login), not su or sudo; systemctl --user status shows whether it runs"

// Run performs all checks.
func Run(ctx context.Context, h Host) []Result {
	results := []Result{
		checkUser(h),
		checkKVM(h),
		checkUserNamespaces(h),
		checkRuntimeDir(h),
		checkUserManager(ctx, h),
		checkBinary(h, "virtqemud", "libvirt-daemon-driver-qemu", "libvirt's QEMU driver daemon", Fail, "/usr/sbin/virtqemud"),
		checkBinary(h, "qemu-system-x86_64", "qemu-kvm-core", "QEMU", Fail),
		checkBinary(h, "qemu-img", "qemu-img", "disk image tool", Fail),
		checkBinary(h, "passt", "passt", "user-mode networking", Fail),
		checkBinary(h, "pasta", "passt", "connects VM network namespaces to the host", Fail),
		checkBinary(h, "nft", "nftables", "enforces network profiles", Fail, "/usr/sbin/nft"),
		checkBinary(h, "ip", "iproute", "reads the routes that network profiles are enforced against", Fail, "/usr/sbin/ip"),
		checkTun(h),
		checkBinary(h, "ssh", "openssh-clients", "OpenSSH client", Fail),
		checkBinary(h, "virsh", "libvirt-client", "libvirt client, used by brig console", Warn),
		checkBinary(h, "virtiofsd", "virtiofsd", "needed only for --mount", Warn, "/usr/libexec/virtiofsd"),
		checkBinary(h, "podman", "podman", "needed only for brig image push", Warn),
		checkBinary(h, "secret-tool", "libsecret", "needed only for providers in OpenShell config directories", Warn),
		checkBinary(h, "ukify", "systemd-ukify", "unified kernel image builder, used by mkosi", Fail, "/usr/lib/systemd/ukify"),
		checkFirmware(h),
		checkMkosi(ctx, h),
		checkOpenShell(ctx, h),
	}
	collapsePackageHints(results)
	return append(results, checkNetwork(ctx, h, results))
}

// collapsePackageHints replaces the hints that name one missing package of
// the required group by the generic line when several are missing, so that
// one command installs them all.
func collapsePackageHints(results []Result) {
	n := 0
	for _, r := range results {
		if r.pkg != "" {
			n++
		}
	}
	if n < 2 {
		return
	}
	for i := range results {
		if results[i].pkg != "" {
			results[i].Hint = depsHint
		}
	}
}

// installHint is the hint for a missing package; a package of the required
// group is remembered in the result, for collapsePackageHints.
func installHint(r *Result, pkg string) {
	r.Hint = "sudo dnf install " + pkg
	if required, err := deps.Packages(); err == nil && slices.Contains(required, pkg) {
		r.pkg = pkg
	}
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
		r.Status, r.Detail, r.Hint = Fail, "/dev/kvm is missing", "enable virtualization (VT-x/AMD-V) in the firmware settings, then load the module: sudo modprobe kvm_intel (Intel) or sudo modprobe kvm_amd (AMD)\n"+
			"In a VM, the hypervisor must offer nested virtualization."
	} else if !h.Writable("/dev/kvm") {
		r.Status, r.Detail, r.Hint = Fail, "/dev/kvm is not accessible", "add your user to the kvm group and log in again: sudo usermod -aG kvm $USER\n"+
			"Fedora's udev rules make /dev/kvm accessible to all users; ls -l /dev/kvm shows whether a local rule changed that."
	}
	return r
}

func checkUserNamespaces(h Host) Result {
	r := Result{Name: "user namespaces", Status: OK, Detail: "enabled"}
	data, err := h.ReadFile("/proc/sys/user/max_user_namespaces")
	n, perr := strconv.Atoi(strings.TrimSpace(string(data)))
	switch {
	case err != nil || perr != nil:
		r.Status, r.Detail, r.Hint = Warn, "cannot read /proc/sys/user/max_user_namespaces", "check that /proc is mounted; mkosi and VM networks need user namespaces: sysctl user.max_user_namespaces"
	case n == 0:
		r.Status, r.Detail, r.Hint = Fail, "disabled", "enable them, since mkosi builds images and pasta runs VM networks in user namespaces: sudo sysctl user.max_user_namespaces=63359\n"+
			"To keep the setting across reboots, put user.max_user_namespaces=63359 in a file in /etc/sysctl.d/."
	}
	return r
}

func checkTun(h Host) Result {
	r := Result{Name: "/dev/net/tun", Status: OK, Detail: "usable"}
	if !h.Writable("/dev/net/tun") {
		if _, err := h.Stat("/dev/net/tun"); err != nil {
			r.Status, r.Detail, r.Hint = Fail, "/dev/net/tun is missing", "load the tun module: sudo modprobe tun\n"+
				"To load it at every boot: echo tun | sudo tee /etc/modules-load.d/tun.conf"
		} else {
			r.Status, r.Detail, r.Hint = Fail, "not accessible", "pasta needs /dev/net/tun; Fedora's udev rules make it accessible to all users, so ls -l /dev/net/tun shows whether a local rule changed that"
		}
	}
	return r
}

func checkRuntimeDir(h Host) Result {
	r := Result{Name: "XDG_RUNTIME_DIR", Status: OK}
	dir := h.Getenv("XDG_RUNTIME_DIR")
	if dir == "" {
		r.Status, r.Detail, r.Hint = Fail, "not set", sessionHint
		return r
	}
	if _, err := h.Stat(dir); err != nil {
		r.Status, r.Detail, r.Hint = Fail, dir+" does not exist", sessionHint
		return r
	}
	r.Detail = dir
	return r
}

// checkUserManager checks that systemd --user runs, since systemd-run
// starts each VM's network in it.
func checkUserManager(ctx context.Context, h Host) Result {
	r := Result{Name: "systemd --user", Status: OK, Detail: "running"}
	if h.Getenv("XDG_RUNTIME_DIR") == "" {
		r.Status, r.Detail = Warn, "not tried, since XDG_RUNTIME_DIR is not set"
	} else if _, err := h.Output(ctx, "systemctl", "--user", "show-environment"); err != nil {
		r.Status, r.Detail, r.Hint = Fail, "cannot reach the user manager (it runs each VM's network)", sessionHint
	}
	return r
}

// checkBinary looks for a command in PATH and then in fallback locations.
func checkBinary(h Host, name, pkg, purpose string, missing Status, fallbacks ...string) Result {
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
	installHint(&r, pkg)
	return r
}

func checkFirmware(h Host) Result {
	r := Result{Name: "UEFI firmware", Status: OK}
	const dir = "/usr/share/edk2/ovmf"
	if fi, err := h.Stat(dir); err == nil && fi.IsDir() {
		r.Detail = dir
		return r
	}
	r.Status, r.Detail = Fail, dir+" is missing"
	installHint(&r, "edk2-ovmf")
	return r
}

var versionRE = regexp.MustCompile(`(\d+)(?:\.(\d+))?`)

func checkMkosi(ctx context.Context, h Host) Result {
	r := Result{Name: "mkosi", Status: OK}
	if _, err := h.LookPath("mkosi"); err != nil {
		r.Status, r.Detail = Fail, "not found (builds VM images)"
		installHint(&r, "mkosi")
		return r
	}
	out, err := h.Output(ctx, "mkosi", "--version")
	if err != nil {
		r.Status, r.Detail, r.Hint = Fail, "mkosi --version failed: "+err.Error(), "run mkosi --version to see why it fails, or reinstall it: sudo dnf reinstall mkosi"
		return r
	}
	m := versionRE.FindStringSubmatch(string(out))
	if m == nil {
		r.Status, r.Detail, r.Hint = Warn, "cannot parse mkosi version "+strings.TrimSpace(string(out)), fmt.Sprintf("check that mkosi --version reports %d or newer; brig cannot tell whether it builds images", image.MinMkosiVersion)
		return r
	}
	major, _ := strconv.Atoi(m[1])
	r.Detail = "version " + m[0]
	if major < image.MinMkosiVersion {
		r.Status = Fail
		r.Detail += fmt.Sprintf(" is older than %d", image.MinMkosiVersion)
		r.Hint = "sudo dnf upgrade mkosi\n" +
			"If dnf offers nothing newer, this Fedora release is too old for brig: upgrade Fedora or install a newer mkosi from https://github.com/systemd/mkosi."
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
		r.Status, r.Detail, r.Hint = Warn, "openshell --version failed: "+err.Error(), "run openshell --version to see why it fails, or reinstall it: sudo dnf reinstall openshell"
		return r
	}
	r.Detail = strings.TrimSpace(string(out))
	return r
}

// networkNeeds are the checks that a VM network needs to pass.
var networkNeeds = []string{"user namespaces", "XDG_RUNTIME_DIR", "systemd --user", "passt", "pasta", "nft", "ip", "/dev/net/tun"}

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
		r.Status, r.Detail, r.Hint = Warn, "not tried: the host has no IPv4 default gateway", "connect the host to a network with an IPv4 default gateway (ip -4 route shows it), or take down interfaces such as container bridges while offline"
	default:
		cause, hint := classifyNetwork(err)
		r.Status, r.Detail = Fail, "does not start"
		if cause != "" {
			r.Detail += ": " + cause
		}
		r.Hint = hint + "\nThe error was:\n" + err.Error()
	}
	return r
}
