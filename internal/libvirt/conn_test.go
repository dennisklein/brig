// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

package libvirt

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/GSI-HPC/go-clikit/progress/progresstest"
	golibvirt "github.com/digitalocean/go-libvirt"
	"libvirt.org/go/libvirtxml"

	"github.com/dennisklein/brig/internal/bytesize"
)

type fakeDomain struct {
	state golibvirt.DomainState
	owned bool
	// saved is true for a shut-off domain with a managed-save image.
	saved bool
	// unrestorable makes restoring the managed-save image fail, as when
	// QEMU cannot connect to the network backend's socket.
	unrestorable bool
	// restoresPaused makes restoring the managed-save image leave the
	// domain paused, as when it was paused when it was saved.
	restoresPaused bool
	// xml is the definition the domain was last defined from.
	xml string
	// disks maps target names to capacities in bytes.
	disks map[string]uint64
	// offAfter is the number of state queries after a shutdown request
	// until the guest is off; negative if the guest ignores requests.
	offAfter int
	stopping bool
}

// fakeClient is an in-memory libvirt daemon that records the calls made.
// Like go-libvirt, it may be called concurrently.
type fakeClient struct {
	mu    sync.Mutex
	doms  map[string]*fakeDomain
	calls []string
	// shutdownBlock, if set, makes accepted shutdown requests block until
	// it is closed, as libvirt does while the guest agent shuts the guest
	// down.
	shutdownBlock chan struct{}
	// shutdownErr, if set, fails shutdown requests for running domains.
	shutdownErr error
}

func libvirtErr(code golibvirt.ErrorNumber, msg string) error {
	return golibvirt.Error{Code: uint32(code), Message: msg} //nolint:gosec // G115: libvirt error codes are small and positive
}

// record and dom expect f.mu to be held.
func (f *fakeClient) record(format string, args ...any) {
	f.calls = append(f.calls, fmt.Sprintf(format, args...))
}

func (f *fakeClient) dom(d golibvirt.Domain) (*fakeDomain, error) {
	if fd, ok := f.doms[d.Name]; ok {
		return fd, nil
	}
	return nil, libvirtErr(golibvirt.ErrNoDomain, "Domain not found")
}

// recorded returns a copy of the calls made so far.
func (f *fakeClient) recorded() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.calls)
}

// domain returns a copy of the named domain's state.
func (f *fakeClient) domain(name string) fakeDomain {
	f.mu.Lock()
	defer f.mu.Unlock()
	return *f.doms[name]
}

func (f *fakeClient) DomainLookupByName(name string) (golibvirt.Domain, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, err := f.dom(golibvirt.Domain{Name: name})
	return golibvirt.Domain{Name: name}, err
}

func (f *fakeClient) DomainGetState(d golibvirt.Domain, _ uint32) (int32, int32, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fd, err := f.dom(d)
	if err != nil {
		return 0, 0, err
	}
	if fd.stopping {
		if fd.offAfter == 0 {
			fd.state, fd.stopping = golibvirt.DomainShutoff, false
		}
		fd.offAfter--
	}
	return int32(fd.state), 0, nil
}

func (f *fakeClient) DomainHasManagedSaveImage(d golibvirt.Domain, _ uint32) (int32, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fd, err := f.dom(d)
	if err != nil || !fd.saved {
		return 0, err
	}
	return 1, nil
}

func (f *fakeClient) DomainManagedSaveRemove(d golibvirt.Domain, _ uint32) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("managedsave-remove %s", d.Name)
	fd, err := f.dom(d)
	if err != nil {
		return err
	}
	if !fd.saved {
		return libvirtErr(golibvirt.ErrSystemError, "Failed to remove managed save file")
	}
	fd.saved = false
	return nil
}

func (f *fakeClient) DomainResume(d golibvirt.Domain) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("resume %s", d.Name)
	fd, err := f.dom(d)
	if err != nil {
		return err
	}
	if fd.state != golibvirt.DomainPaused {
		return libvirtErr(golibvirt.ErrOperationInvalid, "domain is not paused")
	}
	fd.state = golibvirt.DomainRunning
	return nil
}

