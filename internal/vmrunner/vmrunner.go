// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

// Package vmrunner is the controller of brig's vm runner: a broker on the
// host that lets agents in sandboxes start throwaway VMs, spike VMs, and
// drive them through a small set of MCP tools.
//
// Nested virtualization stays off in brig's VMs, so spike VMs are siblings
// of the gateways, made by brig from images the user allows. An agent never
// reaches a spike VM over a network: it sees its screen through
// screenshots, types and clicks through QEMU's input devices, and runs
// commands and moves files through the QEMU guest agent, all through this
// controller. The controller is thus a control channel from agents into the
// host. It offers no shell and no libvirt XML, validates every argument and
// bounds every resource:
//
//   - The token that OpenShell adds to a sandbox's calls names the sandbox.
//     Each VM belongs to the sandbox that created it, and other sandboxes
//     cannot see it.
//   - Quotas bound each sandbox's and the host's VMs, CPUs, memory and disk.
//   - A reaper deletes VMs whose lifetime ended and those of sandboxes that
//     lost their token, on detach or when the sandbox was deleted.
package vmrunner

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sync"
	"time"

	"github.com/dennisklein/brig/internal/bytesize"
	"github.com/dennisklein/brig/internal/config"
	"github.com/dennisklein/brig/internal/libvirt"
	"github.com/dennisklein/brig/internal/paths"
	"github.com/dennisklein/brig/internal/qemuimg"
	"github.com/dennisklein/brig/internal/vm"
)

// ServiceType is the service type whose services this package runs.
const ServiceType = "vm"

// Hypervisor is what the controller needs of a libvirt connection;
// *libvirt.Conn has it.
type Hypervisor interface {
	Define(domainXML string) error
	Start(name string) error
	State(name string) (libvirt.State, error)
	Undefine(name string) error
	Screenshot(name string) ([]byte, string, error)
	SendKeys(name string, codes []uint32, hold time.Duration) error
	MonitorCommand(name, cmd string) (string, error)
	AgentCommand(name, cmd string, timeout time.Duration) (string, error)
	CreateSnapshot(name, snap string) error
	RevertSnapshot(name, snap string) error
	DeleteSnapshot(name, snap string) error
	Close() error
}

// Record is the controller's record of a spike VM, kept as vm.json in the
// VM's directory. The record is written before the domain is defined and
// removed after it is undefined, so a restart finds every VM.
type Record struct {
	ID        string        `json:"id"`
	UUID      string        `json:"uuid"`
	Owner     Owner         `json:"owner"`
	Image     string        `json:"image"`
	CPUs      int           `json:"cpus"`
	Memory    bytesize.Size `json:"memory"`
	Disk      bytesize.Size `json:"disk"`
	CreatedAt time.Time     `json:"created_at"`
	ExpiresAt time.Time     `json:"expires_at"`
	Snapshots []string      `json:"snapshots,omitempty"`
}

// DomainName is the libvirt domain of the VM. Gateway names have no dot, so
// it cannot be a gateway's domain.
func (r *Record) DomainName() string { return DomainName(r.ID) }

// DomainName is the libvirt domain of the spike VM with the given ID.
func DomainName(id string) string { return "brig-spike." + id }

var idRE = regexp.MustCompile(`^[0-9a-f]{8}$`)

// Options configure a Controller.
type Options struct {
	// Dir holds the controller's VMs, one directory each below Dir/vms.
	Dir string
	// Config is the vm_runner section of config.yaml.
	Config config.VMRunner
	// Tokens maps the callers' tokens to their sandboxes.
	Tokens *Tokens
	// Connect opens a connection to the libvirt session daemon. Each
	// operation opens its own, so a restarted daemon costs nothing.
	Connect func(ctx context.Context) (Hypervisor, error)
	// Log receives a line per change and per failure.
	Log *slog.Logger
	// BootTimeout bounds the wait for a new VM's guest agent.
	BootTimeout time.Duration
}

