// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

package libvirt

import (
	"encoding/hex"
	"errors"
	"fmt"
	"maps"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"libvirt.org/go/libvirtxml"

	"github.com/dennisklein/brig/internal/bytesize"
	"github.com/dennisklein/brig/internal/guest"
	"github.com/dennisklein/brig/internal/sdcred"
	"github.com/dennisklein/brig/internal/vm"
)

// MetadataNamespace identifies brig's element in the <metadata> of the
// domains it manages.
const MetadataNamespace = "https://github.com/dennisklein/brig"

const metadataXML = `<brig:vm xmlns:brig="` + MetadataNamespace + `"/>`

// DomainSpec describes the libvirt domain of a brig VM.
type DomainSpec struct {
	// Name is the domain name, e.g. "brig-dev".
	Name string
	// UUID is the domain UUID in canonical form. It must stay the same for
	// the lifetime of the VM: the guest derives its machine-id from it.
	UUID string
	// CPUs is the number of virtual CPUs.
	CPUs int
	// Memory is the guest RAM, a whole number of MiB.
	Memory bytesize.Size
	// RootDisk is the disposable qcow2 overlay on the qcow2 BaseImage;
	// DataDisk is the persistent qcow2 disk. All are absolute paths.
	RootDisk, BaseImage, DataDisk string
	// NetSocket is the absolute path of the vhost-user socket of the
	// passt process that serves the VM's network. QEMU connects to it as
	// the client, so passt must listen there whenever the domain starts
	// or is restored from a managed save.
	NetSocket string
	// Mounts are host directories shared via virtiofs under their Tag.
	// Sandbox mounts must be read-only.
	Mounts []vm.Mount
	// ConsoleLog is the file that receives the serial console output. It
	// is truncated on every start. Empty disables the log.
	ConsoleLog string
	// Credentials reach the guest as SMBIOS type 11 OEM strings. QEMU's
	// command line and libvirt's logs show them, so none may be secret.
	Credentials []sdcred.Credential
	// SecretCredentialFiles maps credential names to host files that reach
	// the guest via fw_cfg, so only the file name shows up on QEMU's
	// command line.
	SecretCredentialFiles map[string]string
}

const (
	// maxFwCfgName is QEMU's limit on fw_cfg file names (FW_CFG_MAX_FILE_PATH
	// minus the terminating NUL).
	maxFwCfgName = 55
	// maxVirtiofsTag is the size of the tag in the virtio-fs device config.
	maxVirtiofsTag = 36
	// maxArgLen bounds the single "-smbios type=11,value=..." argument that
	// libvirt builds from all OEM strings (Linux MAX_ARG_STRLEN).
	maxArgLen = 128 * 1024
	// maxSocketPath is the longest path passt binds a UNIX socket to: the
	// size of sun_path minus the terminating NUL.
	maxSocketPath = 107
)

