// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

// Package libvirt runs brig's VMs as domains of the user's libvirt session
// daemon (qemu:///session), the same daemon that virsh and virt-manager use
// for unprivileged VMs. It speaks libvirt's RPC protocol in pure Go and
// renders domain definitions with libvirtxml.
//
// When the user logs out, the session daemon saves the memory of running
// domains to disk (managed save) and tries to restore them when it starts
// again. A restore fails, and the saved state stays, while nothing serves
// the domain's vhost-user network socket. A saved domain counts as paused:
// Start restores it once the socket is served, while Shutdown and Destroy
// leave no saved state behind, so that the next Start boots afresh from
// possibly replaced disks.
package libvirt