// Controller manages the spike VMs.
type Controller struct {
	dir     string
	cfg     config.VMRunner
	tokens  *Tokens
	connect func(ctx context.Context) (Hypervisor, error)
	log     *slog.Logger
	boot    time.Duration

	// Tests replace these.
	now     func() time.Time
	overlay func(ctx context.Context, path, base string, size bytesize.Size) error
	inspect func(ctx context.Context, path string) (qemuimg.Info, error)
	poll    time.Duration

	mu  sync.Mutex
	vms map[string]*entry
	// busy counts each owner's calls in progress.
	busy map[Owner]int
}

// entry is a VM the controller knows. Operations on the VM hold op shared;
// snapshots, restores and the destroy hold it exclusively. ctx ends when
// the VM is being destroyed, which cancels the operations in progress.
type entry struct {
	rec    Record
	op     sync.RWMutex
	ctx    context.Context
	cancel context.CancelFunc
	// ready is false while the VM is being created.
	ready bool

	mu sync.Mutex
	// width and height are the display's size at the last screenshot.
	width, height int
}

func (e *entry) screen() (int, int) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.width, e.height
}

func (e *entry) setScreen(w, h int) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.width, e.height = w, h
}

// New returns a controller and loads the records of its VMs.
func New(o Options) (*Controller, error) {
	c := &Controller{
		dir: o.Dir, cfg: o.Config, tokens: o.Tokens, connect: o.Connect, log: o.Log, boot: o.BootTimeout,
		now: time.Now, overlay: qemuimg.CreateOverlay, inspect: qemuimg.Inspect, poll: time.Second,
		vms: map[string]*entry{}, busy: map[Owner]int{},
	}
	if c.log == nil {
		c.log = slog.New(slog.DiscardHandler)
	}
	if c.boot == 0 {
		c.boot = 3 * time.Minute
	}
	if err := paths.EnsurePrivate(c.vmsDir()); err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(c.vmsDir())
	if err != nil {
		return nil, err
	}
	for _, d := range entries {
		if !d.IsDir() || !idRE.MatchString(d.Name()) {
			continue
		}
		rec, err := c.loadRecord(d.Name())
		if errors.Is(err, fs.ErrNotExist) {
			// A crash between mkdir and saving the record: nothing
			// was defined yet.
			_ = os.RemoveAll(c.vmDir(d.Name()))
			continue
		}
		if err != nil {
			c.log.Error("skip VM with unreadable record", "id", d.Name(), "err", err)
			continue
		}
		ctx, cancel := context.WithCancel(context.Background())
		c.vms[rec.ID] = &entry{rec: *rec, ctx: ctx, cancel: cancel, ready: true}
	}
	return c, nil
}

func (c *Controller) vmsDir() string         { return filepath.Join(c.dir, "vms") }
func (c *Controller) vmDir(id string) string { return filepath.Join(c.vmsDir(), id) }

func (c *Controller) diskPath(id string) string { return filepath.Join(c.vmDir(id), "disk.qcow2") }

func (c *Controller) loadRecord(id string) (*Record, error) {
	data, err := os.ReadFile(filepath.Join(c.vmDir(id), "vm.json"))
	if err != nil {
		return nil, err
	}
	var r Record
	if err := json.Unmarshal(data, &r); err != nil {
		return nil, err
	}
	if r.ID != id {
		return nil, fmt.Errorf("record is for VM %q", r.ID)
	}
	return &r, nil
}

func (c *Controller) saveRecord(r *Record) error {
	if err := paths.EnsurePrivate(c.vmDir(r.ID)); err != nil {
		return err
	}
	data, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	return vm.WriteFileAtomic(filepath.Join(c.vmDir(r.ID), "vm.json"), append(data, '\n'), 0o600)
}

// maxBusy bounds the calls in progress per owner, so one sandbox cannot
// tie up the controller.
const maxBusy = 4

