// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

package vmrunner

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dennisklein/brig/internal/bytesize"
	"github.com/dennisklein/brig/internal/config"
	"github.com/dennisklein/brig/internal/libvirt"
	"github.com/dennisklein/brig/internal/qemuimg"
	"github.com/dennisklein/brig/internal/service"
)

// fakeHV is a libvirt session daemon with spike VMs whose guest agents
// keep files in memory.
type fakeHV struct {
	mu      sync.Mutex
	domains map[string]*fakeDom
	calls   []string
	// noAgent makes guest agents of new domains never answer.
	noAgent bool
	// failStart fails starting domains.
	failStart error
	// screen is the frame of every display.
	screen []byte
}

type fakeDom struct {
	xml     string
	running bool
	agent   bool
	files   map[string][]byte
	handles map[int]string
	snaps   map[string]map[string][]byte
}

func ppm(w, h int) []byte {
	b := fmt.Appendf(nil, "P6\n%d %d\n255\n", w, h)
	return append(b, bytes.Repeat([]byte{0x10, 0x20, 0x30}, w*h)...)
}

func newHV() *fakeHV { return &fakeHV{domains: map[string]*fakeDom{}, screen: ppm(64, 48)} }

type hvConn struct{ *fakeHV }

func (h hvConn) Close() error { return nil }

func (h *fakeHV) record(format string, args ...any) {
	h.calls = append(h.calls, fmt.Sprintf(format, args...))
}

func (h *fakeHV) recorded() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return slices.Clone(h.calls)
}

func (h *fakeHV) dom(name string) (*fakeDom, error) {
	d, ok := h.domains[name]
	if !ok {
		return nil, fmt.Errorf("domain %s not found", name)
	}
	return d, nil
}

func (h *fakeHV) Define(x string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	name := x[strings.Index(x, "<name>")+6 : strings.Index(x, "</name>")]
	h.record("define %s", name)
	h.domains[name] = &fakeDom{xml: x, agent: !h.noAgent, files: map[string][]byte{}, handles: map[int]string{}, snaps: map[string]map[string][]byte{}}
	return nil
}

func (h *fakeHV) Start(name string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.record("start %s", name)
	if h.failStart != nil {
		return h.failStart
	}
	d, err := h.dom(name)
	if err == nil {
		d.running = true
	}
	return err
}

func (h *fakeHV) State(name string) (libvirt.State, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	d, ok := h.domains[name]
	switch {
	case !ok:
		return libvirt.StateMissing, nil
	case d.running:
		return libvirt.StateRunning, nil
	}
	return libvirt.StateShutoff, nil
}

func (h *fakeHV) Undefine(name string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.record("undefine %s", name)
	delete(h.domains, name)
	return nil
}

func (h *fakeHV) Screenshot(name string) ([]byte, string, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if _, err := h.dom(name); err != nil {
		return nil, "", err
	}
	return h.screen, "image/x-portable-pixmap", nil
}

func (h *fakeHV) SendKeys(name string, codes []uint32, _ time.Duration) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.record("keys %v", codes)
	_, err := h.dom(name)
	return err
}

func (h *fakeHV) MonitorCommand(name, cmd string) (string, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.record("qmp %s", cmd)
	if _, err := h.dom(name); err != nil {
		return "", err
	}
	return `{"return":{},"id":"libvirt-1"}`, nil
}

