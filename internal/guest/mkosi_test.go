// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

package guest

import (
	"io/fs"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
)

func testImageConfig() ImageConfig {
	return ImageConfig{
		FedoraRelease: 44,
		RepoFile:      []byte("[brig]\ngpgkey=file://" + RepoKeyPath + "\n"),
		RepoKey:       []byte("-----BEGIN PGP PUBLIC KEY BLOCK-----\n"),
	}
}

func readFile(t *testing.T, dir, name string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestWriteMkosiConfig(t *testing.T) {
	// mkosi copies file and directory modes into the image, so they must
	// not depend on the umask.
	defer syscall.Umask(syscall.Umask(0o077))

	dir := t.TempDir()
	c := testImageConfig()
	c.OpenShellVersion = "0.1.2"
	if err := WriteMkosiConfig(dir, c); err != nil {
		t.Fatal(err)
	}

	conf := readFile(t, dir, "mkosi.conf")
	for _, want := range []string{
		"\nMinimumVersion=26\n", "\nDistribution=fedora\n", "\nRelease=44\n", "\nArchitecture=x86-64\n",
		"\nFormat=disk\n", "\nOutput=base\n", "\nManifestFormat=json\n", "\nBootable=yes\n",
		"\nBootloader=systemd-boot\n", "\nKernelCommandLine=console=ttyS0 rw systemd.firstboot=no fsck.repair=yes\n",
		"\nSELinuxRelabel=yes\n",
	} {
		if !strings.Contains(conf, want) {
			t.Errorf("mkosi.conf lacks %q:\n%s", want, conf)
		}
	}
	pkgs := mkosiPackages(conf)
	for _, want := range []string{
		"kernel", "systemd", "systemd-udev", "systemd-pam", "kbd", "systemd-boot-unsigned",
		"systemd-networkd", "systemd-resolved", "dbus-broker", "bash", "openssh-server",
		"shadow-utils", "e2fsprogs", "iproute", "sqlite", "qemu-guest-agent", "dnf5",
		"selinux-policy-targeted", "policycoreutils", "container-selinux", "podman", "crun",
		"conmon", "netavark", "aardvark-dns", "passt", "catatonit", "openssl",
		"openshell-0.1.2", "openshell-gateway-0.1.2",
	} {
		if !slices.Contains(pkgs, want) {
			t.Errorf("mkosi.conf does not install %s: %q", want, pkgs)
		}
	}
	for _, p := range pkgs {
		if strings.ContainsAny(p, " \t#") {
			t.Errorf("malformed package %q", p)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "mkosi.conf.tmpl")); err == nil {
		t.Error("template mkosi.conf.tmpl was copied")
	}

	for _, tree := range []string{"mkosi.sandbox", "mkosi.extra"} {
		if got := readFile(t, dir, tree+"/etc/yum.repos.d/brig.repo"); got != string(c.RepoFile) {
			t.Errorf("%s brig.repo = %q", tree, got)
		}
		if got := readFile(t, dir, tree+RepoKeyPath); got != string(c.RepoKey) {
			t.Errorf("%s repository key = %q", tree, got)
		}
	}

	for name, want := range map[string]fs.FileMode{
		"mkosi.postinst.chroot":                                       0o755,
		"mkosi.repart/10-root.conf":                                   0o644,
		"mkosi.extra":                                                 fs.ModeDir | 0o755,
		"mkosi.extra/usr/libexec/brig":                                fs.ModeDir | 0o755,
		"mkosi.extra/usr/libexec/brig/sandbox-volumes":                0o755,
		"mkosi.extra/usr/lib/systemd/user/brig-gateway-proxy.service": 0o644,
		"mkosi.extra/etc/yum.repos.d/brig.repo":                       0o644,
		"mkosi.sandbox/etc/pki/rpm-gpg":                               fs.ModeDir | 0o755,
	} {
		fi, err := os.Stat(filepath.Join(dir, name))
		if err != nil {
			t.Error(err)
		} else if fi.Mode() != want {
			t.Errorf("%s: mode %v, want %v", name, fi.Mode(), want)
		}
	}
}

// mkosiPackages returns the Packages= list of an mkosi.conf, read like
// mkosi's own parser: comments start at any "#" and the lines of a value
// after the first are indented.
func mkosiPackages(conf string) []string {
	var pkgs []string
	in := false
	for _, line := range strings.Split(conf, "\n") {
		line, _, _ = strings.Cut(line, "#")
		switch {
		case strings.TrimSpace(line) == "":
		case strings.HasPrefix(line, "Packages="):
			in = true
			pkgs = append(pkgs, strings.Fields(strings.TrimPrefix(line, "Packages="))...)
		case in && (line[0] == ' ' || line[0] == '\t'):
			pkgs = append(pkgs, strings.Fields(line)...)
		default:
			in = false
		}
	}
	return pkgs
}

func TestWriteMkosiConfigUnpinned(t *testing.T) {
	dir := t.TempDir()
	if err := WriteMkosiConfig(dir, testImageConfig()); err != nil {
		t.Fatal(err)
	}
	pkgs := mkosiPackages(readFile(t, dir, "mkosi.conf"))
	if got := pkgs[max(len(pkgs)-2, 0):]; !slices.Equal(got, []string{"openshell", "openshell-gateway"}) {
		t.Errorf("mkosi.conf does not end with the unpinned OpenShell packages: %q", pkgs)
	}
}

func TestWriteMkosiConfigRejects(t *testing.T) {
	for name, mod := range map[string]func(*ImageConfig){
		"no release":    func(c *ImageConfig) { c.FedoraRelease = 0 },
		"bad version":   func(c *ImageConfig) { c.OpenShellVersion = "0.1.2\nPackages=evil" },
		"blank version": func(c *ImageConfig) { c.OpenShellVersion = " " },
		"no repo file":  func(c *ImageConfig) { c.RepoFile = nil },
		"no repo key":   func(c *ImageConfig) { c.RepoKey = nil },
	} {
		c := testImageConfig()
		mod(&c)
		if err := WriteMkosiConfig(t.TempDir(), c); err == nil {
			t.Errorf("%s: WriteMkosiConfig succeeded", name)
		}
	}

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "stale"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := WriteMkosiConfig(dir, testImageConfig()); err == nil {
		t.Error("WriteMkosiConfig into a non-empty directory succeeded")
	}
	if err := WriteMkosiConfig(filepath.Join(dir, "missing"), testImageConfig()); err == nil {
		t.Error("WriteMkosiConfig into a missing directory succeeded")
	}
}

func TestEmbeddedFiles(t *testing.T) {
	bash, _ := exec.LookPath("bash")
	err := fs.WalkDir(mkosiFiles, "mkosi", func(name string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		data, err := mkosiFiles.ReadFile(name)
		if err != nil {
			return err
		}
		for _, want := range []string{"SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>", "SPDX-License-Identifier: Apache-2.0"} {
			if !strings.Contains(string(data), want) {
				t.Errorf("%s lacks %q", name, want)
			}
		}
		if strings.HasPrefix(string(data), "#!/bin/bash\n") && bash != "" {
			if out, err := exec.Command(bash, "-n", "-c", string(data)).CombinedOutput(); err != nil {
				t.Errorf("%s: bash -n: %v\n%s", name, err, out)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// TestImageMatchesConstants checks that the image's static files agree with
// the constants and paths the Go code relies on.
func TestImageMatchesConstants(t *testing.T) {
	const units = "mkosi.extra/usr/lib/systemd/"
	uid := strconv.Itoa(UID)
	for _, tc := range []struct {
		file  string
		wants []string
	}{
		{"mkosi.postinst.chroot", []string{"--gid " + uid + " " + User + "\n", "--uid " + uid + " ", "--home-dir " + home + " "}},
		{"mkosi.extra/etc/ssh/sshd_config.d/10-brig.conf", []string{"AllowUsers " + User + " root\n"}},
		{units + "user/brig-gateway-proxy.socket", []string{"ListenStream=" + strconv.Itoa(GatewayPort) + "\n"}},
		{units + "user/brig-sandbox-volumes.service", []string{" %E/brig/" + path.Base(sandboxVolumes) + "\n"}},
		{units + "user/openshell-gateway.service.d/50-brig.conf", []string{
			"ConditionUser=" + User + "\n",
			"EnvironmentFile=-%E/brig/" + path.Base(gatewayEnv) + "\n",
		}},
		{"mkosi.extra/usr/share/brig/gateway-mounts.toml", nil},
	} {
		data, err := mkosiFiles.ReadFile("mkosi/" + tc.file)
		if err != nil {
			t.Error(err)
			continue
		}
		for _, want := range tc.wants {
			if !strings.Contains(string(data), want) {
				t.Errorf("%s lacks %q", tc.file, want)
			}
		}
	}
}

// TestPresetsEnableShippedUnits checks that the presets enable only brig
// units that the image ships, in the matching unit directory.
func TestPresetsEnableShippedUnits(t *testing.T) {
	const units = "mkosi/mkosi.extra/usr/lib/systemd/"
	for _, scope := range []string{"system", "user"} {
		data, err := mkosiFiles.ReadFile(units + scope + "-preset/10-brig.preset")
		if err != nil {
			t.Fatal(err)
		}
		for _, line := range strings.Split(string(data), "\n") {
			unit, ok := strings.CutPrefix(line, "enable ")
			if !ok || !strings.HasPrefix(unit, "brig-") {
				continue
			}
			if _, err := fs.Stat(mkosiFiles, units+scope+"/"+unit); err != nil {
				t.Errorf("%s preset enables %s, which the image does not ship", scope, unit)
			}
		}
	}
}

// TestDNFExcludesKernelsAndOpenShell checks that dnf in the VM cannot update
// the kernel or OpenShell in place, but can still install kernel-headers,
// which gcc needs.
func TestDNFExcludesKernelsAndOpenShell(t *testing.T) {
	data, err := mkosiFiles.ReadFile("mkosi/mkosi.extra/etc/dnf/dnf.conf")
	if err != nil {
		t.Fatal(err)
	}
	var excluded []string
	for line := range strings.Lines(string(data)) {
		if v, ok := strings.CutPrefix(line, "excludepkgs="); ok {
			excluded = strings.Split(strings.TrimSpace(v), ",")
		}
	}
	for _, pkg := range []string{"kernel", "kernel-core", "kernel-modules", "openshell", "openshell-gateway"} {
		if !slices.Contains(excluded, pkg) {
			t.Errorf("dnf.conf does not exclude %s: %q", pkg, excluded)
		}
	}
	for _, pkg := range excluded {
		if strings.ContainsAny(pkg, "*?[") || pkg == "kernel-headers" || pkg == "kernel-devel" {
			t.Errorf("dnf.conf excludes %q, which keeps kernel-headers or kernel-devel from installing", pkg)
		}
	}
}