// enter counts a call of owner o; the returned function ends it.
func (c *Controller) enter(o Owner) (func(), error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.busy[o] >= maxBusy {
		return nil, fmt.Errorf("%d calls of this sandbox are still running; wait for them", maxBusy)
	}
	c.busy[o]++
	return func() {
		c.mu.Lock()
		defer c.mu.Unlock()
		if c.busy[o]--; c.busy[o] == 0 {
			delete(c.busy, o)
		}
	}, nil
}

// errNoVM hides whether a VM exists from sandboxes that do not own it.
var errNoVM = errors.New("no such VM; vm_list shows yours")

// get returns owner o's ready VM with the given ID.
func (c *Controller) get(o Owner, id string) (*entry, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.vms[id]
	if !ok || e.rec.Owner != o || !e.ready {
		return nil, errNoVM
	}
	return e, nil
}

// List returns the records of all VMs, sorted by creation, for brig's own
// commands and brig ui.
func (c *Controller) List() []Record {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]Record, 0, len(c.vms))
	for _, e := range c.vms {
		out = append(out, e.rec)
	}
	slices.SortFunc(out, func(a, b Record) int { return a.CreatedAt.Compare(b.CreatedAt) })
	return out
}

func (c *Controller) owned(o Owner) []*entry {
	var out []*entry
	for _, e := range c.vms {
		if e.rec.Owner == o {
			out = append(out, e)
		}
	}
	return out
}

// CreateRequest is what vm_create asks for.
type CreateRequest struct {
	Image    string
	CPUs     int
	Memory   bytesize.Size
	Disk     bytesize.Size
	Lifetime time.Duration
}

// Defaults of a new VM within the quota.
const (
	defaultCPUs   = 2
	defaultMemory = 4 * bytesize.GiB
)

// reserve checks req against the quotas and adds a VM that is not ready yet,
// which counts against them until it is ready or removed.
func (c *Controller) reserve(o Owner, req CreateRequest) (*entry, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	q, h := c.cfg.Quota, c.cfg.HostQuota
	var cpus int
	var mem, disk, hostMem bytesize.Size
	for _, e := range c.vms {
		hostMem += e.rec.Memory
	}
	mine := c.owned(o)
	for _, e := range mine {
		cpus += e.rec.CPUs
		mem += e.rec.Memory
		disk += e.rec.Disk
	}
	switch {
	case len(mine) >= q.VMs:
		return nil, fmt.Errorf("quota: this sandbox may have %d VMs at once; vm_destroy one first", q.VMs)
	case cpus+req.CPUs > q.CPUs:
		return nil, fmt.Errorf("quota: %d CPUs asked, %d of %d left for this sandbox", req.CPUs, q.CPUs-cpus, q.CPUs)
	case mem+req.Memory > q.Memory:
		return nil, fmt.Errorf("quota: %s of memory asked, %s of %s left for this sandbox", req.Memory, q.Memory-mem, q.Memory)
	case disk+req.Disk > q.Disk:
		return nil, fmt.Errorf("quota: %s of disk asked, %s of %s left for this sandbox", req.Disk, q.Disk-disk, q.Disk)
	case len(c.vms) >= h.VMs:
		return nil, fmt.Errorf("quota: the host runs its limit of %d VMs; try again later", h.VMs)
	case hostMem+req.Memory > h.Memory:
		return nil, fmt.Errorf("quota: the host has %s of memory left for VMs, %s asked", h.Memory-hostMem, req.Memory)
	}
	id, uuid, err := newIDs()
	if err != nil {
		return nil, err
	}
	for c.vms[id] != nil {
		if id, uuid, err = newIDs(); err != nil {
			return nil, err
		}
	}
	now := c.now().UTC().Truncate(time.Second)
	ctx, cancel := context.WithCancel(context.Background())
	e := &entry{
		rec: Record{
			ID: id, UUID: uuid, Owner: o, Image: req.Image, CPUs: req.CPUs, Memory: req.Memory, Disk: req.Disk,
			CreatedAt: now, ExpiresAt: now.Add(req.Lifetime),
		},
		ctx: ctx, cancel: cancel,
	}
	c.vms[id] = e
	return e, nil
}

