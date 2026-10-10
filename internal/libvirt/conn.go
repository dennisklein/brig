// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

package libvirt

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/GSI-HPC/go-clikit/progress"
	golibvirt "github.com/digitalocean/go-libvirt"
	"github.com/digitalocean/go-libvirt/socket/dialers"
	"libvirt.org/go/libvirtxml"

	"github.com/dennisklein/brig/internal/bytesize"
)

// client is the subset of *golibvirt.Libvirt that Conn uses. Its methods
// may be called concurrently.
type client interface {
	DomainLookupByName(name string) (golibvirt.Domain, error)
	DomainGetState(dom golibvirt.Domain, flags uint32) (state, reason int32, err error)
	DomainHasManagedSaveImage(dom golibvirt.Domain, flags uint32) (int32, error)
	DomainGetMetadata(dom golibvirt.Domain, typ int32, uri golibvirt.OptString, flags golibvirt.DomainModificationImpact) (string, error)
	DomainGetXMLDesc(dom golibvirt.Domain, flags golibvirt.DomainXMLFlags) (string, error)
	DomainDefineXMLFlags(xml string, flags golibvirt.DomainDefineFlags) (golibvirt.Domain, error)
	DomainCreateWithFlags(dom golibvirt.Domain, flags uint32) (golibvirt.Domain, error)
	DomainResume(dom golibvirt.Domain) error
	DomainShutdownFlags(dom golibvirt.Domain, flags golibvirt.DomainShutdownFlagValues) error
	DomainDestroyFlags(dom golibvirt.Domain, flags golibvirt.DomainDestroyFlagsValues) error
	DomainManagedSaveRemove(dom golibvirt.Domain, flags uint32) error
	DomainUndefineFlags(dom golibvirt.Domain, flags golibvirt.DomainUndefineFlagsValues) error
	DomainGetBlockInfo(dom golibvirt.Domain, path string, flags uint32) (allocation, capacity, physical uint64, err error)
	DomainBlockResize(dom golibvirt.Domain, disk string, size uint64, flags golibvirt.DomainBlockResizeFlags) error
	Disconnect() error
}

// Conn is a connection to the user's libvirt session daemon.
type Conn struct {
	c client
	// poll is the interval at which Shutdown checks the domain state.
	poll time.Duration
	// resend is the interval at which Shutdown repeats its request, which
	// a guest that is still booting may miss.
	resend time.Duration
}

// Connect connects to the user's libvirt session daemon (qemu:///session).
// Like libvirt's own clients, it starts virtqemud when nothing listens on
// the session socket, unless LIBVIRT_AUTOSTART=0.
func Connect(ctx context.Context) (*Conn, error) {
	s, err := newSession(os.Geteuid(), os.Getenv)
	if err != nil {
		return nil, err
	}
	sock, err := s.connect(ctx)
	if err != nil {
		return nil, fmt.Errorf("connect to libvirt session daemon: %w", err)
	}
	// Never use golibvirt.Connect: it asks for qemu:///system.
	l := golibvirt.NewWithDialer(dialers.NewAlreadyConnected(sock))
	if err := l.ConnectToURI(golibvirt.QEMUSession); err != nil {
		_ = sock.Close()
		return nil, fmt.Errorf("connect to libvirt session daemon: %w", err)
	}
	return newConn(l), nil
}

func newConn(c client) *Conn {
	return &Conn{c: c, poll: 500 * time.Millisecond, resend: 5 * time.Second}
}

// Close closes the connection.
func (c *Conn) Close() error { return c.c.Disconnect() }

// State is the coarse state of a domain.
type State int

// Domain states.
const (
	// StateMissing means that no domain of that name is defined.
	StateMissing State = iota
	StateRunning
	StateShutoff
	// StatePaused means that the guest is suspended, either in memory or
	// saved to disk by the session daemon (managed save).
	StatePaused
	// StateOther covers crashed, suspended-to-RAM (S3) and unknown domains.
	StateOther
)

func (s State) String() string {
	switch s {
	case StateMissing:
		return "missing"
	case StateRunning:
		return "running"
	case StateShutoff:
		return "shut off"
	case StatePaused:
		return "paused"
	default:
		return "other"
	}
}

