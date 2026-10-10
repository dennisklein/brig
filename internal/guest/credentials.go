// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

package guest

import (
	"encoding/base64"
	"errors"
	"fmt"
	"path"
	"regexp"
	"strings"
	"unicode"

	"github.com/dennisklein/brig/internal/sdcred"
	"github.com/dennisklein/brig/internal/vm"
)

// BootConfig describes one boot of a VM.
type BootConfig struct {
	// Hostname of the VM.
	Hostname string
	// AuthorizedKey is the OpenSSH public key line that may log in as User
	// and as root.
	AuthorizedKey string
	// Mounts are the VM's virtiofs mounts. Sandbox mounts are also offered
	// to OpenShell sandboxes, as Podman volumes named after their tags.
	Mounts []vm.Mount
	// FormatDataDisk formats the data disk if it holds no file system. Set
	// it only until the disk has been formatted: a disk whose file system
	// cannot be recognised later, e.g. after a crash, must fail to mount
	// rather than be formatted again.
	FormatDataDisk bool
}

// Files in User's home that Credentials writes at every boot.
const (
	// gatewayEnv overrides the gateway's environment, see the
	// openshell-gateway.service drop-in.
	gatewayEnv = home + "/.config/brig/gateway.env"
	// sandboxVolumes lists the sandbox mounts for
	// brig-sandbox-volumes.service.
	sandboxVolumes = home + "/.config/brig/sandbox-volumes"
)

// sandboxMountLabel is the SELinux context of sandbox mounts in the VM.
// Containers can read container_file_t but not the default virtiofs_t.
const sandboxMountLabel = "system_u:object_r:container_file_t:s0"

// tagRE matches virtiofs tags (at most 36 bytes) that are also valid Podman
// volume names.
var tagRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,35}$`)

func (c BootConfig) validate() error {
	if err := vm.ValidateName(c.Hostname); err != nil {
		return err
	}
	if strings.TrimSpace(c.AuthorizedKey) == "" || strings.ContainsAny(c.AuthorizedKey, "\n\r\x00") {
		return errors.New("authorized key must be a single non-empty line")
	}
	return CheckMounts(c.Mounts)
}

// CheckMounts checks that the VM can mount mounts as Credentials sets them
// up, e.g. that no target hides a directory that brig manages in the VM.
func CheckMounts(mounts []vm.Mount) error {
	tags, targets := map[string]bool{}, map[string]bool{}
	for _, m := range mounts {
		if !tagRE.MatchString(m.Tag) {
			return fmt.Errorf("mount %s: invalid tag %q", m, m.Tag)
		}
		t := m.Target
		switch {
		case !path.IsAbs(t) || path.Clean(t) != t || t == "/":
			return fmt.Errorf("mount %s: target must be a clean absolute path other than /", m)
		case strings.ContainsFunc(t, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) || r == '\\' }):
			return fmt.Errorf("mount %s: target must not contain whitespace or backslashes", m)
		case t == home || strings.HasPrefix(home, t+"/"):
			return fmt.Errorf("mount %s: target must not hide %s", m, home)
		case hidesProvisioned(t) != "":
			return fmt.Errorf("mount %s: target must not hide %s, which brig writes at every boot", m, hidesProvisioned(t))
		case !m.ReadOnly && tmpfilesManaged[t]:
			return fmt.Errorf("mount %s: a read-write target must not be %s, which the image's tmpfiles rules chmod or clean", m, t)
		case m.Sandbox && !m.ReadOnly:
			return fmt.Errorf("mount %s: sandbox mounts must be read-only", m)
		case tags[m.Tag] || targets[t]:
			return fmt.Errorf("mount %s: duplicate tag or target", m)
		}
		tags[m.Tag], targets[t] = true, true
	}
	return nil
}

// provisioned are the files that tmpfiles writes at every boot (see
// BootConfig.tmpfiles). A mount must not hide them or their directories:
// tmpfiles would write the files, and set the owners and modes of the
// directories, on the host through virtiofs.
var provisioned = []string{
	"/etc/hostname",
	"/etc/ssh/ssh_host_ed25519_key",
	"/root/.ssh/authorized_keys",
	home + "/.ssh/authorized_keys",
	gatewayEnv,
	sandboxVolumes,
}

// hidesProvisioned returns a provisioned file that a mount at target would
// hide, or "".
func hidesProvisioned(target string) string {
	for _, p := range provisioned {
		if p == target || strings.HasPrefix(p, target+"/") {
			return p
		}
	}
	return ""
}

// tmpfilesManaged are the directories that the image's standard tmpfiles.d
// rules chmod at every boot (tmp.conf, home.conf, var.conf), and that
// systemd-tmpfiles-clean deletes old files from in /tmp and /var/tmp. A
// read-write mount there would let the stock image change and delete files on
// the host through virtiofs.
var tmpfilesManaged = map[string]bool{
	"/tmp":       true,
	"/var/tmp":   true,
	"/srv":       true,
	"/var":       true,
	"/var/log":   true,
	"/var/cache": true,
	"/var/lib":   true,
	"/var/spool": true,
}

// Credentials returns the non-secret systemd credentials for one boot. The
// SSH host key travels separately as the secret credential
// HostKeyCredential.
func Credentials(c BootConfig) ([]sdcred.Credential, error) {
	if err := c.validate(); err != nil {
		return nil, err
	}
	return []sdcred.Credential{
		// /etc/hostname is only written by tmpfiles.extra, after PID 1
		// has set the hostname. This sets it early on the first boot of a
		// new root disk, too.
		{Name: "system.hostname", Data: []byte(c.Hostname)},
		{Name: "tmpfiles.extra", Data: []byte(c.tmpfiles())},
		{Name: "fstab.extra", Data: []byte(c.fstab())},
	}, nil
}

// tmpfiles returns tmpfiles.d(5) lines that provision the VM on every boot.
// f+ lines rewrite files, so changes since the last boot take effect. File
// contents are base64-encoded (~), which also skips specifier expansion.
// Explicit d lines keep tmpfiles from creating missing parent directories
// owned by root. systemd makes the missing parents of a mount point as root,
// so d lines also cover the parents of mounts below home, or User could not
// create e.g. ~/.local/state for the OpenShell gateway. Paths go through
// tmpfilesPath, as tmpfiles expands specifiers and quotes in them, which
// fstab does not. On an existing directory, d fixes mode and owner and
// restores the SELinux label, which a freshly formatted data disk lacks;
// it does not recurse, so Podman storage with subuid-owned files below home
// stays untouched.
func (c BootConfig) tmpfiles() string {
	var b strings.Builder
	dir := func(path, owner string) {
		fmt.Fprintf(&b, "d %s 0700 %s %s -\n", tmpfilesPath(path), owner, owner)
	}
	file := func(path, mode, owner, content string) {
		if content == "" {
			fmt.Fprintf(&b, "f+ %s %s %s %s - -\n", path, mode, owner, owner)
			return
		}
		fmt.Fprintf(&b, "f+~ %s %s %s %s - %s\n", path, mode, owner, owner,
			base64.StdEncoding.EncodeToString([]byte(content)))
	}
	key := strings.TrimSpace(c.AuthorizedKey) + "\n"

	file("/etc/hostname", "0644", "root", c.Hostname+"\n")
	// Skipped if the credential is missing; sshd-keygen then makes a key.
	fmt.Fprintf(&b, "f+^ /etc/ssh/ssh_host_ed25519_key 0600 root root - %s\n", HostKeyCredential)
	dir("/root/.ssh", "root")
	file("/root/.ssh/authorized_keys", "0600", "root", key)

	dir(home, User)
	dir(home+"/.ssh", User)
	file(home+"/.ssh/authorized_keys", "0600", User, key)
	dir(home+"/.config", User)
	dir(home+"/.config/brig", User)
	for _, d := range c.mountParents() {
		dir(d, User)
	}
	var env, volumes string
	for _, m := range c.Mounts {
		if m.Sandbox {
			env = "OPENSHELL_GATEWAY_CONFIG=/usr/share/brig/gateway-mounts.toml\n"
			volumes += m.Tag + " " + m.Target + "\n"
		}
	}
	file(gatewayEnv, "0600", User, env)
	file(sandboxVolumes, "0600", User, volumes)
	return b.String()
}

// tmpfilesPath returns p as the path field of a tmpfiles.d(5) line. tmpfiles
// expands % specifiers, which %% escapes, and treats quotes as syntax, so a
// path with a quote goes in double quotes with \" for each double quote.
func tmpfilesPath(p string) string {
	p = strings.ReplaceAll(p, "%", "%%")
	if !strings.ContainsAny(p, `"'`) {
		return p
	}
	return `"` + strings.ReplaceAll(p, `"`, `\"`) + `"`
}

