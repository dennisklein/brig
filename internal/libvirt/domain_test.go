// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

package libvirt

import (
	"strings"
	"testing"

	"libvirt.org/go/libvirtxml"

	"github.com/dennisklein/brig/internal/bytesize"
	"github.com/dennisklein/brig/internal/sdcred"
	"github.com/dennisklein/brig/internal/vm"
)

func testSpec() DomainSpec {
	return DomainSpec{
		Name:      "brig-dev",
		UUID:      "0b5e2c1a-7d3f-4e8a-9c21-5f6a7b8c9d0e",
		CPUs:      4,
		Memory:    8 * bytesize.GiB,
		RootDisk:  "/vms/dev/root.qcow2",
		BaseImage: "/images/f44/base.qcow2",
		DataDisk:  "/vms/dev/data.qcow2",
		NetSocket: "/run/user/1000/brig/dev/net.sock",
		Mounts: []vm.Mount{
			{Source: "/home/u/src", Target: "/mnt/src", ReadOnly: true, Sandbox: true, Tag: "brig0"},
			{Source: "/home/u/out", Target: "/mnt/out", Tag: "brig1"},
		},
		MountIDMap: &libvirtxml.DomainFilesystemIDMap{
			UID: []libvirtxml.DomainFilesystemIDMapEntry{{Start: 1000, Target: 4242, Count: 1}},
			GID: []libvirtxml.DomainFilesystemIDMapEntry{{Start: 1000, Target: 4343, Count: 1}},
		},
		ConsoleLog: "/vms/dev/console.log",
		Credentials: []sdcred.Credential{
			{Name: "fstab.extra", Data: []byte("x\n")},
			{Name: "tmpfiles.extra", Data: []byte("y\n")},
		},
		SecretCredentialFiles: map[string]string{
			"tmpfiles.brig-hostkey": "/vms/dev/ssh_host_ed25519_key",
			"a.second":              "/vms/dev/second",
		},
	}
}

func parse(t *testing.T, s DomainSpec) (string, libvirtxml.Domain) {
	t.Helper()
	x, err := DomainXML(s)
	if err != nil {
		t.Fatal(err)
	}
	var d libvirtxml.Domain
	if err := d.Unmarshal(x); err != nil {
		t.Fatalf("%v\n%s", err, x)
	}
	return x, d
}