func (f *fakeClient) DomainGetMetadata(d golibvirt.Domain, typ int32, uri golibvirt.OptString, _ golibvirt.DomainModificationImpact) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fd, err := f.dom(d)
	if err != nil {
		return "", err
	}
	if typ != int32(golibvirt.DomainMetadataElement) || !slices.Equal(uri, golibvirt.OptString{MetadataNamespace}) || !fd.owned {
		return "", libvirtErr(golibvirt.ErrNoDomainMetadata, "metadata not found")
	}
	return metadataXML, nil
}

func (f *fakeClient) DomainGetXMLDesc(d golibvirt.Domain, flags golibvirt.DomainXMLFlags) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fd, err := f.dom(d)
	if err != nil {
		return "", err
	}
	if flags != golibvirt.DomainXMLInactive {
		return "", fmt.Errorf("unexpected flags %d", flags)
	}
	return fd.xml, nil
}

func (f *fakeClient) DomainDefineXMLFlags(x string, flags golibvirt.DomainDefineFlags) (golibvirt.Domain, error) {
	var d libvirtxml.Domain
	if err := d.Unmarshal(x); err != nil {
		return golibvirt.Domain{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("define %s %d", d.Name, flags)
	if fd, ok := f.doms[d.Name]; ok {
		fd.owned = strings.Contains(x, MetadataNamespace)
		fd.xml = x
	} else {
		f.doms[d.Name] = &fakeDomain{state: golibvirt.DomainShutoff, owned: strings.Contains(x, MetadataNamespace), xml: x}
	}
	return golibvirt.Domain{Name: d.Name}, nil
}

func (f *fakeClient) DomainCreateWithFlags(d golibvirt.Domain, flags uint32) (golibvirt.Domain, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("create %s %d", d.Name, flags)
	fd, err := f.dom(d)
	if err != nil {
		return d, err
	}
	if fd.state != golibvirt.DomainShutoff {
		return d, libvirtErr(golibvirt.ErrOperationInvalid, "domain is already running")
	}
	if fd.saved && fd.unrestorable {
		// libvirt keeps the image when the restore fails.
		return d, libvirtErr(golibvirt.ErrOperationFailed, "Failed to connect to net.sock")
	}
	fd.state, fd.saved = golibvirt.DomainRunning, false // restores a saved domain
	if fd.restoresPaused {
		fd.state = golibvirt.DomainPaused
	}
	return d, nil
}

func (f *fakeClient) DomainShutdownFlags(d golibvirt.Domain, flags golibvirt.DomainShutdownFlagValues) error {
	f.mu.Lock()
	f.record("shutdown %s %d", d.Name, flags)
	fd, err := f.dom(d)
	if err == nil && fd.state != golibvirt.DomainRunning {
		err = libvirtErr(golibvirt.ErrOperationInvalid, "domain is not running")
	}
	if err == nil {
		err = f.shutdownErr
	}
	if err == nil && fd.offAfter >= 0 {
		fd.stopping = true
	}
	block := f.shutdownBlock
	f.mu.Unlock()
	if err == nil && block != nil {
		<-block
	}
	return err
}

func (f *fakeClient) DomainDestroyFlags(d golibvirt.Domain, flags golibvirt.DomainDestroyFlagsValues) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("destroy %s %d", d.Name, flags)
	fd, err := f.dom(d)
	if err != nil {
		return err
	}
	if fd.state == golibvirt.DomainShutoff {
		return libvirtErr(golibvirt.ErrOperationInvalid, "domain is not running")
	}
	fd.state, fd.stopping = golibvirt.DomainShutoff, false
	return nil
}

func (f *fakeClient) DomainUndefineFlags(d golibvirt.Domain, flags golibvirt.DomainUndefineFlagsValues) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("undefine %s %d", d.Name, flags)
	if _, err := f.dom(d); err != nil {
		return err
	}
	delete(f.doms, d.Name)
	return nil
}

func (f *fakeClient) DomainGetBlockInfo(d golibvirt.Domain, disk string, _ uint32) (uint64, uint64, uint64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fd, err := f.dom(d)
	if err != nil {
		return 0, 0, 0, err
	}
	capacity, ok := fd.disks[disk]
	if !ok {
		return 0, 0, 0, libvirtErr(golibvirt.ErrInvalidArg, "disk not found")
	}
	return 0, capacity, 0, nil
}

func (f *fakeClient) DomainBlockResize(d golibvirt.Domain, disk string, size uint64, flags golibvirt.DomainBlockResizeFlags) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("resize %s %s %d %d", d.Name, disk, size, flags)
	fd, err := f.dom(d)
	if err == nil {
		fd.disks[disk] = size
	}
	return err
}