var uuidRE = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// DomainXML renders the persistent domain definition of a brig VM: a
// headless q35 KVM guest booting UEFI without Secure Boot, a virtio NIC
// served by an external passt over vhost-user, and first-boot
// configuration via systemd credentials.
func DomainXML(s DomainSpec) (string, error) {
	if err := s.validate(); err != nil {
		return "", fmt.Errorf("domain %q: %w", s.Name, err)
	}
	d := libvirtxml.Domain{
		Type:     "kvm",
		Name:     s.Name,
		UUID:     s.UUID,
		Metadata: &libvirtxml.DomainMetadata{XML: metadataXML},
		Memory:   &libvirtxml.DomainMemory{Value: uint(s.Memory.MiB()), Unit: "MiB"},
		// vhost-user backends (passt, virtiofsd) access guest RAM, so it
		// must be shared memory.
		MemoryBacking: &libvirtxml.DomainMemoryBacking{
			MemorySource: &libvirtxml.DomainMemorySource{Type: "memfd"},
			MemoryAccess: &libvirtxml.DomainMemoryAccess{Mode: "shared"},
		},
		VCPU: &libvirtxml.DomainVCPU{Placement: "static", Value: uint(s.CPUs)},
		OS: &libvirtxml.DomainOS{
			Firmware: "efi",
			Type:     &libvirtxml.DomainOSType{Arch: "x86_64", Machine: "q35", Type: "hvm"},
			// The mkosi image boots unsigned UKIs.
			FirmwareInfo: &libvirtxml.DomainOSFirmwareInfo{
				Features: []libvirtxml.DomainOSFirmwareFeature{{Enabled: "no", Name: "secure-boot"}},
			},
		},
		Features: &libvirtxml.DomainFeatureList{
			ACPI: &libvirtxml.DomainFeature{},
			APIC: &libvirtxml.DomainFeatureAPIC{},
			// The guest is headless. Without these, libvirt and QEMU
			// add a PS/2 controller, the VMware backdoor port and
			// vmmouse, which the guest could drive for no gain.
			PS2:    &libvirtxml.DomainFeatureState{State: "off"},
			VMPort: &libvirtxml.DomainFeatureState{State: "off"},
		},
		// Nested virtualization is off: nothing in the guest needs it, and it
		// would expose the host kernel's VMX/SVM emulation to root in the VM.
		CPU: &libvirtxml.DomainCPU{
			Mode: "host-passthrough",
			Features: []libvirtxml.DomainCPUFeature{
				{Policy: "disable", Name: "vmx"},
				{Policy: "disable", Name: "svm"},
			},
		},
		// A guest that suspends to RAM would keep QEMU alive with its
		// network socket unserved.
		PM: &libvirtxml.DomainPM{
			SuspendToMem:  &libvirtxml.DomainPMPolicy{Enabled: "no"},
			SuspendToDisk: &libvirtxml.DomainPMPolicy{Enabled: "no"},
		},
		OnPoweroff: "destroy",
		OnReboot:   "restart",
		OnCrash:    "destroy",
		Devices:    devices(s),
	}
	if len(s.Credentials) > 0 {
		// libvirt refuses <smbios mode='sysinfo'/> without SMBIOS sysinfo.
		d.OS.SMBios = &libvirtxml.DomainSMBios{Mode: "sysinfo"}
		oem := make([]string, len(s.Credentials))
		for i, c := range s.Credentials {
			oem[i] = c.OEMString()
		}
		d.SysInfo = append(d.SysInfo, libvirtxml.DomainSysInfo{
			SMBIOS: &libvirtxml.DomainSysInfoSMBIOS{OEMStrings: &libvirtxml.DomainSysInfoOEMStrings{Entry: oem}},
		})
	}
	if len(s.SecretCredentialFiles) > 0 {
		var entries []libvirtxml.DomainSysInfoEntry
		for _, name := range slices.Sorted(maps.Keys(s.SecretCredentialFiles)) {
			entries = append(entries, libvirtxml.DomainSysInfoEntry{
				Name: sdcred.Credential{Name: name}.FwCfgName(),
				File: s.SecretCredentialFiles[name],
			})
		}
		d.SysInfo = append(d.SysInfo, libvirtxml.DomainSysInfo{
			FWCfg: &libvirtxml.DomainSysInfoFWCfg{Entry: entries},
		})
	}
	return d.Marshal()
}

