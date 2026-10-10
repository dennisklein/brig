// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

package guest

import (
	"encoding/base64"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dennisklein/brig/internal/vm"
)

const testKey = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIB4z brig@host"

func testBootConfig() BootConfig {
	return BootConfig{
		Hostname:       "dev",
		AuthorizedKey:  testKey,
		FormatDataDisk: true,
		Mounts: []vm.Mount{
			{Source: "/src/a", Target: "/mnt/a", ReadOnly: true, Tag: "brig0"},
			{Source: "/src/b", Target: "/work/b", Tag: "brig1"},
			{Source: "/src/c", Target: "/home/agent/c", ReadOnly: true, Sandbox: true, Tag: "brig2"},
		},
	}
}

func credentialData(t *testing.T, c BootConfig) map[string]string {
	t.Helper()
	creds, err := Credentials(c)
	if err != nil {
		t.Fatal(err)
	}
	data := map[string]string{}
	for _, cred := range creds {
		if err := cred.Validate(); err != nil {
			t.Error(err)
		}
		if cred.Secret {
			t.Errorf("credential %s is secret", cred.Name)
		}
		data[cred.Name] = string(cred.Data)
	}
	return data
}

// tmpfilesEntry is a parsed tmpfiles.d line with its decoded content.
type tmpfilesEntry struct {
	typ, mode, user, group, content string
}

func parseTmpfiles(t *testing.T, text string) map[string]tmpfilesEntry {
	t.Helper()
	entries := map[string]tmpfilesEntry{}
	for _, line := range strings.Split(strings.TrimSuffix(text, "\n"), "\n") {
		f := strings.Fields(line)
		if len(f) < 6 || len(f) > 7 || f[5] != "-" {
			t.Fatalf("malformed tmpfiles line %q", line)
		}
		e := tmpfilesEntry{typ: f[0], mode: f[2], user: f[3], group: f[4]}
		if len(f) == 7 && f[6] != "-" {
			e.content = f[6]
			if strings.Contains(e.typ, "~") {
				b, err := base64.StdEncoding.DecodeString(f[6])
				if err != nil {
					t.Fatalf("%q: %v", line, err)
				}
				e.content = string(b)
			}
		}
		if _, dup := entries[f[1]]; dup {
			t.Fatalf("duplicate tmpfiles path %s", f[1])
		}
		entries[f[1]] = e
	}
	return entries
}

func TestCredentials(t *testing.T) {
	data := credentialData(t, testBootConfig())
	if len(data) != 3 {
		t.Errorf("credentials = %v", data)
	}
	if got := data["system.hostname"]; got != "dev" {
		t.Errorf("system.hostname = %q", got)
	}

	files := parseTmpfiles(t, data["tmpfiles.extra"])
	for path, want := range map[string]tmpfilesEntry{
		"/etc/hostname":                            {"f+~", "0644", "root", "root", "dev\n"},
		"/etc/ssh/ssh_host_ed25519_key":            {"f+^", "0600", "root", "root", HostKeyCredential},
		"/root/.ssh":                               {"d", "0700", "root", "root", ""},
		"/root/.ssh/authorized_keys":               {"f+~", "0600", "root", "root", testKey + "\n"},
		"/home/agent":                              {"d", "0700", "agent", "agent", ""},
		"/home/agent/.ssh":                         {"d", "0700", "agent", "agent", ""},
		"/home/agent/.ssh/authorized_keys":         {"f+~", "0600", "agent", "agent", testKey + "\n"},
		"/home/agent/.config":                      {"d", "0700", "agent", "agent", ""},
		"/home/agent/.config/brig":                 {"d", "0700", "agent", "agent", ""},
		"/home/agent/.config/brig/gateway.env":     {"f+~", "0600", "agent", "agent", "OPENSHELL_GATEWAY_CONFIG=/usr/share/brig/gateway-mounts.toml\n"},
		"/home/agent/.config/brig/sandbox-volumes": {"f+~", "0600", "agent", "agent", "brig2 /home/agent/c\n"},
	} {
		if got, ok := files[path]; !ok {
			t.Errorf("tmpfiles.extra lacks %s", path)
		} else if got != want {
			t.Errorf("tmpfiles.extra %s = %+v, want %+v", path, got, want)
		}
	}
	if len(files) != 11 {
		t.Errorf("tmpfiles.extra has %d entries:\n%s", len(files), data["tmpfiles.extra"])
	}

	wantFstab := `/dev/disk/by-id/virtio-brig-data /home/agent ext4 defaults,x-systemd.makefs,x-systemd.growfs 0 2
brig0 /mnt/a virtiofs ro,nofail 0 0
brig1 /work/b virtiofs rw,nofail 0 0
brig2 /home/agent/c virtiofs ro,nofail,context=system_u:object_r:container_file_t:s0 0 0
`
	if got := data["fstab.extra"]; got != wantFstab {
		t.Errorf("fstab.extra =\n%s\nwant\n%s", got, wantFstab)
	}
}