func TestDomainXML(t *testing.T) {
	x, d := parse(t, testSpec())

	if d.Type != "kvm" || d.Name != "brig-dev" || d.UUID != "0b5e2c1a-7d3f-4e8a-9c21-5f6a7b8c9d0e" {
		t.Errorf("type/name/uuid = %q %q %q", d.Type, d.Name, d.UUID)
	}
	if d.Memory.Value != 8192 || d.Memory.Unit != "MiB" || d.VCPU.Value != 4 {
		t.Errorf("memory %d %s, vcpus %d", d.Memory.Value, d.Memory.Unit, d.VCPU.Value)
	}
	if d.OS.Firmware != "efi" || d.OS.Type.Machine != "q35" || d.OS.Type.Arch != "x86_64" || d.OS.SMBios.Mode != "sysinfo" {
		t.Errorf("os = %+v", d.OS)
	}
	if f := d.OS.FirmwareInfo.Features; len(f) != 1 || f[0] != (libvirtxml.DomainOSFirmwareFeature{Enabled: "no", Name: "secure-boot"}) {
		t.Errorf("firmware features = %+v", f)
	}
	if d.Features.ACPI == nil || d.Features.APIC == nil || d.CPU.Mode != "host-passthrough" {
		t.Errorf("features %+v, cpu %+v", d.Features, d.CPU)
	}
	if f := d.CPU.Features; len(f) != 2 || f[0] != (libvirtxml.DomainCPUFeature{Policy: "disable", Name: "vmx"}) ||
		f[1] != (libvirtxml.DomainCPUFeature{Policy: "disable", Name: "svm"}) {
		t.Errorf("cpu features = %+v, want vmx and svm disabled", f)
	}
	if f := d.Features; f.PS2 == nil || f.PS2.State != "off" || f.VMPort == nil || f.VMPort.State != "off" {
		t.Errorf("ps2 %+v, vmport %+v", f.PS2, f.VMPort)
	}
	if c := d.Devices.Controllers; len(c) != 1 || c[0].Type != "usb" || c[0].Model != "none" {
		t.Errorf("controllers %+v", c)
	}
	if d.PM == nil || d.PM.SuspendToMem == nil || d.PM.SuspendToMem.Enabled != "no" || d.PM.SuspendToDisk == nil || d.PM.SuspendToDisk.Enabled != "no" {
		t.Errorf("pm = %+v", d.PM)
	}
	if d.OnPoweroff != "destroy" || d.OnReboot != "restart" || d.OnCrash != "destroy" {
		t.Errorf("lifecycle = %s %s %s", d.OnPoweroff, d.OnReboot, d.OnCrash)
	}
	if d.Metadata == nil || !strings.Contains(d.Metadata.XML, `xmlns:brig="https://github.com/dennisklein/brig"`) {
		t.Errorf("metadata = %+v", d.Metadata)
	}
	if d.MemoryBacking == nil || d.MemoryBacking.MemorySource.Type != "memfd" || d.MemoryBacking.MemoryAccess.Mode != "shared" {
		t.Errorf("memory backing = %+v", d.MemoryBacking)
	}

	disks := d.Devices.Disks
	if len(disks) != 2 {
		t.Fatalf("got %d disks", len(disks))
	}
	root, data := disks[0], disks[1]
	if root.Target.Dev != "vda" || root.Target.Bus != "virtio" || root.Source.File.File != "/vms/dev/root.qcow2" ||
		root.Driver.Type != "qcow2" || root.Driver.Discard != "unmap" || root.Boot.Order != 1 {
		t.Errorf("root disk = %+v", root)
	}
	if b := root.BackingStore; b == nil || b.Format.Type != "qcow2" || b.Source.File.File != "/images/f44/base.qcow2" ||
		!selfContained(b.BackingStore) {
		t.Errorf("root backing store = %+v", b)
	}
	if data.Target.Dev != "vdb" || data.Serial != "brig-data" || data.Source.File.File != "/vms/dev/data.qcow2" ||
		data.Driver.Discard != "unmap" || !selfContained(data.BackingStore) {
		t.Errorf("data disk = %+v", data)
	}

	if n := len(d.Devices.Interfaces); n != 1 {
		t.Fatalf("got %d interfaces", n)
	}
	iface := d.Devices.Interfaces[0]
	if iface.Source.VHostUser == nil || iface.Source.VHostUser.Chardev == nil ||
		iface.Model.Type != "virtio" || iface.MAC.Address != "52:54:00:8c:9d:0e" ||
		iface.Backend != nil || len(iface.PortForward) != 0 {
		t.Errorf("interface = %+v", iface)
	} else if u := iface.Source.VHostUser.Chardev.UNIX; u == nil || u.Path != "/run/user/1000/brig/dev/net.sock" ||
		u.Mode != "client" || u.Reconnect != nil {
		t.Errorf("interface source = %+v", u)
	}

	if s := d.Devices.Serials; len(s) != 1 || s[0].Source.Pty == nil || s[0].Log.File != "/vms/dev/console.log" {
		t.Errorf("serials = %+v", s)
	}
	if c := d.Devices.Channels; len(c) != 1 || c[0].Source.UNIX == nil || c[0].Target.VirtIO.Name != "org.qemu.guest_agent.0" {
		t.Errorf("channels = %+v", c)
	}
	if r := d.Devices.RNGs; len(r) != 1 || r[0].Model != "virtio" || r[0].Backend.Random.Device != "/dev/urandom" {
		t.Errorf("rngs = %+v", r)
	}
	if d.Devices.MemBalloon.Model != "virtio" {
		t.Errorf("memballoon = %+v", d.Devices.MemBalloon)
	}

	fs := d.Devices.Filesystems
	if len(fs) != 2 {
		t.Fatalf("got %d filesystems", len(fs))
	}
	for i, m := range testSpec().Mounts {
		f := fs[i]
		if f.AccessMode != "passthrough" || f.Driver.Type != "virtiofs" || f.Source.Mount.Dir != m.Source ||
			f.Target.Dir != m.Tag || (f.ReadOnly != nil) != m.ReadOnly ||
			f.IDMap == nil || f.IDMap.UID[0].Target != 4242 || f.IDMap.GID[0].Target != 4343 {
			t.Errorf("filesystem %d = %+v", i, f)
		}
	}

	var oem []string
	var fwcfg []libvirtxml.DomainSysInfoEntry
	for _, si := range d.SysInfo {
		if si.SMBIOS != nil {
			oem = append(oem, si.SMBIOS.OEMStrings.Entry...)
		}
		if si.FWCfg != nil {
			fwcfg = append(fwcfg, si.FWCfg.Entry...)
		}
	}
	if len(oem) != 2 || oem[0] != "io.systemd.credential.binary:fstab.extra=eAo=" || oem[1] != "io.systemd.credential.binary:tmpfiles.extra=eQo=" {
		t.Errorf("OEM strings = %q", oem)
	}
	wantFw := []libvirtxml.DomainSysInfoEntry{
		{Name: "opt/io.systemd.credentials/a.second", File: "/vms/dev/second"},
		{Name: "opt/io.systemd.credentials/tmpfiles.brig-hostkey", File: "/vms/dev/ssh_host_ed25519_key"},
	}
	if len(fwcfg) != 2 || fwcfg[0] != wantFw[0] || fwcfg[1] != wantFw[1] {
		t.Errorf("fw_cfg entries = %+v, want %+v", fwcfg, wantFw)
	}

	for _, sub := range []string{
		`<os firmware="efi">`,
		`<type arch="x86_64" machine="q35">hvm</type>`,
		`<feature enabled="no" name="secure-boot">`,
		`<smbios mode="sysinfo">`,
		`<disk type="file" device="disk">`,
		`<backingStore type="file">`,
		`<interface type="vhostuser">`,
		`<source type="unix" mode="client" path="/run/user/1000/brig/dev/net.sock">`,
		`<model type="virtio">`,
		`<filesystem type="mount" accessmode="passthrough">`,
		`<sysinfo type="smbios">`,
		`<sysinfo type="fwcfg">`,
		`<serial type="pty">`,
		`<channel type="unix">`,
		`<memballoon model="virtio">`,
	} {
		if !strings.Contains(x, sub) {
			t.Errorf("domain XML lacks %s:\n%s", sub, x)
		}
	}

	if n := strings.Count(x, "<backingStore></backingStore>"); n != 2 {
		t.Errorf("domain XML has %d empty backing stores, want 2:\n%s", n, x)
	}
	for _, sub := range []string{"<portForward", `<backend type="passt"`, `<interface type="user"`} {
		if strings.Contains(x, sub) {
			t.Errorf("domain XML has %s; brig runs passt itself:\n%s", sub, x)
		}
	}
	if again, _ := DomainXML(testSpec()); again != x {
		t.Error("DomainXML is not deterministic")
	}
}