func (h *fakeHV) AgentCommand(name, cmd string, _ time.Duration) (string, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	d, err := h.dom(name)
	if err != nil {
		return "", err
	}
	if !d.agent {
		return "", errors.New("Guest agent is not responding")
	}
	var req struct {
		Execute   string         `json:"execute"`
		Arguments map[string]any `json:"arguments"`
	}
	if err := json.Unmarshal([]byte(cmd), &req); err != nil {
		return "", err
	}
	if req.Execute != "guest-ping" {
		h.record("agent %s", req.Execute)
	}
	ret := func(v any) (string, error) {
		b, _ := json.Marshal(map[string]any{"return": v})
		return string(b), nil
	}
	a := req.Arguments
	switch req.Execute {
	case "guest-ping":
		return ret(struct{}{})
	case "guest-exec":
		args, _ := a["arg"].([]any)
		out := fmt.Sprint(append([]any{a["path"]}, args...))
		d.files["/exec.out"] = []byte(out)
		return ret(map[string]int{"pid": 7})
	case "guest-exec-status":
		code := 0
		if strings.Contains(string(d.files["/exec.out"]), "false") {
			code = 1
		}
		return ret(map[string]any{"exited": true, "exitcode": code, "out-data": base64.StdEncoding.EncodeToString(d.files["/exec.out"])})
	case "guest-file-open":
		h := len(d.handles) + 1
		d.handles[h] = a["path"].(string)
		if a["mode"] == "w" {
			d.files[d.handles[h]] = nil
		}
		if _, ok := d.files[d.handles[h]]; !ok && a["mode"] == "r" {
			return `{"error":{"class":"GenericError","desc":"No such file"}}`, nil
		}
		return ret(h)
	case "guest-file-write":
		p := d.handles[int(a["handle"].(float64))]
		b, _ := base64.StdEncoding.DecodeString(a["buf-b64"].(string))
		d.files[p] = append(d.files[p], b...)
		return ret(map[string]int{"count": len(b)})
	case "guest-file-read":
		data := d.files[d.handles[int(a["handle"].(float64))]]
		return ret(map[string]any{"count": len(data), "buf-b64": base64.StdEncoding.EncodeToString(data), "eof": true})
	case "guest-file-close":
		delete(d.handles, int(a["handle"].(float64)))
		return ret(struct{}{})
	}
	return "", fmt.Errorf("unexpected agent command %s", req.Execute)
}

func (h *fakeHV) CreateSnapshot(name, snap string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.record("snapshot %s", snap)
	d, err := h.dom(name)
	if err != nil {
		return err
	}
	files := map[string][]byte{}
	for k, v := range d.files {
		files[k] = slices.Clone(v)
	}
	d.snaps[snap] = files
	return nil
}

func (h *fakeHV) RevertSnapshot(name, snap string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.record("revert %s", snap)
	d, err := h.dom(name)
	if err != nil {
		return err
	}
	d.files = d.snaps[snap]
	return nil
}

func (h *fakeHV) DeleteSnapshot(name, snap string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.record("delete-snapshot %s", snap)
	d, err := h.dom(name)
	if err == nil {
		delete(d.snaps, snap)
	}
	return err
}

// testEnv is a controller behind its MCP server, with two sandboxes'
// tokens.
type testEnv struct {
	t     *testing.T
	c     *Controller
	hv    *fakeHV
	srv   *httptest.Server
	dir   string
	clock time.Time
	// overlays are the disks created, by path, with their base.
	overlays map[string]string
}

var (
	alice = Owner{Gateway: "dev", Workspace: "default", Sandbox: "alice"}
	bob   = Owner{Gateway: "dev", Workspace: "default", Sandbox: "bob"}
)

func writeTokens(t *testing.T, path string, tokens map[string]Owner) {
	t.Helper()
	f := service.Tokens{Version: service.TokensVersion}
	for tok, o := range tokens {
		f.Tokens = append(f.Tokens, service.Attachment{
			SHA256: service.TokenHash(tok), Gateway: o.Gateway, Workspace: o.Workspace, Sandbox: o.Sandbox,
			Provider: "brig-svc-vm-" + o.Sandbox, Created: time.Date(2026, 10, 21, 0, 0, 0, 0, time.UTC),
		})
	}
	b, _ := json.Marshal(f)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmp, path); err != nil {
		t.Fatal(err)
	}
}

func testConfig(t *testing.T) config.VMRunner {
	cfg := config.Builtin().VMRunner
	cfg.Images = map[string]config.RunnerImage{"fedora": {File: filepath.Join(t.TempDir(), "fedora.qcow2")}}
	return cfg
}

