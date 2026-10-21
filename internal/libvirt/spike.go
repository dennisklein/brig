// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

package libvirt

import (
	"encoding/xml"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"libvirt.org/go/libvirtxml"

	"github.com/dennisklein/brig/internal/bytesize"
)

// SpikeSpec describes the libvirt domain of a spike VM: a throwaway VM that
// the vm runner starts for an agent from an image the user allows. An agent
// drives it only through the runner, by screenshots, input events and the
// guest agent.
type SpikeSpec struct {
	// Name is the domain name, e.g. "brig-spike-0123abcd".
	Name string
	// UUID is the domain UUID in canonical form.
	UUID string
	// CPUs is the number of virtual CPUs.
	CPUs int
	// Memory is the guest RAM, a whole number of MiB.
	Memory bytesize.Size
	// Disk is the qcow2 overlay on the qcow2 BaseImage. Both are absolute
	// paths.
	Disk, BaseImage string
	// Owner names the sandbox that the VM belongs to. It is recorded in
	// the domain's metadata for people who look at the domain with virsh.
	Owner string
}

// spikeMetadata is brig's metadata element in a spike VM's domain. It has
// brig's namespace, so Define and Undefine treat the domain as brig's.
type spikeMetadata struct {
	XMLName xml.Name `xml:"brig:vm"`
	NS      string   `xml:"xmlns:brig,attr"`
	Role    string   `xml:"role,attr"`
	Owner   string   `xml:"owner,attr"`
}

// SpikeRole is the role in the metadata of spike VMs' domains.
const SpikeRole = "spike"

// SpikeDomainXML renders the persistent domain definition of a spike VM: a
// q35 KVM guest booting with BIOS, which, unlike UEFI with its pflash
// variable store, allows internal snapshots of disk and RAM. It has a
// display for screenshots, a virtio tablet and keyboard for input and the
// guest agent's channel, and nothing else that reaches the host: no network
// interface, no shared directories, no serial log on the host's disk.
func SpikeDomainXML(s SpikeSpec) (string, error) {
	if err := s.validate(); err != nil {
		return "", fmt.Errorf("domain %q: %w", s.Name, err)
	}
	meta, err := xml.Marshal(spikeMetadata{NS: MetadataNamespace, Role: SpikeRole, Owner: s.Owner})
	if err != nil {
		return "", err
	}
	root := disk("vda", s.Disk)
	root.BackingStore = &libvirtxml.DomainDiskBackingStore{
		Format:       &libvirtxml.DomainDiskFormat{Type: "qcow2"},
		Source:       fileSource(s.BaseImage),
		BackingStore: &libvirtxml.DomainDiskBackingStore{},
	}
	root.Boot = &libvirtxml.DomainDeviceBoot{Order: 1}
	devs := &libvirtxml.DomainDeviceList{
		Controllers: []libvirtxml.DomainController{{Type: "usb", Model: "none"}},
		Disks:       []libvirtxml.DomainDisk{root},
		// No graphics element: QEMU still renders the console of the video
		// device, which screenshots read, but listens nowhere.
		Videos: []libvirtxml.DomainVideo{{Model: libvirtxml.DomainVideoModel{Type: "virtio", Heads: 1, Primary: "yes"}}},
		Inputs: []libvirtxml.DomainInput{
			{Type: "tablet", Bus: "virtio"},
			{Type: "keyboard", Bus: "virtio"},
		},
		Channels: []libvirtxml.DomainChannel{{
			Source: &libvirtxml.DomainChardevSource{UNIX: &libvirtxml.DomainChardevSourceUNIX{}},
			Target: &libvirtxml.DomainChannelTarget{
				VirtIO: &libvirtxml.DomainChannelTargetVirtIO{Name: "org.qemu.guest_agent.0"},
			},
		}},
		RNGs: []libvirtxml.DomainRNG{{
			Model:   "virtio",
			Backend: &libvirtxml.DomainRNGBackend{Random: &libvirtxml.DomainRNGBackendRandom{Device: "/dev/urandom"}},
		}},
		MemBalloon: &libvirtxml.DomainMemBalloon{
			Model: "virtio",
			Stats: &libvirtxml.DomainMemBalloonStats{Period: statsPeriod},
		},
	}
	d := libvirtxml.Domain{
		Type:     "kvm",
		Name:     s.Name,
		UUID:     s.UUID,
		Metadata: &libvirtxml.DomainMetadata{XML: string(meta)},
		Memory:   &libvirtxml.DomainMemory{Value: uint(s.Memory.MiB()), Unit: "MiB"},
		VCPU:     &libvirtxml.DomainVCPU{Placement: "static", Value: uint(s.CPUs)},
		OS: &libvirtxml.DomainOS{
			Type: &libvirtxml.DomainOSType{Arch: "x86_64", Machine: "q35", Type: "hvm"},
		},
		Features: &libvirtxml.DomainFeatureList{
			ACPI: &libvirtxml.DomainFeature{},
			APIC: &libvirtxml.DomainFeatureAPIC{},
			// The virtio tablet and keyboard replace PS/2, and nothing
			// needs the VMware backdoor port.
			PS2:    &libvirtxml.DomainFeatureState{State: "off"},
			VMPort: &libvirtxml.DomainFeatureState{State: "off"},
		},
		// As in gateways, nested virtualization stays off.
		CPU: &libvirtxml.DomainCPU{
			Mode: "host-passthrough",
			Features: []libvirtxml.DomainCPUFeature{
				{Policy: "disable", Name: "vmx"},
				{Policy: "disable", Name: "svm"},
			},
		},
		PM: &libvirtxml.DomainPM{
			SuspendToMem:  &libvirtxml.DomainPMPolicy{Enabled: "no"},
			SuspendToDisk: &libvirtxml.DomainPMPolicy{Enabled: "no"},
		},
		OnPoweroff: "destroy",
		OnReboot:   "restart",
		OnCrash:    "destroy",
		Devices:    devs,
	}
	return d.Marshal()
}

func (s SpikeSpec) validate() error {
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
	check(filepath.IsAbs(s.Disk), "disk path %q is not absolute", s.Disk)
	check(filepath.IsAbs(s.BaseImage), "base image path %q is not absolute", s.BaseImage)
	return errors.Join(errs...)
}