func newIDs() (string, string, error) {
	var b [20]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", "", err
	}
	u := b[4:]
	u[6] = u[6]&0x0f | 0x40 // version 4
	u[8] = u[8]&0x3f | 0x80 // RFC 4122 variant
	uuid := fmt.Sprintf("%x-%x-%x-%x-%x", u[0:4], u[4:6], u[6:8], u[8:10], u[10:16])
	return hex.EncodeToString(b[:4]), uuid, nil
}

// CreateResult says what Create made.
type CreateResult struct {
	Record
	// AgentError is set when the guest agent did not answer in time: the
	// VM runs, but exec and file copies do not work.
	AgentError string
}

// Create creates and starts a VM for owner o and waits for its guest agent.
func (c *Controller) Create(ctx context.Context, o Owner, req CreateRequest) (*CreateResult, error) {
	img, ok := c.cfg.Images[req.Image]
	if !ok {
		return nil, fmt.Errorf("unknown image %q; vm_images lists the images you may use", req.Image)
	}
	q := c.cfg.Quota
	if req.CPUs == 0 {
		req.CPUs = min(defaultCPUs, q.CPUs)
	}
	if req.Memory == 0 {
		req.Memory = min(defaultMemory, q.Memory)
	}
	if req.Lifetime == 0 || req.Lifetime > time.Duration(q.Lifetime) {
		req.Lifetime = time.Duration(q.Lifetime)
	}
	switch {
	case req.CPUs < 1:
		return nil, errors.New("cpus must be at least 1")
	case req.Memory < 256*bytesize.MiB || req.Memory%bytesize.MiB != 0:
		return nil, errors.New("memory must be a whole number of MiB, at least 256 MiB")
	case req.Lifetime < time.Minute:
		return nil, errors.New("lifetime must be at least a minute")
	}
	info, err := c.inspect(ctx, img.File)
	if err != nil {
		c.log.Error("inspect image", "image", req.Image, "err", err)
		return nil, fmt.Errorf("image %q cannot be read; ask the user to check it", req.Image)
	}
	if info.Format != "qcow2" || info.BackingFile != "" {
		c.log.Error("image is not a self-contained qcow2 file", "image", req.Image, "format", info.Format, "backing", info.BackingFile)
		return nil, fmt.Errorf("image %q is not a self-contained qcow2 image; ask the user to convert it", req.Image)
	}
	if req.Disk == 0 {
		req.Disk = info.VirtualSize
	}
	if req.Disk < info.VirtualSize {
		return nil, fmt.Errorf("disk must be at least the image's size, %s", info.VirtualSize)
	}
	e, err := c.reserve(o, req)
	if err != nil {
		return nil, err
	}
	res, err := c.create(ctx, e, img.File)
	if err != nil {
		c.log.Error("create VM", "owner", o.String(), "id", e.rec.ID, "err", err)
		if cerr := c.remove(context.WithoutCancel(ctx), e); cerr != nil {
			// Show it, so the reaper and vm_destroy can retry.
			c.log.Error("clean up VM", "id", e.rec.ID, "err", cerr)
			c.mu.Lock()
			e.ready = true
			c.mu.Unlock()
		}
		return nil, err
	}
	c.log.Info("created VM", "owner", o.String(), "id", e.rec.ID, "image", req.Image,
		"cpus", req.CPUs, "memory", req.Memory.String(), "disk", req.Disk.String())
	return res, nil
}