func newEnv(t *testing.T) *testEnv {
	t.Helper()
	dir := t.TempDir()
	writeTokens(t, filepath.Join(dir, "tokens.json"), map[string]Owner{"tok-alice": alice, "tok-bob": bob})
	env := &testEnv{t: t, hv: newHV(), dir: dir, clock: time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC), overlays: map[string]string{}}
	env.start(testConfig(t))
	return env
}

func (env *testEnv) start(cfg config.VMRunner) {
	t := env.t
	c, err := New(Options{
		Dir: env.dir, Config: cfg, Tokens: NewTokens(filepath.Join(env.dir, "tokens.json")),
		Connect:     func(context.Context) (Hypervisor, error) { return hvConn{env.hv}, nil },
		BootTimeout: 50 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	c.poll = time.Millisecond
	c.now = func() time.Time { return env.clock }
	var mu sync.Mutex
	c.overlay = func(_ context.Context, path, base string, _ bytesize.Size) error {
		mu.Lock()
		defer mu.Unlock()
		env.overlays[path] = base
		return os.WriteFile(path, nil, 0o600)
	}
	c.inspect = func(context.Context, string) (qemuimg.Info, error) {
		return qemuimg.Info{Format: "qcow2", VirtualSize: 5 * bytesize.GiB}, nil
	}
	if env.srv != nil {
		env.srv.Close()
	}
	env.c = c
	env.srv = httptest.NewServer(c.Server(nil))
	t.Cleanup(env.srv.Close)
}

// call calls a tool as the owner of token and returns the result.
func (env *testEnv) call(token, tool string, args any) result {
	env.t.Helper()
	a, _ := json.Marshal(args)
	body := fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":%q,"arguments":%s}}`, tool, a)
	req, _ := http.NewRequest(http.MethodPost, env.srv.URL, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		env.t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return result{status: resp.StatusCode, raw: string(b)}
	}
	var r struct {
		Result struct {
			Content []struct {
				Type, Text, Data, MIMEType string
			} `json:"content"`
			Structured map[string]any `json:"structuredContent"`
			IsError    bool           `json:"isError"`
		} `json:"result"`
		Error *struct{ Message string } `json:"error"`
	}
	if err := json.Unmarshal(b, &r); err != nil {
		env.t.Fatalf("%v: %s", err, b)
	}
	if r.Error != nil {
		env.t.Fatalf("%s: protocol error %s", tool, r.Error.Message)
	}
	res := result{status: resp.StatusCode, raw: string(b), isError: r.Result.IsError, data: r.Result.Structured}
	for _, c := range r.Result.Content {
		switch c.Type {
		case "text":
			res.text = c.Text
		case "image":
			res.image, _ = base64.StdEncoding.DecodeString(c.Data)
		}
	}
	return res
}

type result struct {
	status  int
	raw     string
	text    string
	isError bool
	data    map[string]any
	image   []byte
}

func (env *testEnv) ok(token, tool string, args any) result {
	env.t.Helper()
	r := env.call(token, tool, args)
	if r.status != http.StatusOK || r.isError {
		env.t.Fatalf("%s(%v) failed: %s", tool, args, r.raw)
	}
	return r
}

func (env *testEnv) fails(token, tool string, args any, want string) {
	env.t.Helper()
	r := env.call(token, tool, args)
	if !r.isError || !strings.Contains(r.text, want) {
		env.t.Errorf("%s(%v) = %s, want an error with %q", tool, args, r.raw, want)
	}
}

func (env *testEnv) create(token string, args map[string]any) string {
	env.t.Helper()
	if args == nil {
		args = map[string]any{}
	}
	args["image"] = "fedora"
	return env.ok(token, "vm_create", args).data["id"].(string)
}

func TestCreateListDestroy(t *testing.T) {
	env := newEnv(t)
	r := env.ok("tok-alice", "vm_create", map[string]any{"image": "fedora"})
	id := r.data["id"].(string)
	if !idRE.MatchString(id) || r.data["cpus"] != 2.0 || r.data["memory_mib"] != 4096.0 || r.data["disk_gib"] != 5.0 ||
		r.data["guest_agent"] != "ready" || r.data["expires_at"] != "2026-10-21T16:00:00Z" {
		t.Errorf("vm_create = %s", r.raw)
	}
	dom := env.hv.domains[DomainName(id)]
	if dom == nil || !dom.running || !strings.Contains(dom.xml, `owner="dev/default/alice"`) {
		t.Fatalf("domain = %+v", dom)
	}
	disk := filepath.Join(env.dir, "vms", id, "disk.qcow2")
	if env.overlays[disk] != env.c.cfg.Images["fedora"].File {
		t.Errorf("overlays = %v", env.overlays)
	}

	r = env.ok("tok-alice", "vm_list", nil)
	vms := r.data["vms"].([]any)
	if len(vms) != 1 || vms[0].(map[string]any)["id"] != id || vms[0].(map[string]any)["state"] != "running" {
		t.Errorf("vm_list = %s", r.raw)
	}
	// Bob sees none of Alice's VMs and cannot touch them.
	if r := env.ok("tok-bob", "vm_list", nil); len(r.data["vms"].([]any)) != 0 {
		t.Errorf("bob's vm_list = %s", r.raw)
	}
	for _, tool := range []string{"vm_screenshot", "vm_destroy"} {
		env.fails("tok-bob", tool, map[string]any{"vm": id}, "no such VM")
	}
	env.fails("tok-bob", "vm_exec", map[string]any{"vm": id, "argv": []string{"id"}}, "no such VM")

	env.ok("tok-alice", "vm_destroy", map[string]any{"vm": id})
	if _, err := os.Stat(filepath.Join(env.dir, "vms", id)); !os.IsNotExist(err) {
		t.Errorf("VM directory left: %v", err)
	}
	if env.hv.domains[DomainName(id)] != nil {
		t.Error("domain left defined")
	}
	env.fails("tok-alice", "vm_destroy", map[string]any{"vm": id}, "no such VM")
}

func TestAuth(t *testing.T) {
	env := newEnv(t)
	if r := env.call("tok-mallory", "vm_list", nil); r.status != http.StatusUnauthorized {
		t.Errorf("unknown token: %d %s", r.status, r.raw)
	}
	// Detaching alice removes her token at once.
	writeTokens(t, filepath.Join(env.dir, "tokens.json"), map[string]Owner{"tok-bob": bob})
	if r := env.call("tok-alice", "vm_list", nil); r.status != http.StatusUnauthorized {
		t.Errorf("removed token: %d %s", r.status, r.raw)
	}
	env.ok("tok-bob", "vm_list", nil)
	// A broken token file lets nobody in.
	if err := os.WriteFile(filepath.Join(env.dir, "tokens.json"), []byte("[{]"), 0o600); err != nil {
		t.Fatal(err)
	}
	if r := env.call("tok-bob", "vm_list", nil); r.status != http.StatusUnauthorized {
		t.Errorf("broken token file: %d %s", r.status, r.raw)
	}
}

func TestCreateValidation(t *testing.T) {
	env := newEnv(t)
	env.fails("tok-alice", "vm_create", map[string]any{"image": "windows"}, "unknown image")
	env.fails("tok-alice", "vm_create", map[string]any{"image": "fedora", "memory_mib": 100}, "at least 256 MiB")
	env.fails("tok-alice", "vm_create", map[string]any{"image": "fedora", "disk_gib": 2}, "at least the image's size")
	env.fails("tok-alice", "vm_create", map[string]any{"image": "fedora", "cpus": -1}, "out of range")
	env.fails("tok-alice", "vm_create", map[string]any{"image": "fedora", "xml": "<domain/>"}, "unknown field")
	env.c.inspect = func(context.Context, string) (qemuimg.Info, error) {
		return qemuimg.Info{Format: "qcow2", BackingFile: "/etc/shadow"}, nil
	}
	env.fails("tok-alice", "vm_create", map[string]any{"image": "fedora"}, "not a self-contained qcow2")
	if len(env.hv.domains) != 0 {
		t.Errorf("domains defined: %v", env.hv.domains)
	}
}

func TestQuotas(t *testing.T) {
	env := newEnv(t)
	// Lifetime is capped at the quota's.
	r := env.ok("tok-alice", "vm_create", map[string]any{"image": "fedora", "lifetime_minutes": 100000, "cpus": 3})
	if r.data["expires_at"] != "2026-10-21T16:00:00Z" {
		t.Errorf("expires_at = %v", r.data["expires_at"])
	}
	env.fails("tok-alice", "vm_create", map[string]any{"image": "fedora", "cpus": 2}, "2 CPUs asked, 1 of 4 left")
	env.fails("tok-alice", "vm_create", map[string]any{"image": "fedora", "cpus": 1, "memory_mib": 8192}, "of memory asked")
	env.fails("tok-alice", "vm_create", map[string]any{"image": "fedora", "cpus": 1, "disk_gib": 60}, "of disk asked")
	env.create("tok-alice", map[string]any{"cpus": 1})
	env.fails("tok-alice", "vm_create", map[string]any{"image": "fedora", "cpus": 1}, "may have 2 VMs")
	// Bob has a quota of his own, but the host's memory runs out.
	cfg := env.c.cfg
	cfg.HostQuota.Memory = 11 * bytesize.GiB
	env.c.cfg = cfg
	env.fails("tok-bob", "vm_create", map[string]any{"image": "fedora"}, "the host has 3GiB of memory left")
	cfg.HostQuota.VMs = 2
	env.c.cfg = cfg
	env.fails("tok-bob", "vm_create", map[string]any{"image": "fedora", "memory_mib": 1024}, "limit of 2 VMs")
}

func TestCreateFailureCleansUp(t *testing.T) {
	env := newEnv(t)
	env.hv.failStart = errors.New("no KVM")
	env.fails("tok-alice", "vm_create", map[string]any{"image": "fedora"}, "no KVM")
	if len(env.hv.domains) != 0 || len(env.c.List()) != 0 {
		t.Errorf("left domains %v, VMs %v", env.hv.domains, env.c.List())
	}
	entries, _ := os.ReadDir(filepath.Join(env.dir, "vms"))
	if len(entries) != 0 {
		t.Errorf("left directories %v", entries)
	}
	// The failed VM no longer counts against the quota.
	env.hv.failStart = nil
	env.create("tok-alice", nil)
	env.create("tok-alice", nil)
}

func TestCreateWithoutAgent(t *testing.T) {
	env := newEnv(t)
	env.hv.noAgent = true
	r := env.ok("tok-alice", "vm_create", map[string]any{"image": "fedora"})
	if !strings.Contains(r.data["guest_agent"].(string), "not answering") {
		t.Errorf("vm_create = %s", r.raw)
	}
	id := r.data["id"].(string)
	env.fails("tok-alice", "vm_exec", map[string]any{"vm": id, "argv": []string{"true"}}, "not responding")
	env.ok("tok-alice", "vm_screenshot", map[string]any{"vm": id})
}

func TestScreenAndInput(t *testing.T) {
	env := newEnv(t)
	id := env.create("tok-alice", nil)
	r := env.ok("tok-alice", "vm_screenshot", map[string]any{"vm": id})
	img, err := png.Decode(bytes.NewReader(r.image))
	if err != nil || img.Bounds().Dx() != 64 || img.Bounds().Dy() != 48 || r.text != "64x48 pixels" {
		t.Fatalf("screenshot: %v, %v, %q", err, img.Bounds(), r.text)
	}
	env.ok("tok-alice", "vm_click", map[string]any{"vm": id, "x": 63, "y": 0, "double": true})
	env.ok("tok-alice", "vm_click", map[string]any{"vm": id, "x": 10, "y": 47, "button": "none"})
	env.fails("tok-alice", "vm_click", map[string]any{"vm": id, "x": 64, "y": 0}, "outside the 64x48 display")
	env.ok("tok-alice", "vm_type", map[string]any{"vm": id, "text": "Hi!\n"})
	env.fails("tok-alice", "vm_type", map[string]any{"vm": id, "text": "ü"}, "cannot be typed")
	env.fails("tok-alice", "vm_type", map[string]any{"vm": id, "text": strings.Repeat("a", 4097)}, "1 to 4096 characters")
	env.ok("tok-alice", "vm_key", map[string]any{"vm": id, "keys": []string{"ctrl+alt+t", "Enter"}})
	env.fails("tok-alice", "vm_key", map[string]any{"vm": id, "keys": []string{"ctrl+hyper"}}, "unknown key")

	var got []string
	for _, c := range env.hv.recorded() {
		if strings.HasPrefix(c, "qmp ") || strings.HasPrefix(c, "keys ") {
			got = append(got, c)
		}
	}
	move := func(x, y int) string {
		return fmt.Sprintf(`qmp {"arguments":{"events":[{"type":"abs","data":{"axis":"x","value":%d}},{"type":"abs","data":{"axis":"y","value":%d}}]},"execute":"input-send-event"}`, x, y)
	}
	btn := func(down bool) string {
		return fmt.Sprintf(`qmp {"arguments":{"events":[{"type":"btn","data":{"down":%v,"button":"left"}}]},"execute":"input-send-event"}`, down)
	}
	want := []string{
		move(absMax, 0), btn(true), btn(false), btn(true), btn(false),
		move(10*absMax/63, absMax),
		"keys [42 35]", "keys [23]", "keys [42 2]", "keys [28]",
		"keys [29 56 20]", "keys [28]",
	}
	if !slices.Equal(got, want) {
		t.Errorf("input =\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

func TestExecAndFiles(t *testing.T) {
	env := newEnv(t)
	id := env.create("tok-alice", nil)
	r := env.ok("tok-alice", "vm_exec", map[string]any{"vm": id, "argv": []string{"echo", "hi"}, "env": []string{"A=b"}})
	if r.data["stdout"] != "[echo hi]" || r.data["exit_code"] != 0.0 {
		t.Errorf("vm_exec = %s", r.raw)
	}
	if r := env.call("tok-alice", "vm_exec", map[string]any{"vm": id, "argv": []string{"false"}}); !r.isError || r.data["exit_code"] != 1.0 {
		t.Errorf("vm_exec(false) = %s", r.raw)
	}
	env.fails("tok-alice", "vm_exec", map[string]any{"vm": id, "argv": []string{""}}, "must name a program")
	env.fails("tok-alice", "vm_exec", map[string]any{"vm": id, "argv": []string{"x"}, "env": []string{"novalue"}}, "not NAME=VALUE")
	env.fails("tok-alice", "vm_exec", map[string]any{"vm": id, "argv": []string{"x"}, "timeout_seconds": 1801}, "timeout_seconds")
	env.fails("tok-alice", "vm_exec", map[string]any{"vm": id, "argv": slices.Repeat([]string{"x"}, 257)}, "entries")

	content := []byte("#!/bin/sh\necho app\n")
	env.ok("tok-alice", "vm_copy_in", map[string]any{"vm": id, "path": "/tmp/app.sh", "content_base64": base64.StdEncoding.EncodeToString(content)})
	env.ok("tok-alice", "vm_copy_in", map[string]any{"vm": id, "path": "/tmp/app.sh", "content_base64": base64.StdEncoding.EncodeToString([]byte("x")), "append": true})
	r = env.ok("tok-alice", "vm_copy_out", map[string]any{"vm": id, "path": "/tmp/app.sh"})
	if got, _ := base64.StdEncoding.DecodeString(r.data["content_base64"].(string)); string(got) != string(content)+"x" || r.data["eof"] != true {
		t.Errorf("vm_copy_out = %s", r.raw)
	}
	env.fails("tok-alice", "vm_copy_in", map[string]any{"vm": id, "path": "tmp/x", "content_base64": ""}, "not absolute")
	env.fails("tok-alice", "vm_copy_in", map[string]any{"vm": id, "path": "/x", "content_base64": "!!"}, "content_base64")
	env.fails("tok-alice", "vm_copy_out", map[string]any{"vm": id, "path": "/missing"}, "No such file")
	env.fails("tok-alice", "vm_copy_out", map[string]any{"vm": id, "path": "/x", "length": MaxCopy + 1}, "length must be")
}

func TestCopyInLimit(t *testing.T) {
	env := newEnv(t)
	id := env.create("tok-alice", nil)
	big := base64.StdEncoding.EncodeToString(make([]byte, MaxCopy+1))
	env.fails("tok-alice", "vm_copy_in", map[string]any{"vm": id, "path": "/x", "content_base64": big}, "may have 16777216 bytes")
	ok := base64.StdEncoding.EncodeToString(make([]byte, MaxCopy))
	env.ok("tok-alice", "vm_copy_in", map[string]any{"vm": id, "path": "/x", "content_base64": ok})
}

func TestSnapshots(t *testing.T) {
	env := newEnv(t)
	id := env.create("tok-alice", nil)
	put := func(s string) {
		env.ok("tok-alice", "vm_copy_in", map[string]any{"vm": id, "path": "/f", "content_base64": base64.StdEncoding.EncodeToString([]byte(s))})
	}
	put("before")
	env.ok("tok-alice", "vm_snapshot", map[string]any{"vm": id, "name": "clean"})
	env.fails("tok-alice", "vm_snapshot", map[string]any{"vm": id, "name": "clean"}, "already")
	put("after")
	env.ok("tok-alice", "vm_restore", map[string]any{"vm": id, "name": "clean"})
	r := env.ok("tok-alice", "vm_copy_out", map[string]any{"vm": id, "path": "/f"})
	if got, _ := base64.StdEncoding.DecodeString(r.data["content_base64"].(string)); string(got) != "before" {
		t.Errorf("after restore /f = %q", got)
	}
	env.fails("tok-alice", "vm_restore", map[string]any{"vm": id, "name": "other"}, "no snapshot")
	env.ok("tok-alice", "vm_snapshot", map[string]any{"vm": id, "name": "s2"})
	env.ok("tok-alice", "vm_snapshot", map[string]any{"vm": id, "name": "s3"})
	env.fails("tok-alice", "vm_snapshot", map[string]any{"vm": id, "name": "s4"}, "may have 3 snapshots")
	env.ok("tok-alice", "vm_snapshot_delete", map[string]any{"vm": id, "name": "s2"})
	r = env.ok("tok-alice", "vm_list", nil)
	if s := r.data["vms"].([]any)[0].(map[string]any)["snapshots"]; fmt.Sprint(s) != "[clean s3]" {
		t.Errorf("snapshots = %v", s)
	}
	// The record keeps them across a restart.
	env.start(env.c.cfg)
	if recs := env.c.List(); len(recs) != 1 || !slices.Equal(recs[0].Snapshots, []string{"clean", "s3"}) {
		t.Errorf("after restart: %+v", recs)
	}
}

func TestReaper(t *testing.T) {
	env := newEnv(t)
	short := env.create("tok-alice", map[string]any{"lifetime_minutes": 10, "cpus": 1})
	long := env.create("tok-alice", map[string]any{"cpus": 1})
	bobs := env.create("tok-bob", nil)

	env.clock = env.clock.Add(10 * time.Minute)
	env.c.Reap(context.Background())
	if ids := ids(env.c.List()); !slices.Equal(ids, []string{long, bobs}) {
		t.Errorf("after expiry: %v", ids)
	}
	// Bob's sandbox is detached: his token goes, and so do his VMs.
	writeTokens(t, filepath.Join(env.dir, "tokens.json"), map[string]Owner{"tok-alice": alice})
	env.c.Reap(context.Background())
	if ids := ids(env.c.List()); !slices.Equal(ids, []string{long}) {
		t.Errorf("after detach: %v", ids)
	}
	if _, ok := env.hv.domains[DomainName(short)]; ok {
		t.Error("expired domain left")
	}
	// A broken token file reaps nothing.
	if err := os.WriteFile(filepath.Join(env.dir, "tokens.json"), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	env.c.Reap(context.Background())
	if len(env.c.List()) != 1 {
		t.Error("broken token file reaped a VM")
	}
}

func ids(recs []Record) []string {
	var out []string
	for _, r := range recs {
		out = append(out, r.ID)
	}
	return out
}

func TestStart(t *testing.T) {
	env := newEnv(t)
	id := env.create("tok-alice", nil)
	env.fails("tok-alice", "vm_start", map[string]any{"vm": id}, "running already")
	env.hv.domains[DomainName(id)].running = false
	env.ok("tok-alice", "vm_start", map[string]any{"vm": id})
	if !env.hv.domains[DomainName(id)].running {
		t.Error("VM not started")
	}
}

func TestRestartKeepsVMs(t *testing.T) {
	env := newEnv(t)
	id := env.create("tok-alice", nil)
	// A directory without a record is a crashed create.
	if err := os.MkdirAll(filepath.Join(env.dir, "vms", "0badf00d"), 0o700); err != nil {
		t.Fatal(err)
	}
	env.start(env.c.cfg)
	if got := ids(env.c.List()); !slices.Equal(got, []string{id}) {
		t.Errorf("after restart: %v", got)
	}
	if _, err := os.Stat(filepath.Join(env.dir, "vms", "0badf00d")); !os.IsNotExist(err) {
		t.Error("stale directory kept")
	}
	env.ok("tok-alice", "vm_screenshot", map[string]any{"vm": id})
}

func TestBusyLimit(t *testing.T) {
	env := newEnv(t)
	var dones []func()
	for range maxBusy {
		done, err := env.c.enter(alice)
		if err != nil {
			t.Fatal(err)
		}
		dones = append(dones, done)
	}
	env.fails("tok-alice", "vm_list", nil, "still running")
	env.ok("tok-bob", "vm_list", nil)
	dones[0]()
	env.ok("tok-alice", "vm_list", nil)
}

func TestDestroyCancelsOperations(t *testing.T) {
	env := newEnv(t)
	id := env.create("tok-alice", nil)
	e, err := env.c.get(alice, id)
	if err != nil {
		t.Fatal(err)
	}
	started := make(chan struct{})
	errc := make(chan error, 1)
	go func() {
		errc <- env.c.do(context.Background(), alice, id, false, func(ctx context.Context, _ Hypervisor, _ *entry) error {
			close(started)
			<-ctx.Done()
			return ctx.Err()
		})
	}()
	<-started
	if err := env.c.Destroy(context.Background(), alice, id); err != nil {
		t.Fatal(err)
	}
	if err := <-errc; !errors.Is(err, context.Canceled) {
		t.Errorf("operation ended with %v", err)
	}
	if e.ctx.Err() == nil {
		t.Error("VM context not cancelled")
	}
}

func TestToolsList(t *testing.T) {
	env := newEnv(t)
	req, _ := http.NewRequest(http.MethodPost, env.srv.URL, strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
	req.Header.Set("Authorization", "Bearer tok-alice")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	var r struct {
		Result struct {
			Tools []struct {
				Name        string          `json:"name"`
				InputSchema json.RawMessage `json:"inputSchema"`
			} `json:"tools"`
		} `json:"result"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, tool := range r.Result.Tools {
		names = append(names, tool.Name)
		var s map[string]any
		if err := json.Unmarshal(tool.InputSchema, &s); err != nil || s["type"] != "object" {
			t.Errorf("%s: schema %s: %v", tool.Name, tool.InputSchema, err)
		}
	}
	want := []string{
		"vm_images", "vm_create", "vm_list", "vm_start", "vm_screenshot", "vm_click", "vm_type", "vm_key", "vm_exec",
		"vm_copy_in", "vm_copy_out", "vm_snapshot", "vm_restore", "vm_snapshot_delete", "vm_destroy",
	}
	if !slices.Equal(names, want) {
		t.Errorf("tools = %v", names)
	}
	if r := env.ok("tok-alice", "vm_images", nil); fmt.Sprint(r.data["images"]) != "[map[name:fedora]]" {
		t.Errorf("vm_images = %s", r.raw)
	}
}