func (f *fakeClient) Disconnect() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("disconnect")
	return nil
}

func newFake(doms map[string]*fakeDomain) (*Conn, *fakeClient) {
	f := &fakeClient{doms: doms}
	c := newConn(f)
	c.poll, c.resend = time.Millisecond, 5*time.Millisecond
	return c, f
}

func running(offAfter int) *fakeDomain {
	return &fakeDomain{state: golibvirt.DomainRunning, owned: true, offAfter: offAfter}
}

func checkCalls(t *testing.T, f *fakeClient, want ...string) {
	t.Helper()
	if got := f.recorded(); !slices.Equal(got, want) {
		t.Errorf("calls = %q, want %q", got, want)
	}
}

func TestStateOf(t *testing.T) {
	for in, want := range map[golibvirt.DomainState]State{
		golibvirt.DomainNostate:     StateOther,
		golibvirt.DomainRunning:     StateRunning,
		golibvirt.DomainBlocked:     StateRunning,
		golibvirt.DomainPaused:      StatePaused,
		golibvirt.DomainShutdown:    StateRunning,
		golibvirt.DomainShutoff:     StateShutoff,
		golibvirt.DomainCrashed:     StateOther,
		golibvirt.DomainPmsuspended: StateOther,
	} {
		if got := stateOf(in); got != want {
			t.Errorf("stateOf(%d) = %s, want %s", in, got, want)
		}
	}
	for s, want := range map[State]string{
		StateMissing: "missing", StateRunning: "running", StateShutoff: "shut off",
		StatePaused: "paused", StateOther: "other",
	} {
		if s.String() != want {
			t.Errorf("%d.String() = %q, want %q", s, s.String(), want)
		}
	}
}

func TestState(t *testing.T) {
	c, _ := newFake(map[string]*fakeDomain{
		"brig-a": running(0),
		"brig-c": {state: golibvirt.DomainShutoff},
		"brig-d": {state: golibvirt.DomainShutoff, saved: true},
		"brig-e": {state: golibvirt.DomainPaused},
	})
	for name, want := range map[string]State{
		"brig-a": StateRunning, "brig-b": StateMissing, "brig-c": StateShutoff,
		"brig-d": StatePaused, "brig-e": StatePaused,
	} {
		if got, err := c.State(name); err != nil || got != want {
			t.Errorf("State(%s) = %s, %v; want %s", name, got, err, want)
		}
	}
}

func TestDefine(t *testing.T) {
	x, err := DomainXML(testSpec())
	if err != nil {
		t.Fatal(err)
	}
	define := fmt.Sprintf("define brig-dev %d", golibvirt.DomainDefineValidate)

	t.Run("new", func(t *testing.T) {
		c, f := newFake(map[string]*fakeDomain{})
		if err := c.Define(x); err != nil {
			t.Fatal(err)
		}
		checkCalls(t, f, define)
	})
	t.Run("redefine", func(t *testing.T) {
		dom := running(0)
		dom.xml = x
		c, f := newFake(map[string]*fakeDomain{"brig-dev": dom})
		if err := c.Define(x); err != nil {
			t.Fatal(err)
		}
		checkCalls(t, f, define)
	})
	t.Run("snapshot", func(t *testing.T) {
		spec := testSpec()
		spec.DataDisk += ".snap"
		moved, err := DomainXML(spec)
		if err != nil {
			t.Fatal(err)
		}
		dom := running(0)
		dom.xml = moved
		c, f := newFake(map[string]*fakeDomain{"brig-dev": dom})
		if err := c.Define(x); err == nil || !strings.Contains(err.Error(), "disk vdb is "+spec.DataDisk) {
			t.Errorf("Define() = %v, want refusal", err)
		}
		checkCalls(t, f)
	})
	t.Run("foreign", func(t *testing.T) {
		c, f := newFake(map[string]*fakeDomain{"brig-dev": {state: golibvirt.DomainShutoff}})
		if err := c.Define(x); err == nil || !strings.Contains(err.Error(), "not created by brig") {
			t.Errorf("Define() = %v, want refusal", err)
		}
		checkCalls(t, f)
	})
	t.Run("garbage", func(t *testing.T) {
		c, _ := newFake(map[string]*fakeDomain{})
		if err := c.Define("<domain"); err == nil {
			t.Error("Define() accepted malformed XML")
		}
	})
}

