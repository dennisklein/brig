// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

package libvirt

import (
	"bytes"
	"errors"
	"fmt"
	"regexp"
	"time"

	golibvirt "github.com/digitalocean/go-libvirt"
)

// This file has the operations that the vm runner uses to drive a spike VM
// from outside: screenshots and input through QEMU, commands and files
// through the guest agent, and internal snapshots.

// MaxScreenshot bounds the raw screenshot that Screenshot accepts. A guest
// picks its own display resolution; 64 MiB holds an uncompressed 4K frame
// with room to spare.
const MaxScreenshot = 64 << 20

// Screenshot returns the current frame of the running domain's display in
// the format QEMU wrote it, usually PPM, and that format's MIME type.
func (c *Conn) Screenshot(name string) ([]byte, string, error) {
	dom, err := c.c.DomainLookupByName(name)
	if err != nil {
		return nil, "", fmt.Errorf("screenshot of domain %s: %w", name, err)
	}
	w := &limitedBuffer{max: MaxScreenshot}
	mime, err := c.c.DomainScreenshot(dom, w, 0, 0)
	if w.over {
		return nil, "", fmt.Errorf("screenshot of domain %s: larger than %d bytes", name, MaxScreenshot)
	}
	if err != nil {
		return nil, "", fmt.Errorf("screenshot of domain %s: %w", name, err)
	}
	m := ""
	if len(mime) > 0 {
		m = mime[0]
	}
	return w.buf.Bytes(), m, nil
}

// limitedBuffer collects at most max bytes and fails writes beyond that.
type limitedBuffer struct {
	buf  bytes.Buffer
	max  int
	over bool
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	if b.buf.Len()+len(p) > b.max {
		b.over = true
		return 0, errors.New("screenshot too large")
	}
	return b.buf.Write(p)
}

// maxKeysPerEvent is libvirt's limit on the keys that one send-key call
// presses together (VIR_DOMAIN_SEND_KEY_MAX_KEYS).
const maxKeysPerEvent = 16

// SendKeys presses the keys with the given Linux input event codes together,
// holds them for hold and releases them.
func (c *Conn) SendKeys(name string, codes []uint32, hold time.Duration) error {
	if len(codes) == 0 || len(codes) > maxKeysPerEvent {
		return fmt.Errorf("send keys to domain %s: need 1 to %d keys, got %d", name, maxKeysPerEvent, len(codes))
	}
	if hold < 0 || hold > time.Second {
		return fmt.Errorf("send keys to domain %s: hold %s is not within a second", name, hold)
	}
	dom, err := c.c.DomainLookupByName(name)
	if err == nil {
		err = c.c.DomainSendKey(dom, uint32(golibvirt.KeycodeSetLinux), uint32(hold.Milliseconds()), codes, 0) //nolint:gosec // G115: at most 1000
	}
	if err != nil {
		return fmt.Errorf("send keys to domain %s: %w", name, err)
	}
	return nil
}

// MonitorCommand runs a QMP command, as JSON, on the running domain's QEMU
// and returns QEMU's JSON reply. libvirt marks the domain as tainted by a
// custom monitor command; that only shows in its log.
func (c *Conn) MonitorCommand(name, cmd string) (string, error) {
	dom, err := c.c.DomainLookupByName(name)
	if err != nil {
		return "", fmt.Errorf("QEMU monitor of domain %s: %w", name, err)
	}
	out, err := c.c.QEMUDomainMonitorCommand(dom, cmd, 0)
	if err != nil {
		return "", fmt.Errorf("QEMU monitor of domain %s: %w", name, err)
	}
	return out, nil
}

// AgentCommand runs a command, as JSON, on the guest agent of the running
// domain and returns the agent's JSON reply. It waits at most timeout, at
// least a second, for the agent to answer.
func (c *Conn) AgentCommand(name, cmd string, timeout time.Duration) (string, error) {
	dom, err := c.c.DomainLookupByName(name)
	if err != nil {
		return "", fmt.Errorf("guest agent of domain %s: %w", name, err)
	}
	secs := max(int32(timeout/time.Second), 1) //nolint:gosec // G115: callers pass minutes, not decades
	out, err := c.c.QEMUDomainAgentCommand(dom, cmd, secs, 0)
	if err != nil {
		return "", fmt.Errorf("guest agent of domain %s: %w", name, err)
	}
	if len(out) == 0 {
		return "", fmt.Errorf("guest agent of domain %s: empty reply", name)
	}
	return out[0], nil
}

var snapshotNameRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

// snapshotXML asks for an internal snapshot of disk vda and the RAM. The
// name is checked against snapshotNameRE, so it needs no escaping.
const snapshotXML = `<domainsnapshot><name>%s</name><memory snapshot="internal"/>` +
	`<disks><disk name="vda" snapshot="internal"/></disks></domainsnapshot>`

// CreateSnapshot takes an internal snapshot of the domain's disk vda and,
// if it runs, its RAM. Internal snapshots live in the disk's qcow2 file, so
// deleting the file deletes them.
func (c *Conn) CreateSnapshot(name, snap string) error {
	if !snapshotNameRE.MatchString(snap) {
		return fmt.Errorf("invalid snapshot name %q", snap)
	}
	dom, err := c.lookupOwned(name)
	if err == nil {
		_, err = c.c.DomainSnapshotCreateXML(dom, fmt.Sprintf(snapshotXML, snap), uint32(golibvirt.DomainSnapshotCreateAtomic))
	}
	if err != nil {
		return fmt.Errorf("snapshot domain %s: %w", name, err)
	}
	return nil
}

// RevertSnapshot returns the domain to the named snapshot, running if it
// ran when the snapshot was taken.
func (c *Conn) RevertSnapshot(name, snap string) error {
	if !snapshotNameRE.MatchString(snap) {
		return fmt.Errorf("invalid snapshot name %q", snap)
	}
	dom, err := c.lookupOwned(name)
	if err != nil {
		return fmt.Errorf("revert domain %s: %w", name, err)
	}
	s, err := c.c.DomainSnapshotLookupByName(dom, snap, 0)
	if err == nil {
		err = c.c.DomainRevertToSnapshot(s, uint32(golibvirt.DomainSnapshotRevertForce))
	}
	if err != nil {
		return fmt.Errorf("revert domain %s to snapshot %s: %w", name, snap, err)
	}
	return nil
}

// DeleteSnapshot deletes the named snapshot of the domain.
func (c *Conn) DeleteSnapshot(name, snap string) error {
	if !snapshotNameRE.MatchString(snap) {
		return fmt.Errorf("invalid snapshot name %q", snap)
	}
	dom, err := c.lookupOwned(name)
	if err != nil {
		return fmt.Errorf("delete snapshot of domain %s: %w", name, err)
	}
	s, err := c.c.DomainSnapshotLookupByName(dom, snap, 0)
	if err == nil {
		err = c.c.DomainSnapshotDelete(s, 0)
	}
	if err != nil {
		return fmt.Errorf("delete snapshot %s of domain %s: %w", snap, name, err)
	}
	return nil
}