func stateOf(s golibvirt.DomainState) State {
	switch s {
	case golibvirt.DomainRunning, golibvirt.DomainBlocked, golibvirt.DomainShutdown:
		// DomainShutdown means "being shut down": the guest still runs.
		return StateRunning
	case golibvirt.DomainShutoff:
		return StateShutoff
	case golibvirt.DomainPaused:
		return StatePaused
	default:
		return StateOther
	}
}

// State returns the state of the named domain; StateMissing if there is
// no such domain.
func (c *Conn) State(name string) (State, error) {
	dom, err := c.c.DomainLookupByName(name)
	if err == nil {
		return c.state(dom)
	}
	if golibvirt.IsNotFound(err) {
		return StateMissing, nil
	}
	return 0, fmt.Errorf("domain %s: %w", name, err)
}

func (c *Conn) state(dom golibvirt.Domain) (State, error) {
	raw, _, err := c.c.DomainGetState(dom, 0)
	st := stateOf(golibvirt.DomainState(raw))
	if err == nil && st == StateShutoff {
		var saved int32
		if saved, err = c.c.DomainHasManagedSaveImage(dom, 0); saved == 1 {
			st = StatePaused
		}
	}
	if golibvirt.IsNotFound(err) {
		return StateMissing, nil
	}
	if err != nil {
		return 0, fmt.Errorf("domain %s: %w", dom.Name, err)
	}
	return st, nil
}

// Define defines a persistent domain from domainXML or replaces the
// definition of the existing domain of the same name and UUID. Changes to a
// running or suspended domain take effect when it next boots. Define
// refuses to replace a domain that brig did not create, and one whose disks
// no longer are the files domainXML names, as after an external snapshot.
func (c *Conn) Define(domainXML string) error {
	var d libvirtxml.Domain
	if err := d.Unmarshal(domainXML); err != nil {
		return fmt.Errorf("parse domain XML: %w", err)
	}
	dom, err := c.lookupOwned(d.Name)
	if err != nil && !golibvirt.IsNotFound(err) {
		return err
	}
	if err == nil {
		if err := c.checkDisks(dom, &d); err != nil {
			return err
		}
	}
	if _, err := c.c.DomainDefineXMLFlags(domainXML, golibvirt.DomainDefineValidate); err != nil {
		return fmt.Errorf("define domain %s: %w", d.Name, err)
	}
	return nil
}

// checkDisks fails if a disk of dom's definition is not the file that want
// names for the same target. libvirt points the definition at an overlay
// file when a snapshot is taken with virsh or virt-manager. Redefining the
// disk as the base would roll the guest back to the snapshot and let it
// write into the backing file of the overlay, which holds the work since.
func (c *Conn) checkDisks(dom golibvirt.Domain, want *libvirtxml.Domain) error {
	x, err := c.c.DomainGetXMLDesc(dom, golibvirt.DomainXMLInactive)
	if err != nil {
		return fmt.Errorf("domain %s: %w", dom.Name, err)
	}
	var have libvirtxml.Domain
	if err := have.Unmarshal(x); err != nil {
		return fmt.Errorf("parse XML of domain %s: %w", dom.Name, err)
	}
	return diskMismatch(&have, want)
}

func diskMismatch(have, want *libvirtxml.Domain) error {
	if have.Devices == nil || want.Devices == nil {
		return nil
	}
	for _, w := range want.Devices.Disks {
		if w.Target == nil || w.Source == nil || w.Source.File == nil {
			continue
		}
		for _, h := range have.Devices.Disks {
			if h.Target == nil || h.Target.Dev != w.Target.Dev {
				continue
			}
			if h.Source == nil || h.Source.File == nil || h.Source.File.File != w.Source.File.File {
				cur := "another source"
				if h.Source != nil && h.Source.File != nil {
					cur = h.Source.File.File
				}
				return fmt.Errorf("domain %s: disk %s is %s, not %s; brig does not support snapshots made with virsh or virt-manager, so merge the snapshot's overlay into the disk first",
					have.Name, w.Target.Dev, cur, w.Source.File.File)
			}
		}
	}
	return nil
}