// mountParents returns the directories below home that lie between home and
// a mount target, shallowest first, and that tmpfiles does not already
// provision. It skips directories inside a mount: d would change the owner
// and mode of the host directory through virtiofs.
func (c BootConfig) mountParents() []string {
	seen := map[string]bool{home + "/.ssh": true, home + "/.config": true, home + "/.config/brig": true}
	inMount := func(d string) bool {
		for _, m := range c.Mounts {
			if d == m.Target || strings.HasPrefix(d, m.Target+"/") {
				return true
			}
		}
		return false
	}
	var dirs []string
	for _, m := range c.Mounts {
		var chain []string
		for d := path.Dir(m.Target); strings.HasPrefix(d, home+"/"); d = path.Dir(d) {
			chain = append([]string{d}, chain...)
		}
		for _, d := range chain {
			if !seen[d] && !inMount(d) {
				dirs = append(dirs, d)
			}
			seen[d] = true
		}
	}
	return dirs
}

// fstab returns the fstab(5) lines of the data disk and the virtiofs mounts.
// The data disk is formatted with FormatDataDisk and grown after a resize. A
// virtiofs mount that fails does not fail the boot.
func (c BootConfig) fstab() string {
	var b strings.Builder
	opts := "defaults,x-systemd.growfs"
	if c.FormatDataDisk {
		opts = "defaults,x-systemd.makefs,x-systemd.growfs"
	}
	fmt.Fprintf(&b, "/dev/disk/by-id/virtio-%s %s ext4 %s 0 2\n", DataDiskSerial, home, opts)
	for _, m := range c.Mounts {
		opts := "rw,nofail"
		if m.ReadOnly {
			opts = "ro,nofail"
		}
		if m.Sandbox {
			opts += ",context=" + sandboxMountLabel
		}
		fmt.Fprintf(&b, "%s %s virtiofs %s 0 0\n", m.Tag, m.Target, opts)
	}
	return b.String()
}