// selfContained reports whether b is an empty <backingStore/>, which
// libvirtxml parses as a file source without a path.
func selfContained(b *libvirtxml.DomainDiskBackingStore) bool {
	return b != nil && b.Format == nil && b.BackingStore == nil &&
		(b.Source == nil || b.Source.File == nil || b.Source.File.File == "")
}

func TestDomainXMLMinimal(t *testing.T) {
	s := testSpec()
	s.Mounts, s.Credentials, s.SecretCredentialFiles, s.ConsoleLog = nil, nil, nil, ""
	x, d := parse(t, s)
	if len(d.Devices.Filesystems) != 0 {
		t.Errorf("filesystems without mounts:\n%s", x)
	}
	// The vhost-user NIC needs shared memory even without virtiofs.
	if m := d.MemoryBacking; m == nil || m.MemorySource.Type != "memfd" || m.MemoryAccess.Mode != "shared" {
		t.Errorf("memory backing = %+v, want shared memfd", m)
	}
	// libvirt rejects <smbios mode='sysinfo'/> without SMBIOS sysinfo.
	if d.OS.SMBios != nil || len(d.SysInfo) != 0 {
		t.Errorf("sysinfo without credentials:\n%s", x)
	}
	if d.Devices.Serials[0].Log != nil {
		t.Errorf("console log without path:\n%s", x)
	}
}