// Start boots the named domain or, if it is paused, resumes it. A domain
// saved to disk resumes with the configuration and credentials of its
// previous boot.
func (c *Conn) Start(name string) error {
	dom, err := c.c.DomainLookupByName(name)
	if err == nil {
		err = c.start(dom)
	}
	if err != nil {
		return fmt.Errorf("start domain %s: %w", name, err)
	}
	return nil
}

func (c *Conn) start(dom golibvirt.Domain) error {
	raw, _, err := c.c.DomainGetState(dom, 0)
	if err != nil {
		return err
	}
	if golibvirt.DomainState(raw) == golibvirt.DomainPaused {
		return c.c.DomainResume(dom)
	}
	// This restores a managed-save image if there is one.
	_, err = c.c.DomainCreateWithFlags(dom, 0)
	return err
}

// resume resumes dom if it is paused. A managed-save image of a domain that
// was paused when it was saved restores paused.
func (c *Conn) resume(dom golibvirt.Domain) error {
	raw, _, err := c.c.DomainGetState(dom, 0)
	if err == nil && golibvirt.DomainState(raw) == golibvirt.DomainPaused {
		err = c.c.DomainResume(dom)
	}
	return err
}

// Shutdown asks the guest to power off, via the guest agent if it runs or
// else via the ACPI power button, and waits up to timeout for the domain
// to shut off before it destroys the domain. It resumes a paused domain
// first so that the guest can shut down cleanly, and destroys it if it
// cannot be resumed: a domain saved to disk restores only while its
// network socket is served, for instance. It returns nil if the domain is
// shut off or missing. If ctx ends first, Shutdown returns ctx's error and
// leaves the guest to finish shutting down on its own.
func (c *Conn) Shutdown(ctx context.Context, name string, timeout time.Duration) error {
	dom, err := c.c.DomainLookupByName(name)
	if golibvirt.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("shut down domain %s: %w", name, err)
	}
	st, err := c.state(dom)
	switch {
	case err != nil:
		return err
	case st == StateShutoff || st == StateMissing:
		return nil
	case st == StateOther:
		return c.Destroy(name)
	case st == StatePaused:
		if c.start(dom) != nil || c.resume(dom) != nil {
			return c.Destroy(name)
		}
	}
	wctx, wait := progress.Start(ctx, progress.KindWait, "wait for the guest to power off")
	done, err := c.awaitShutoff(wctx, dom, timeout)
	wait.End(err)
	if done || err != nil {
		return err
	}
	return c.Destroy(name)
}

// awaitShutoff requests a guest shutdown and waits for it. It reports
// whether the domain shut off before the timeout.
//
// Through the guest agent, libvirt answers a shutdown request only once the
// guest has powered off, or after a minute. Requests therefore run in the
// background, one at a time, while the timeout and ctx keep applying.
func (c *Conn) awaitShutoff(ctx context.Context, dom golibvirt.Domain, timeout time.Duration) (bool, error) {
	answered := make(chan error, 1)
	pending, first := false, true
	var lastRequest time.Time
	request := func() {
		pending, lastRequest = true, time.Now()
		go func() {
			err := c.c.DomainShutdownFlags(dom, golibvirt.DomainShutdownGuestAgent|golibvirt.DomainShutdownAcpiPowerBtn)
			if isCode(err, golibvirt.ErrOperationInvalid) || golibvirt.IsNotFound(err) {
				err = nil // not running (any more); the state check tells
			}
			answered <- err
		}()
	}
	request()
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	poll := time.NewTicker(c.poll)
	defer poll.Stop()
	for {
		select {
		case <-ctx.Done():
			return false, ctx.Err()
		case <-deadline.C:
			return false, nil
		case err := <-answered:
			if err != nil && first {
				return false, fmt.Errorf("shut down domain %s: %w", dom.Name, err)
			}
			pending, first = false, false // later requests are best effort
			continue
		case <-poll.C:
		}
		st, err := c.state(dom)
		if err != nil {
			return false, err
		}
		if st == StateShutoff || st == StateMissing {
			return true, nil
		}
		if !pending && time.Since(lastRequest) >= c.resend {
			request()
		}
	}
}