func devices(s DomainSpec) *libvirtxml.DomainDeviceList {
	root := disk("vda", s.RootDisk)
	root.BackingStore = &libvirtxml.DomainDiskBackingStore{
		Format:       &libvirtxml.DomainDiskFormat{Type: "qcow2"},
		Source:       fileSource(s.BaseImage),
		BackingStore: &libvirtxml.DomainDiskBackingStore{},
	}
	root.Boot = &libvirtxml.DomainDeviceBoot{Order: 1}
	data := disk("vdb", s.DataDisk)
	data.Serial = guest.DataDiskSerial

	serial := libvirtxml.DomainSerial{
		Source: &libvirtxml.DomainChardevSource{Pty: &libvirtxml.DomainChardevSourcePty{}},
	}
	if s.ConsoleLog != "" {
		serial.Log = &libvirtxml.DomainChardevLog{File: s.ConsoleLog}
	}

	devs := &libvirtxml.DomainDeviceList{
		// Without this, libvirt adds a USB controller that nothing uses.
		Controllers: []libvirtxml.DomainController{{Type: "usb", Model: "none"}},
		Disks:       []libvirtxml.DomainDisk{root, data},
		Interfaces:  []libvirtxml.DomainInterface{nic(s)},
		// libvirt adds the matching <console> itself.
		Serials: []libvirtxml.DomainSerial{serial},
		Channels: []libvirtxml.DomainChannel{{
			// Without a path, libvirt picks the socket path itself.
			Source: &libvirtxml.DomainChardevSource{UNIX: &libvirtxml.DomainChardevSourceUNIX{}},
			Target: &libvirtxml.DomainChannelTarget{
				VirtIO: &libvirtxml.DomainChannelTargetVirtIO{Name: "org.qemu.guest_agent.0"},
			},
		}},
		RNGs: []libvirtxml.DomainRNG{{
			Model:   "virtio",
			Backend: &libvirtxml.DomainRNGBackend{Random: &libvirtxml.DomainRNGBackendRandom{Device: "/dev/urandom"}},
		}},
		MemBalloon: &libvirtxml.DomainMemBalloon{Model: "virtio"},
	}
	for _, m := range s.Mounts {
		fs := libvirtxml.DomainFilesystem{
			AccessMode: "passthrough",
			Driver:     &libvirtxml.DomainFilesystemDriver{Type: "virtiofs"},
			Source:     &libvirtxml.DomainFilesystemSource{Mount: &libvirtxml.DomainFilesystemSourceMount{Dir: m.Source}},
			Target:     &libvirtxml.DomainFilesystemTarget{Dir: m.Tag},
		}
		if m.ReadOnly {
			fs.ReadOnly = &libvirtxml.DomainFilesystemReadOnly{}
		}
		devs.Filesystems = append(devs.Filesystems, fs)
	}
	return devs
}

func disk(dev, path string) libvirtxml.DomainDisk {
	return libvirtxml.DomainDisk{
		Device: "disk",
		Driver: &libvirtxml.DomainDiskDriver{Name: "qemu", Type: "qcow2", Discard: "unmap"},
		Source: fileSource(path),
		// An empty backingStore declares the image self-contained, so
		// libvirt does not follow backing files named in the image header.
		BackingStore: &libvirtxml.DomainDiskBackingStore{},
		Target:       &libvirtxml.DomainDiskTarget{Dev: dev, Bus: "virtio"},
	}
}

func fileSource(path string) *libvirtxml.DomainDiskSource {
	return &libvirtxml.DomainDiskSource{File: &libvirtxml.DomainDiskSourceFile{File: path}}
}

// nic returns the VM's network interface: a virtio NIC whose backend is the
// passt process listening on s.NetSocket. brig runs that passt itself, so
// libvirt manages no network backend and forwards no ports.
func nic(s DomainSpec) libvirtxml.DomainInterface {
	return libvirtxml.DomainInterface{
		// A MAC derived from the UUID keeps the guest's NIC stable when the
		// domain is redefined; libvirt would pick a random one each time.
		MAC: &libvirtxml.DomainInterfaceMAC{Address: macFromUUID(s.UUID)},
		Source: &libvirtxml.DomainInterfaceSource{VHostUser: &libvirtxml.DomainInterfaceSourceVHostUser{
			Chardev: &libvirtxml.DomainChardevSource{
				UNIX: &libvirtxml.DomainChardevSourceUNIX{Path: s.NetSocket, Mode: "client"},
			},
		}},
		Model: &libvirtxml.DomainInterfaceModel{Type: "virtio"},
	}
}

// macFromUUID returns a locally administered MAC address with QEMU's
// 52:54:00 prefix and the last three bytes of the validated uuid.
func macFromUUID(uuid string) string {
	b, _ := hex.DecodeString(strings.ReplaceAll(uuid, "-", ""))
	return fmt.Sprintf("52:54:00:%02x:%02x:%02x", b[13], b[14], b[15])
}