func TestStart(t *testing.T) {
	c, f := newFake(map[string]*fakeDomain{
		"brig-a": {state: golibvirt.DomainShutoff},
		"brig-p": {state: golibvirt.DomainPaused},
		"brig-s": {state: golibvirt.DomainShutoff, saved: true},
	})
	for _, name := range []string{"brig-a", "brig-p", "brig-s"} {
		if err := c.Start(name); err != nil {
			t.Fatal(err)
		}
		if st, _ := c.State(name); st != StateRunning {
			t.Errorf("%s is %s after Start", name, st)
		}
	}
	if err := c.Start("brig-a"); err == nil {
		t.Error("starting a running domain succeeded")
	}
	if err := c.Start("brig-b"); !golibvirt.IsNotFound(err) {
		t.Errorf("Start(missing) = %v, want not found", err)
	}
	checkCalls(t, f, "create brig-a 0", "resume brig-p", "create brig-s 0", "create brig-a 0")
}

func TestShutdown(t *testing.T) {
	request := fmt.Sprintf("shutdown brig-a %d", golibvirt.DomainShutdownGuestAgent|golibvirt.DomainShutdownAcpiPowerBtn)
	destroy := "destroy brig-a 0"

	t.Run("graceful", func(t *testing.T) {
		c, f := newFake(map[string]*fakeDomain{"brig-a": running(2)})
		if err := c.Shutdown(context.Background(), "brig-a", time.Minute); err != nil {
			t.Fatal(err)
		}
		checkCalls(t, f, request)
	})
	t.Run("timeout", func(t *testing.T) {
		c, f := newFake(map[string]*fakeDomain{"brig-a": running(-1)})
		if err := c.Shutdown(context.Background(), "brig-a", 30*time.Millisecond); err != nil {
			t.Fatal(err)
		}
		calls := f.recorded()
		if n := strings.Count(strings.Join(calls, "\n"), request); n < 2 {
			t.Errorf("shutdown requested %d times, want repeats: %q", n, calls)
		}
		if !slices.Contains(calls, destroy) || f.domain("brig-a").state != golibvirt.DomainShutoff {
			t.Errorf("not destroyed after timeout: %q", calls)
		}
	})
	t.Run("canceled", func(t *testing.T) {
		c, f := newFake(map[string]*fakeDomain{"brig-a": running(-1)})
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
		defer cancel()
		if err := c.Shutdown(ctx, "brig-a", time.Minute); !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("Shutdown() = %v, want deadline exceeded", err)
		}
		if calls := f.recorded(); slices.Contains(calls, destroy) {
			t.Errorf("destroyed after cancellation: %q", calls)
		}
	})
	t.Run("request fails", func(t *testing.T) {
		c, f := newFake(map[string]*fakeDomain{"brig-a": running(-1)})
		f.shutdownErr = libvirtErr(golibvirt.ErrOperationFailed, "cannot acquire state change lock")
		err := c.Shutdown(context.Background(), "brig-a", time.Minute)
		if err == nil || !strings.Contains(err.Error(), "state change lock") {
			t.Errorf("Shutdown() = %v, want the request's error", err)
		}
		checkCalls(t, f, request)
	})
	// Through the guest agent, libvirt answers only once the guest is off
	// or after a minute; meanwhile the state must still be polled and the
	// timeout and ctx must still apply.
	blocking := func(t *testing.T, offAfter int) (*Conn, *fakeClient) {
		t.Helper()
		c, f := newFake(map[string]*fakeDomain{"brig-a": running(offAfter)})
		f.shutdownBlock = make(chan struct{})
		t.Cleanup(func() { close(f.shutdownBlock) })
		return c, f
	}
	t.Run("blocking request, guest stops", func(t *testing.T) {
		c, f := blocking(t, 3)
		if err := c.Shutdown(context.Background(), "brig-a", time.Minute); err != nil {
			t.Fatal(err)
		}
		checkCalls(t, f, request)
	})
	t.Run("blocking request, timeout", func(t *testing.T) {
		c, f := blocking(t, -1)
		start := time.Now()
		if err := c.Shutdown(context.Background(), "brig-a", 30*time.Millisecond); err != nil {
			t.Fatal(err)
		}
		if d := time.Since(start); d > 5*time.Second {
			t.Errorf("Shutdown took %v despite a 30ms timeout", d)
		}
		// One request at a time: the first one never returns.
		checkCalls(t, f, request, destroy)
	})
	t.Run("blocking request, canceled", func(t *testing.T) {
		c, f := blocking(t, -1)
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
		defer cancel()
		if err := c.Shutdown(ctx, "brig-a", time.Minute); !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("Shutdown() = %v, want deadline exceeded", err)
		}
		checkCalls(t, f, request)
	})
	t.Run("paused", func(t *testing.T) {
		c, f := newFake(map[string]*fakeDomain{"brig-a": {state: golibvirt.DomainPaused, owned: true}})
		if err := c.Shutdown(context.Background(), "brig-a", time.Minute); err != nil {
			t.Fatal(err)
		}
		checkCalls(t, f, "resume brig-a", request)
	})
	t.Run("saved", func(t *testing.T) {
		c, f := newFake(map[string]*fakeDomain{"brig-a": {state: golibvirt.DomainShutoff, saved: true}})
		if err := c.Shutdown(context.Background(), "brig-a", time.Minute); err != nil {
			t.Fatal(err)
		}
		checkCalls(t, f, "create brig-a 0", request)
		if d := f.domain("brig-a"); d.saved || d.state != golibvirt.DomainShutoff {
			t.Errorf("domain after Shutdown = %+v", d)
		}
	})
	t.Run("saved while paused", func(t *testing.T) {
		c, f := newFake(map[string]*fakeDomain{"brig-a": {state: golibvirt.DomainShutoff, saved: true, restoresPaused: true}})
		if err := c.Shutdown(context.Background(), "brig-a", time.Minute); err != nil {
			t.Fatal(err)
		}
		checkCalls(t, f, "create brig-a 0", "resume brig-a", request)
	})
	t.Run("saved, cannot restore", func(t *testing.T) {
		c, f := newFake(map[string]*fakeDomain{"brig-a": {state: golibvirt.DomainShutoff, saved: true, unrestorable: true}})
		if err := c.Shutdown(context.Background(), "brig-a", time.Minute); err != nil {
			t.Fatal(err)
		}
		checkCalls(t, f, "create brig-a 0", destroy, "managedsave-remove brig-a")
		if st, _ := c.State("brig-a"); st != StateShutoff {
			t.Errorf("domain is %s after Shutdown", st)
		}
	})
	t.Run("crashed", func(t *testing.T) {
		c, f := newFake(map[string]*fakeDomain{"brig-a": {state: golibvirt.DomainCrashed}})
		if err := c.Shutdown(context.Background(), "brig-a", time.Minute); err != nil {
			t.Fatal(err)
		}
		checkCalls(t, f, destroy)
	})
	t.Run("off or missing", func(t *testing.T) {
		c, f := newFake(map[string]*fakeDomain{"brig-a": {state: golibvirt.DomainShutoff, owned: true}})
		for _, name := range []string{"brig-a", "brig-b"} {
			if err := c.Shutdown(context.Background(), name, time.Minute); err != nil {
				t.Errorf("Shutdown(%s) = %v", name, err)
			}
		}
		checkCalls(t, f)
	})
}

