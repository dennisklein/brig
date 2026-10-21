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

// Fixed facts about rootless Docker in brig VMs, which sandboxes use when
// brig sandbox create --docker lets them.
const (
	// DockerUser runs rootless dockerd. It owns nothing but its home, so
	// a sandbox that takes over Docker cannot read User's files.
	DockerUser = "dockerd"
	// DockerUID is DockerUser's UID and GID.
	DockerUID = 1001
	// DockerPort is the VM's loopback port of the Docker API, which a
	// sandbox reaches as host.openshell.internal:DockerPort.
	DockerPort = 2375
	// DockerSocket is dockerd's API socket in the VM. A container that
	// drives Docker, such as testcontainers' Ryuk, mounts it.
	DockerSocket = "/run/user/1001/docker.sock"
	// FirstPublishedPort and LastPublishedPort bound the host ports that
	// Docker picks for published container ports; see
	// /usr/libexec/brig/dockerd.
	FirstPublishedPort = 61000
	LastPublishedPort  = 61127
)

// Where DockerUser's home lives: a directory on the data disk, so that
// images and volumes last like Podman's, bind-mounted outside home, which
// DockerUser cannot enter.
const (
	dockerHome = "/home/" + DockerUser
	dockerData = home + "/.local/share/brig/docker"
)

// home is User's home directory, the mount point of the data disk.
const home = "/home/" + User