func TestDomainXMLRejects(t *testing.T) {
	for _, tc := range []struct {
		name   string
		modify func(*DomainSpec)
		want   string
	}{
		{"empty name", func(s *DomainSpec) { s.Name = "" }, "invalid name"},
		{"bad uuid", func(s *DomainSpec) { s.UUID = "nope" }, "invalid UUID"},
		{"no cpus", func(s *DomainSpec) { s.CPUs = 0 }, "at least one CPU"},
		{"odd memory", func(s *DomainSpec) { s.Memory = bytesize.MiB + 1 }, "whole number of MiB"},
		{"no memory", func(s *DomainSpec) { s.Memory = 0 }, "whole number of MiB"},
		{"relative disk", func(s *DomainSpec) { s.DataDisk = "data.qcow2" }, "data disk path"},
		{"relative base", func(s *DomainSpec) { s.BaseImage = "" }, "base image path"},
		{"relative log", func(s *DomainSpec) { s.ConsoleLog = "console.log" }, "console log"},
		{"no socket", func(s *DomainSpec) { s.NetSocket = "" }, "network socket path"},
		{"relative socket", func(s *DomainSpec) { s.NetSocket = "net.sock" }, "network socket path"},
		{"long socket", func(s *DomainSpec) { s.NetSocket = "/" + strings.Repeat("s", 107) }, "at most 107 bytes"},
		{"relative mount", func(s *DomainSpec) { s.Mounts[0].Source = "src" }, "not absolute"},
		{"empty tag", func(s *DomainSpec) { s.Mounts[0].Tag = "" }, "virtiofs tag"},
		{"long tag", func(s *DomainSpec) { s.Mounts[0].Tag = strings.Repeat("t", 37) }, "virtiofs tag"},
		{"duplicate tag", func(s *DomainSpec) { s.Mounts[1].Tag = "brig0" }, "duplicate virtiofs tag"},
		{"writable sandbox mount", func(s *DomainSpec) { s.Mounts[1].Sandbox = true }, "sandbox mounts must be read-only"},
		{"mounts without ID map", func(s *DomainSpec) { s.MountIDMap = nil }, "need an ID map"},
		{"bad credential name", func(s *DomainSpec) { s.Credentials[0].Name = "a/b" }, "invalid credential name"},
		{"secret oem credential", func(s *DomainSpec) { s.Credentials[0].Secret = true }, "is secret"},
		{"duplicate credential", func(s *DomainSpec) { s.SecretCredentialFiles["fstab.extra"] = "/x" }, "duplicate credential"},
		{"huge credentials", func(s *DomainSpec) { s.Credentials[0].Data = make([]byte, 100*1024) }, "too large"},
		{"long fw_cfg name", func(s *DomainSpec) { s.SecretCredentialFiles[strings.Repeat("k", 29)] = "/x" }, "exceeds 55"},
		{"relative secret file", func(s *DomainSpec) { s.SecretCredentialFiles["k"] = "key" }, "absolute path"},
		{"comma in secret file", func(s *DomainSpec) { s.SecretCredentialFiles["k"] = "/a,b" }, "without commas"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := testSpec()
			tc.modify(&s)
			_, err := DomainXML(s)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("DomainXML() error = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestDomainXMLLimits(t *testing.T) {
	s := testSpec()
	s.Mounts[0].Tag = strings.Repeat("t", 36)
	s.SecretCredentialFiles = map[string]string{strings.Repeat("k", 28): "/x"} // 55-character fw_cfg name
	s.NetSocket = "/" + strings.Repeat("s", 106)                               // 107 bytes
	if _, err := DomainXML(s); err != nil {
		t.Error(err)
	}
}