func TestDestroy(t *testing.T) {
	c, f := newFake(map[string]*fakeDomain{
		"brig-a": running(-1),
		"brig-s": {state: golibvirt.DomainShutoff, saved: true},
	})
	for _, name := range []string{"brig-a", "brig-a", "brig-b", "brig-s"} {
		if err := c.Destroy(name); err != nil {
			t.Errorf("Destroy(%s) = %v", name, err)
		}
	}
	for _, name := range []string{"brig-a", "brig-s"} {
		if st, _ := c.State(name); st != StateShutoff {
			t.Errorf("%s is %s after Destroy", name, st)
		}
	}
	checkCalls(t, f, "destroy brig-a 0", "destroy brig-a 0", "destroy brig-s 0", "managedsave-remove brig-s")
}

func TestUndefine(t *testing.T) {
	flags := golibvirt.DomainUndefineNvram | golibvirt.DomainUndefineManagedSave |
		golibvirt.DomainUndefineSnapshotsMetadata | golibvirt.DomainUndefineCheckpointsMetadata

	c, f := newFake(map[string]*fakeDomain{
		"brig-a": running(-1),
		"brig-x": {state: golibvirt.DomainShutoff},
	})
	if err := c.Undefine("brig-a"); err != nil {
		t.Fatal(err)
	}
	if _, ok := f.doms["brig-a"]; ok {
		t.Error("domain still defined")
	}
	if err := c.Undefine("brig-a"); err != nil {
		t.Errorf("Undefine(missing) = %v", err)
	}
	if err := c.Undefine("brig-x"); err == nil || !strings.Contains(err.Error(), "not created by brig") {
		t.Errorf("Undefine(foreign) = %v, want refusal", err)
	}
	checkCalls(t, f, "destroy brig-a 0", fmt.Sprintf("undefine brig-a %d", flags))
}