func (c *Controller) create(ctx context.Context, e *entry, base string) (*CreateResult, error) {
	r := &e.rec
	if err := c.saveRecord(r); err != nil {
		return nil, err
	}
	if err := c.overlay(ctx, c.diskPath(r.ID), base, r.Disk); err != nil {
		return nil, fmt.Errorf("create disk: %w", err)
	}
	x, err := libvirt.SpikeDomainXML(libvirt.SpikeSpec{
		Name: r.DomainName(), UUID: r.UUID, CPUs: r.CPUs, Memory: r.Memory,
		Disk: c.diskPath(r.ID), BaseImage: base, Owner: r.Owner.String(),
	})
	if err != nil {
		return nil, err
	}
	hv, err := c.connect(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = hv.Close() }()
	if err := hv.Define(x); err != nil {
		return nil, err
	}
	if err := hv.Start(r.DomainName()); err != nil {
		return nil, err
	}
	c.mu.Lock()
	e.ready = true
	c.mu.Unlock()
	res := &CreateResult{Record: *r}
	bctx, cancel := context.WithTimeout(ctx, c.boot)
	defer cancel()
	if err := c.waitAgent(bctx, hv, r.DomainName()); err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		res.AgentError = err.Error()
	}
	return res, nil
}

// remove undefines the VM's domain, deletes its files and forgets it. It
// cancels the VM's operations and waits for them first.
func (c *Controller) remove(ctx context.Context, e *entry) error {
	e.cancel()
	e.op.Lock()
	defer e.op.Unlock()
	hv, err := c.connect(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = hv.Close() }()
	if err := hv.Undefine(e.rec.DomainName()); err != nil {
		return err
	}
	if err := os.RemoveAll(c.vmDir(e.rec.ID)); err != nil {
		return err
	}
	c.mu.Lock()
	delete(c.vms, e.rec.ID)
	c.mu.Unlock()
	return nil
}

// Destroy deletes owner o's VM.
func (c *Controller) Destroy(ctx context.Context, o Owner, id string) error {
	e, err := c.get(o, id)
	if err != nil {
		return err
	}
	if err := c.remove(ctx, e); err != nil {
		return err
	}
	c.log.Info("destroyed VM", "owner", o.String(), "id", id, "reason", "asked")
	return nil
}

// DestroyAny deletes any VM, for the user's own commands.
func (c *Controller) DestroyAny(ctx context.Context, id string) error {
	c.mu.Lock()
	e, ok := c.vms[id]
	c.mu.Unlock()
	if !ok {
		return fmt.Errorf("no VM %s", id)
	}
	if err := c.remove(ctx, e); err != nil {
		return err
	}
	c.log.Info("destroyed VM", "owner", e.rec.Owner.String(), "id", id, "reason", "user")
	return nil
}

// Reap deletes the VMs whose lifetime ended and those whose owner has no
// token any more. A token file that cannot be read deletes nothing.
func (c *Controller) Reap(ctx context.Context) {
	owners, err := c.tokens.Owners()
	if err != nil {
		c.log.Error("read tokens", "err", err)
		owners = nil
	}
	now := c.now()
	c.mu.Lock()
	var doomed []*entry
	var reasons []string
	for _, e := range c.vms {
		if !e.ready {
			continue
		}
		switch {
		case !now.Before(e.rec.ExpiresAt):
			doomed, reasons = append(doomed, e), append(reasons, "lifetime ended")
		case owners != nil && !owners[e.rec.Owner]:
			doomed, reasons = append(doomed, e), append(reasons, "sandbox detached")
		}
	}
	c.mu.Unlock()
	for i, e := range doomed {
		if err := c.remove(ctx, e); err != nil {
			c.log.Error("destroy VM", "id", e.rec.ID, "reason", reasons[i], "err", err)
			continue
		}
		c.log.Info("destroyed VM", "owner", e.rec.Owner.String(), "id", e.rec.ID, "reason", reasons[i])
	}
}

// RunReaper reaps every interval until ctx ends.
func (c *Controller) RunReaper(ctx context.Context, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		c.Reap(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}