func TestCredentialsWithoutSandboxMounts(t *testing.T) {
	c := testBootConfig()
	c.Mounts = nil
	c.FormatDataDisk = false
	c.AuthorizedKey = "  " + testKey + "\t"
	data := credentialData(t, c)
	files := parseTmpfiles(t, data["tmpfiles.extra"])
	// Truncated, so a mounts configuration from an earlier boot is gone.
	for _, path := range []string{"/home/agent/.config/brig/gateway.env", "/home/agent/.config/brig/sandbox-volumes"} {
		if got, want := files[path], (tmpfilesEntry{"f+", "0600", "agent", "agent", ""}); got != want {
			t.Errorf("%s = %+v, want %+v", path, got, want)
		}
	}
	if got := files["/home/agent/.ssh/authorized_keys"].content; got != testKey+"\n" {
		t.Errorf("authorized_keys = %q", got)
	}
	if got, want := data["fstab.extra"], "/dev/disk/by-id/virtio-brig-data /home/agent ext4 defaults,x-systemd.growfs 0 2\n"; got != want {
		t.Errorf("fstab.extra = %q, want %q", got, want)
	}
}

func TestCredentialsRejects(t *testing.T) {
	mount := func(m vm.Mount) func(*BootConfig) {
		return func(c *BootConfig) { c.Mounts = append(c.Mounts, m) }
	}
	for name, mod := range map[string]func(*BootConfig){
		"bad hostname":       func(c *BootConfig) { c.Hostname = "Dev" },
		"no key":             func(c *BootConfig) { c.AuthorizedKey = " " },
		"multi-line key":     func(c *BootConfig) { c.AuthorizedKey = testKey + "\n" + testKey },
		"no tag":             mount(vm.Mount{Target: "/x", ReadOnly: true}),
		"bad tag":            mount(vm.Mount{Target: "/x", ReadOnly: true, Tag: "a b"}),
		"long tag":           mount(vm.Mount{Target: "/x", ReadOnly: true, Tag: strings.Repeat("a", 37)}),
		"duplicate tag":      mount(vm.Mount{Target: "/x", ReadOnly: true, Tag: "brig0"}),
		"duplicate target":   mount(vm.Mount{Target: "/mnt/a", ReadOnly: true, Tag: "brig9"}),
		"relative target":    mount(vm.Mount{Target: "x", ReadOnly: true, Tag: "brig9"}),
		"root target":        mount(vm.Mount{Target: "/", ReadOnly: true, Tag: "brig9"}),
		"unclean target":     mount(vm.Mount{Target: "/a/../b", ReadOnly: true, Tag: "brig9"}),
		"blank in target":    mount(vm.Mount{Target: "/a b", ReadOnly: true, Tag: "brig9"}),
		"home target":        mount(vm.Mount{Target: "/home/agent", ReadOnly: true, Tag: "brig9"}),
		"home parent target": mount(vm.Mount{Target: "/home", ReadOnly: true, Tag: "brig9"}),
		"rw sandbox mount":   mount(vm.Mount{Target: "/x", Sandbox: true, Tag: "brig9"}),
	} {
		c := testBootConfig()
		mod(&c)
		if _, err := Credentials(c); err == nil {
			t.Errorf("%s: Credentials succeeded", name)
		}
	}
}

func TestCredentialsRejectsMountsOverProvisionedPaths(t *testing.T) {
	data := credentialData(t, testBootConfig())
	for path := range parseTmpfiles(t, data["tmpfiles.extra"]) {
		for dir := path; dir != "/"; dir = filepath.Dir(dir) {
			c := testBootConfig()
			c.Mounts = append(c.Mounts, vm.Mount{Source: "/src/x", Target: dir, Tag: "brig9"})
			if _, err := Credentials(c); err == nil {
				t.Errorf("Credentials accepted a mount at %s, which hides %s", dir, path)
			}
		}
	}
	c := testBootConfig()
	c.Mounts = append(c.Mounts,
		vm.Mount{Source: "/src/x", Target: "/home/agent/.config/gh", Tag: "brig8"},
		vm.Mount{Source: "/src/y", Target: "/home/agent/.sshx", Tag: "brig9"})
	if _, err := Credentials(c); err != nil {
		t.Errorf("Credentials: %v", err)
	}
}