func (s DomainSpec) validate() error {
	var errs []error
	check := func(ok bool, format string, args ...any) {
		if !ok {
			errs = append(errs, fmt.Errorf(format, args...))
		}
	}
	check(s.Name != "" && !strings.ContainsAny(s.Name, "/ \t\n"), "invalid name %q", s.Name)
	check(uuidRE.MatchString(s.UUID), "invalid UUID %q", s.UUID)
	check(s.CPUs > 0, "need at least one CPU, got %d", s.CPUs)
	check(s.Memory > 0 && s.Memory%bytesize.MiB == 0, "memory %s is not a positive whole number of MiB", s.Memory)
	for _, p := range []struct{ what, path string }{
		{"root disk", s.RootDisk}, {"base image", s.BaseImage}, {"data disk", s.DataDisk},
	} {
		check(filepath.IsAbs(p.path), "%s path %q is not absolute", p.what, p.path)
	}
	check(s.ConsoleLog == "" || filepath.IsAbs(s.ConsoleLog), "console log path %q is not absolute", s.ConsoleLog)
	check(filepath.IsAbs(s.NetSocket) && len(s.NetSocket) <= maxSocketPath,
		"network socket path %q must be absolute and at most %d bytes long", s.NetSocket, maxSocketPath)
	errs = append(errs, s.validateMounts()...)
	errs = append(errs, s.validateCredentials()...)
	return errors.Join(errs...)
}

func (s DomainSpec) validateMounts() []error {
	var errs []error
	tags := map[string]bool{}
	for _, m := range s.Mounts {
		if !filepath.IsAbs(m.Source) {
			errs = append(errs, fmt.Errorf("mount source %q is not absolute", m.Source))
		}
		if m.Tag == "" || len(m.Tag) > maxVirtiofsTag {
			errs = append(errs, fmt.Errorf("mount %s: virtiofs tag %q must have 1 to %d bytes", m.Source, m.Tag, maxVirtiofsTag))
		}
		if tags[m.Tag] {
			errs = append(errs, fmt.Errorf("mount %s: duplicate virtiofs tag %q", m.Source, m.Tag))
		}
		tags[m.Tag] = true
		// OpenShell sandboxes may bind-mount these; virtiofsd must refuse
		// writes whatever the guest's mount options say.
		if m.Sandbox && !m.ReadOnly {
			errs = append(errs, fmt.Errorf("mount %s: sandbox mounts must be read-only", m.Source))
		}
	}
	return errs
}

func (s DomainSpec) validateCredentials() []error {
	var errs []error
	names := map[string]bool{}
	unique := func(name string) {
		if names[name] {
			errs = append(errs, fmt.Errorf("duplicate credential %q", name))
		}
		names[name] = true
	}
	argLen := len("type=11")
	for _, c := range s.Credentials {
		unique(c.Name)
		if err := c.Validate(); err != nil {
			errs = append(errs, err)
		}
		if c.Secret {
			errs = append(errs, fmt.Errorf("credential %q is secret: pass it as a file via fw_cfg", c.Name))
		}
		argLen += len(",value=") + len(c.OEMString())
	}
	if argLen >= maxArgLen {
		errs = append(errs, fmt.Errorf("credentials too large for SMBIOS OEM strings: %d bytes, limit %d", argLen, maxArgLen-1))
	}
	for _, name := range slices.Sorted(maps.Keys(s.SecretCredentialFiles)) {
		unique(name)
		c := sdcred.Credential{Name: name}
		if err := c.Validate(); err != nil {
			errs = append(errs, err)
		}
		if len(c.FwCfgName()) > maxFwCfgName {
			errs = append(errs, fmt.Errorf("credential %q: fw_cfg name %q exceeds %d characters", name, c.FwCfgName(), maxFwCfgName))
		}
		// libvirt passes the path in "-fw_cfg name=...,file=PATH" without
		// escaping commas, and escapes XML special characters.
		if f := s.SecretCredentialFiles[name]; !filepath.IsAbs(f) || strings.ContainsAny(f, ",<>&'\"") {
			errs = append(errs, fmt.Errorf("credential %q: file %q must be an absolute path without commas or XML special characters", name, f))
		}
	}
	return errs
}
