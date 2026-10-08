// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

// Package deps lists the Fedora packages brig needs on the host.
package deps

import (
	"fmt"
	"sort"
	"strings"
)

// Package is a Fedora package and why brig needs it.
type Package struct {
	Name   string
	Reason string
}

// Group is a set of packages needed for one feature.
type Group struct {
	Name        string
	Description string
	Packages    []Package
}

// Groups returns all package groups; the first one, "required", is always
// needed.
func Groups() []Group {
	return []Group{
		{
			Name:        "required",
			Description: "run VMs and build their images",
			Packages: []Package{
				{"libvirt-daemon-driver-qemu", "libvirt's QEMU driver (virtqemud), run as your user"},
				{"libvirt-client", "virsh, for brig console"},
				{"qemu-kvm-core", "QEMU with KVM acceleration"},
				{"qemu-img", "creates and converts VM disks"},
				{"edk2-ovmf", "UEFI firmware for the VMs"},
				{"passt", "user-mode networking for rootless VMs (passt and pasta)"},
				{"nftables", "enforces network profiles in each VM's network namespace"},
				{"openssh-clients", "brig ssh, gateway setup and image push"},
				{"mkosi", "builds the VM base images"},
				{"distribution-gpg-keys", "Fedora's package signing keys, also for releases newer than the host's"},
				{"systemd-ukify", "builds the images' unified kernel images"},
				{"systemd-udev", "provides systemd-repart, which partitions the images"},
				{"e2fsprogs", "creates the images' ext4 root file system"},
				{"dosfstools", "creates the images' EFI system partition"},
				{"mtools", "populates the EFI system partition without root"},
				{"cpio", "packs the images' initrd"},
				{"zstd", "compresses the images' initrd"},
				{"policycoreutils", "labels image files for SELinux"},
				{"selinux-policy-targeted", "SELinux policy used to label image files"},
				{"container-selinux", "SELinux policy for the images' container files"},
			},
		},
		{
			Name:        "mounts",
			Description: "share host directories with VMs (brig create --mount)",
			Packages: []Package{
				{"virtiofsd", "serves host directories to VMs over virtiofs"},
			},
		},
		{
			Name:        "push",
			Description: "copy container images from the host into VMs (brig image push)",
			Packages: []Package{
				{"podman", "exports the container images built on the host"},
			},
		},
		{
			Name:        "secrets",
			Description: "read provider secrets from your keyring (OpenShell config directories)",
			Packages: []Package{
				{"libsecret", "secret-tool, which brig sync runs to look up provider secrets"},
			},
		},
	}
}

// Packages returns the sorted, de-duplicated package names of the required
// group plus the named optional groups; "all" selects every group.
func Packages(optional ...string) ([]string, error) {
	groups := Groups()
	selected := map[string]bool{"required": true}
	for _, name := range optional {
		if name == "all" {
			for _, g := range groups {
				selected[g.Name] = true
			}
			continue
		}
		found := false
		for _, g := range groups {
			found = found || g.Name == name
		}
		if !found {
			return nil, fmt.Errorf("unknown package group %q (want %s or all)", name, strings.Join(groupNames(groups), ", "))
		}
		selected[name] = true
	}
	seen := map[string]bool{}
	var names []string
	for _, g := range groups {
		if !selected[g.Name] {
			continue
		}
		for _, p := range g.Packages {
			if !seen[p.Name] {
				seen[p.Name] = true
				names = append(names, p.Name)
			}
		}
	}
	sort.Strings(names)
	return names, nil
}

// OptionalGroupNames returns the names of the optional groups.
func OptionalGroupNames() []string { return groupNames(Groups()[1:]) }

func groupNames(groups []Group) []string {
	names := make([]string, len(groups))
	for i, g := range groups {
		names[i] = g.Name
	}
	return names
}