// Destroy powers the named domain off immediately, like pulling the plug,
// and discards its memory if the domain was saved to disk. It returns nil
// if the domain is shut off or missing.
func (c *Conn) Destroy(name string) error {
	dom, err := c.c.DomainLookupByName(name)
	if err == nil {
		err = c.c.DomainDestroyFlags(dom, golibvirt.DomainDestroyDefault)
		if isCode(err, golibvirt.ErrOperationInvalid) {
			err = nil // not running
		}
	}
	if err == nil {
		var saved int32
		if saved, err = c.c.DomainHasManagedSaveImage(dom, 0); saved == 1 {
			err = c.c.DomainManagedSaveRemove(dom, 0)
		}
	}
	if err == nil || golibvirt.IsNotFound(err) {
		return nil
	}
	return fmt.Errorf("destroy domain %s: %w", name, err)
}

// Undefine destroys the named domain and deletes its definition along with
// its UEFI variable store and any snapshot or checkpoint metadata. It
// returns nil if the domain is missing and refuses to delete a domain that
// brig did not create.
func (c *Conn) Undefine(name string) error {
	dom, err := c.lookupOwned(name)
	if golibvirt.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if err := c.Destroy(name); err != nil {
		return err
	}
	flags := golibvirt.DomainUndefineNvram | golibvirt.DomainUndefineManagedSave |
		golibvirt.DomainUndefineSnapshotsMetadata | golibvirt.DomainUndefineCheckpointsMetadata
	if err := c.c.DomainUndefineFlags(dom, flags); err != nil && !golibvirt.IsNotFound(err) {
		return fmt.Errorf("undefine domain %s: %w", name, err)
	}
	return nil
}

// BlockResize grows the disk (target name such as "vdb", or source path)
// of the running domain to size bytes; the guest sees the new size at
// once. It refuses to shrink the disk, which QEMU would do without asking.
// Resize the image of a stopped domain with qemu-img instead.
func (c *Conn) BlockResize(name, disk string, size bytesize.Size) error {
	dom, err := c.c.DomainLookupByName(name)
	if err == nil {
		err = c.blockResize(dom, disk, size)
	}
	if err != nil {
		return fmt.Errorf("resize %s of domain %s: %w", disk, name, err)
	}
	return nil
}

func (c *Conn) blockResize(dom golibvirt.Domain, disk string, size bytesize.Size) error {
	// Round up to whole sectors, as qemu-img does when creating and resizing
	// images, so a VM record's size and the disk's capacity agree.
	size = (size + 511) &^ 511
	// libvirt refuses to shrink only since 12.3.0 and with a flag that
	// older versions reject, so compare with the capacity here.
	_, capacity, _, err := c.c.DomainGetBlockInfo(dom, disk, 0)
	if err != nil {
		return err
	}
	if uint64(size) < capacity {
		return fmt.Errorf("refusing to shrink it from %s to %s", bytesize.Size(capacity), size)
	}
	return c.c.DomainBlockResize(dom, disk, uint64(size), golibvirt.DomainBlockResizeBytes)
}

// lookupOwned looks up the named domain and checks that it carries brig's
// metadata. Errors for a missing domain satisfy golibvirt.IsNotFound.
func (c *Conn) lookupOwned(name string) (golibvirt.Domain, error) {
	dom, err := c.c.DomainLookupByName(name)
	if err != nil {
		return dom, err
	}
	_, err = c.c.DomainGetMetadata(dom, int32(golibvirt.DomainMetadataElement),
		golibvirt.OptString{MetadataNamespace}, golibvirt.DomainAffectCurrent)
	if isCode(err, golibvirt.ErrNoDomainMetadata) {
		return dom, fmt.Errorf("domain %s exists but was not created by brig; rename or remove it", name)
	}
	if err != nil {
		return dom, fmt.Errorf("domain %s: %w", name, err)
	}
	return dom, nil
}

// isCode reports whether err is a libvirt error with the given code.
func isCode(err error, code golibvirt.ErrorNumber) bool {
	var e golibvirt.Error
	return errors.As(err, &e) && int64(e.Code) == int64(code)
}