func TestBlockResize(t *testing.T) {
	a := running(-1)
	a.disks = map[string]uint64{"vdb": uint64(20 * bytesize.GiB)}
	c, f := newFake(map[string]*fakeDomain{"brig-a": a})
	if err := c.BlockResize("brig-a", "vdb", 40*bytesize.GiB); err != nil {
		t.Fatal(err)
	}
	if err := c.BlockResize("brig-a", "vdb", 30*bytesize.GiB); err == nil || !strings.Contains(err.Error(), "refusing to shrink it from 40GiB to 30GiB") {
		t.Errorf("shrinking = %v, want refusal", err)
	}
	if err := c.BlockResize("brig-a", "vdc", 40*bytesize.GiB); err == nil {
		t.Error("resizing a missing disk succeeded")
	}
	if err := c.BlockResize("brig-b", "vdb", bytesize.GiB); err == nil {
		t.Error("resizing a missing domain succeeded")
	}
	// Sizes are rounded up to whole 512-byte sectors.
	if err := c.BlockResize("brig-a", "vdb", 40*bytesize.GiB+1); err != nil {
		t.Fatal(err)
	}
	checkCalls(t, f,
		fmt.Sprintf("resize brig-a vdb %d %d", 40*bytesize.GiB, golibvirt.DomainBlockResizeBytes),
		fmt.Sprintf("resize brig-a vdb %d %d", 40*bytesize.GiB+512, golibvirt.DomainBlockResizeBytes))
}

func TestClose(t *testing.T) {
	c, f := newFake(nil)
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	checkCalls(t, f, "disconnect")
}

func TestIsCode(t *testing.T) {
	err := fmt.Errorf("context: %w", libvirtErr(golibvirt.ErrOperationInvalid, "x"))
	if !isCode(err, golibvirt.ErrOperationInvalid) || isCode(err, golibvirt.ErrNoDomain) || isCode(errors.New("x"), golibvirt.ErrNoDomain) {
		t.Error("isCode misclassifies errors")
	}
}

func TestShutdownProgress(t *testing.T) {
	for _, tc := range []struct {
		name     string
		offAfter int
		timeout  time.Duration
		want     string
	}{
		{"graceful", 2, time.Minute, "wait wait for the guest to power off: ok\n"},
		// The domain is destroyed once the wait is over; that is no failure.
		{"timeout", -1, 30 * time.Millisecond, "wait wait for the guest to power off: ok\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, _ := newFake(map[string]*fakeDomain{"brig-a": running(tc.offAfter)})
			ctx, w := progresstest.Watch(t.Context(), t)
			if err := c.Shutdown(ctx, "brig-a", tc.timeout); err != nil {
				t.Fatal(err)
			}
			if got := w.Finish(); got != tc.want {
				t.Errorf("progress\n%s\nwant\n%s", got, tc.want)
			}
		})
	}
	t.Run("canceled", func(t *testing.T) {
		c, _ := newFake(map[string]*fakeDomain{"brig-a": running(-1)})
		ctx, w := progresstest.Watch(t.Context(), t)
		ctx, cancel := context.WithTimeout(ctx, 10*time.Millisecond)
		defer cancel()
		if err := c.Shutdown(ctx, "brig-a", time.Minute); err == nil {
			t.Fatal("Shutdown succeeded")
		}
		t.Log(w.Tree())
		if got, want := w.Finish(), "wait wait for the guest to power off: failed (timeout)"; !strings.HasPrefix(got, want) {
			t.Errorf("progress\n%s\nwant it to start with %q", got, want)
		}
	})
}
