// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

// Package guest defines what runs inside a brig VM: the mkosi configuration
// of the base image and the systemd credentials that configure each boot.
package guest

// Fixed facts about every brig base image. Changing them breaks VMs whose
// persistent data disk was initialised by an older image.
const (
	// User runs the OpenShell gateway and owns /home/User, which lives on
	// the VM's persistent data disk.
	User = "agent"
	// UID is User's UID and GID.
	UID = 1000
	// SSHPort is the guest port of sshd.
	SSHPort = 22
	// GatewayPort is the guest port at which the OpenShell gateway is
	// reachable from the host. A socket proxy forwards it to the gateway,
	// which itself listens on 127.0.0.1:17670 only.
	GatewayPort = 17671
	// DataDiskSerial is the virtio serial of the data disk; the guest finds
	// it as /dev/disk/by-id/virtio-<DataDiskSerial>.
	DataDiskSerial = "brig-data"
	// HostKeyCredential carries the VM's SSH host private key. It is a
	// secret credential, so it must be passed as an fw_cfg file.
	HostKeyCredential = "tmpfiles.brig-hostkey" //nolint:gosec // G101: a credential name, not a credential
)

// home is User's home directory, the mount point of the data disk.
const home = "/home/" + User
