// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

package vmrunner

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"path"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/dennisklein/brig/internal/bytesize"
	"github.com/dennisklein/brig/internal/mcp"
	"github.com/dennisklein/brig/internal/qga"
	"github.com/dennisklein/brig/internal/version"
)

// Limits of the tools' arguments and results.
const (
	// MaxCopy bounds the file data of one vm_copy_in or vm_copy_out call.
	MaxCopy = 16 << 20
	// MaxBody bounds a request: a vm_copy_in call with MaxCopy bytes in
	// base64 and room for the rest.
	MaxBody = MaxCopy/3*4 + 1<<20
	// maxText bounds vm_type's text.
	maxText = 4096
	// maxCombos bounds the key combinations of one vm_key call.
	maxCombos = 64
	// maxArgv and maxArgvBytes bound vm_exec's command line, maxStdin its
	// standard input and maxOutput what it returns of stdout and stderr.
	maxArgv      = 256
	maxArgvBytes = 128 << 10
	maxStdin     = 1 << 20
	maxOutput    = 1 << 20
	// defaultExecTimeout and maxExecTimeout bound vm_exec's wait.
	defaultExecTimeout = 5 * time.Minute
	maxExecTimeout     = 30 * time.Minute
	maxPath            = 4096
)

const instructions = `These tools start throwaway VMs on the user's host and drive them, for
testing software in a real OS with a screen. The VMs have no network. Use
vm_images to see the images you may use, vm_create to start a VM, and the
id it returns in the other tools. Look at the screen with vm_screenshot and
use its pixel coordinates in vm_click. Run commands with vm_exec (as root,
through the guest agent) and move files with vm_copy_in and vm_copy_out.
vm_snapshot and vm_restore save and return to a state. Destroy VMs you no
longer need with vm_destroy; brig destroys them when their lifetime ends.`

// Server returns the MCP server that offers the tools.
func (c *Controller) Server(log *slog.Logger) *mcp.Server {
	return &mcp.Server{
		Name: "brig-vm-runner", Version: version.Version(), Instructions: instructions,
		Tools: c.tools(), Auth: c.tokens.Auth, MaxBody: MaxBody, Log: log,
	}
}

// call wraps a tool: it finds the caller, bounds its calls in progress and
// logs failures.
func (c *Controller) call(name string, f func(ctx context.Context, o Owner, args json.RawMessage) (*mcp.Result, error)) func(context.Context, json.RawMessage) (*mcp.Result, error) {
	return func(ctx context.Context, args json.RawMessage) (*mcp.Result, error) {
		o, ok := ownerOf(ctx)
		if !ok {
			return nil, errors.New("no caller")
		}
		done, err := c.enter(o)
		if err != nil {
			return nil, err
		}
		defer done()
		res, err := f(ctx, o, args)
		if err != nil {
			c.log.Info("tool failed", "tool", name, "owner", o.String(), "err", err)
		}
		return res, err
	}
}

func schema(props string, required ...string) json.RawMessage {
	req, _ := json.Marshal(required)
	if len(required) == 0 {
		req = []byte("[]")
	}
	return json.RawMessage(`{"type":"object","additionalProperties":false,"properties":{` + props + `},"required":` + string(req) + `}`)
}

const vmProp = `"vm":{"type":"string","description":"The VM's id from vm_create or vm_list."}`

func (c *Controller) tools() []mcp.Tool {
	return []mcp.Tool{
		{
			Name: "vm_images", Title: "List images", ReadOnly: true,
			Description: "Lists the images that vm_create can start a VM from.",
			InputSchema: schema(""),
			Call:        c.call("vm_images", c.toolImages),
		},
		{
			Name: "vm_create", Title: "Create a VM",
			Description: "Creates and starts a VM from an image and waits until its guest agent answers. Returns the VM's id.",
			InputSchema: schema(`"image":{"type":"string","description":"An image name from vm_images."},`+
				`"cpus":{"type":"integer","minimum":1,"description":"Virtual CPUs, default 2."},`+
				`"memory_mib":{"type":"integer","minimum":256,"description":"Memory in MiB, default 4096."},`+
				`"disk_gib":{"type":"integer","minimum":1,"description":"Disk size in GiB, default the image's size."},`+
				`"lifetime_minutes":{"type":"integer","minimum":1,"description":"Minutes until brig destroys the VM, default and most the quota's lifetime."}`, "image"),
			Call: c.call("vm_create", c.toolCreate),
		},
		{
			Name: "vm_list", Title: "List VMs", ReadOnly: true,
			Description: "Lists your VMs with their state, size, snapshots and when brig destroys them.",
			InputSchema: schema(""),
			Call:        c.call("vm_list", c.toolList),
		},
		{
			Name: "vm_start", Title: "Start a VM",
			Description: "Boots a VM that is shut off, as after its OS powered off or the host restarted.",
			InputSchema: schema(vmProp, "vm"),
			Call:        c.call("vm_start", c.toolStart),
		},
		{
			Name: "vm_screenshot", Title: "Take a screenshot", ReadOnly: true,
			Description: "Returns the VM's screen as a PNG image. vm_click takes the image's pixel coordinates.",
			InputSchema: schema(vmProp, "vm"),
			Call:        c.call("vm_screenshot", c.toolScreenshot),
		},
		{
			Name: "vm_click", Title: "Click",
			Description: "Moves the pointer to a pixel of the last screenshot and clicks a button. Button none only moves; wheel-up and wheel-down scroll.",
			InputSchema: schema(vmProp+`,"x":{"type":"integer","minimum":0},"y":{"type":"integer","minimum":0},`+
				`"button":{"type":"string","enum":["left","right","middle","wheel-up","wheel-down","none"],"description":"Default left."},`+
				`"double":{"type":"boolean","description":"Click twice."}`, "vm", "x", "y"),
			Call: c.call("vm_click", c.toolClick),
		},
		{
			Name: "vm_type", Title: "Type text",
			Description: "Types text on a US keyboard layout: printable ASCII, tab and newline, at most 4096 characters.",
			InputSchema: schema(vmProp+`,"text":{"type":"string"}`, "vm", "text"),
			Call:        c.call("vm_type", c.toolType),
		},
		{
			Name: "vm_key", Title: "Press keys",
			Description: "Presses key combinations one after another, such as [\"ctrl+alt+t\", \"enter\"]. Keys: letters, digits, f1-f12, enter, esc, tab, space, backspace, delete, insert, home, end, pageup, pagedown, up, down, left, right, ctrl, shift, alt, altgr, super, menu, printscreen and punctuation names such as minus, comma, dot and slash.",
			InputSchema: schema(vmProp+`,"keys":{"type":"array","items":{"type":"string"},"minItems":1,"maxItems":64}`, "vm", "keys"),
			Call:        c.call("vm_key", c.toolKey),
		},
		{
			Name: "vm_exec", Title: "Run a command", Destructive: true,
			Description: "Runs a program in the VM as root through the guest agent and returns its exit code, stdout and stderr (at most 1 MiB each). No shell: pass [\"sh\", \"-c\", \"...\"] for one.",
			InputSchema: schema(vmProp+`,"argv":{"type":"array","items":{"type":"string"},"minItems":1,"description":"The program and its arguments."},`+
				`"stdin":{"type":"string","description":"Standard input, at most 1 MiB."},`+
				`"env":{"type":"array","items":{"type":"string"},"description":"NAME=VALUE entries."},`+
				`"timeout_seconds":{"type":"integer","minimum":1,"maximum":1800,"description":"Kill the program after this long, default 300."}`, "vm", "argv"),
			Call: c.call("vm_exec", c.toolExec),
		},
		{
			Name: "vm_copy_in", Title: "Copy a file into the VM", Destructive: true,
			Description: "Writes base64 content, at most 16 MiB, to an absolute path in the VM, owned by root. Larger files: write the first part, then append the rest.",
			InputSchema: schema(vmProp+`,"path":{"type":"string"},"content_base64":{"type":"string"},`+
				`"append":{"type":"boolean","description":"Append to the file instead of replacing it."}`, "vm", "path", "content_base64"),
			Call: c.call("vm_copy_in", c.toolCopyIn),
		},
		{
			Name: "vm_copy_out", Title: "Copy a file out of the VM", ReadOnly: true,
			Description: "Reads at most 16 MiB of a file in the VM from an offset and returns them as base64, with eof true at the end of the file.",
			InputSchema: schema(vmProp+`,"path":{"type":"string"},"offset":{"type":"integer","minimum":0},`+
				`"length":{"type":"integer","minimum":1,"maximum":16777216,"description":"Default 16 MiB."}`, "vm", "path"),
			Call: c.call("vm_copy_out", c.toolCopyOut),
		},
		{
			Name: "vm_snapshot", Title: "Take a snapshot",
			Description: "Saves the VM's disk and memory as a named snapshot that vm_restore returns to.",
			InputSchema: schema(vmProp+`,"name":{"type":"string","pattern":"^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$"}`, "vm", "name"),
			Call:        c.call("vm_snapshot", c.toolSnapshot),
		},
		{
			Name: "vm_restore", Title: "Restore a snapshot", Destructive: true,
			Description: "Returns the VM to a snapshot; changes since are lost.",
			InputSchema: schema(vmProp+`,"name":{"type":"string"}`, "vm", "name"),
			Call:        c.call("vm_restore", c.toolRestore),
		},
		{
			Name: "vm_snapshot_delete", Title: "Delete a snapshot", Destructive: true,
			Description: "Deletes a snapshot of the VM.",
			InputSchema: schema(vmProp+`,"name":{"type":"string"}`, "vm", "name"),
			Call:        c.call("vm_snapshot_delete", c.toolSnapshotDelete),
		},
		{
			Name: "vm_destroy", Title: "Destroy a VM", Destructive: true,
			Description: "Powers the VM off and deletes it with its disk and snapshots.",
			InputSchema: schema(vmProp, "vm"),
			Call:        c.call("vm_destroy", c.toolDestroy),
		},
	}
}

type imageInfo struct {
	Name string `json:"name"`
}

func (c *Controller) toolImages(_ context.Context, _ Owner, args json.RawMessage) (*mcp.Result, error) {
	if err := mcp.Decode(args, &struct{}{}); err != nil {
		return nil, err
	}
	names := make([]string, 0, len(c.cfg.Images))
	for name := range c.cfg.Images {
		names = append(names, name)
	}
	slices.Sort(names)
	out := make([]imageInfo, len(names))
	for i, n := range names {
		out[i] = imageInfo{Name: n}
	}
	return mcp.JSON(map[string]any{"images": out})
}

func (c *Controller) toolCreate(ctx context.Context, o Owner, args json.RawMessage) (*mcp.Result, error) {
	var a struct {
		Image           string `json:"image"`
		CPUs            int    `json:"cpus"`
		MemoryMiB       int    `json:"memory_mib"`
		DiskGiB         int    `json:"disk_gib"`
		LifetimeMinutes int    `json:"lifetime_minutes"`
	}
	if err := mcp.Decode(args, &a); err != nil {
		return nil, err
	}
	// Bound the numbers before they are multiplied: the quota checks the
	// rest.
	if a.CPUs < 0 || a.CPUs > 1024 || a.MemoryMiB < 0 || a.MemoryMiB > 1<<24 || a.DiskGiB < 0 || a.DiskGiB > 1<<16 ||
		a.LifetimeMinutes < 0 || a.LifetimeMinutes > 1<<20 {
		return nil, errors.New("cpus, memory_mib, disk_gib or lifetime_minutes out of range")
	}
	res, err := c.Create(ctx, o, CreateRequest{
		Image: a.Image, CPUs: a.CPUs,
		Memory:   bytesize.Size(a.MemoryMiB) * bytesize.MiB,
		Disk:     bytesize.Size(a.DiskGiB) * bytesize.GiB,
		Lifetime: time.Duration(a.LifetimeMinutes) * time.Minute,
	})
	if err != nil {
		return nil, err
	}
	out := map[string]any{
		"id": res.ID, "image": res.Image, "cpus": res.CPUs, "memory_mib": res.Memory.MiB(),
		"disk_gib": float64(res.Disk) / (1 << 30), "expires_at": res.ExpiresAt,
		"guest_agent": "ready",
	}
	if res.AgentError != "" {
		out["guest_agent"] = "not answering: vm_exec and file copies will fail until it does; screenshots and input work"
	}
	return mcp.JSON(out)
}

func (c *Controller) toolList(ctx context.Context, o Owner, args json.RawMessage) (*mcp.Result, error) {
	if err := mcp.Decode(args, &struct{}{}); err != nil {
		return nil, err
	}
	vms, err := c.ListOwned(ctx, o)
	if err != nil {
		return nil, err
	}
	return mcp.JSON(map[string]any{"vms": vms})
}

type vmArg struct {
	VM string `json:"vm"`
}

func (c *Controller) toolStart(ctx context.Context, o Owner, args json.RawMessage) (*mcp.Result, error) {
	var a vmArg
	if err := mcp.Decode(args, &a); err != nil {
		return nil, err
	}
	if err := c.Start(ctx, o, a.VM); err != nil {
		return nil, err
	}
	return mcp.Text("started"), nil
}

func (c *Controller) toolScreenshot(ctx context.Context, o Owner, args json.RawMessage) (*mcp.Result, error) {
	var a vmArg
	if err := mcp.Decode(args, &a); err != nil {
		return nil, err
	}
	png, w, h, err := c.Screenshot(ctx, o, a.VM)
	if err != nil {
		return nil, err
	}
	return &mcp.Result{Content: []mcp.Content{
		{Type: "image", Data: base64.StdEncoding.EncodeToString(png), MIMEType: "image/png"},
		{Type: "text", Text: fmt.Sprintf("%dx%d pixels", w, h)},
	}}, nil
}

func (c *Controller) toolClick(ctx context.Context, o Owner, args json.RawMessage) (*mcp.Result, error) {
	var a struct {
		VM     string `json:"vm"`
		X      int    `json:"x"`
		Y      int    `json:"y"`
		Button string `json:"button"`
		Double bool   `json:"double"`
	}
	if err := mcp.Decode(args, &a); err != nil {
		return nil, err
	}
	if a.Button == "" {
		a.Button = "left"
	}
	if err := c.Click(ctx, o, a.VM, a.X, a.Y, a.Button, a.Double); err != nil {
		return nil, err
	}
	return mcp.Text("done"), nil
}

func (c *Controller) toolType(ctx context.Context, o Owner, args json.RawMessage) (*mcp.Result, error) {
	var a struct {
		VM   string `json:"vm"`
		Text string `json:"text"`
	}
	if err := mcp.Decode(args, &a); err != nil {
		return nil, err
	}
	if n := utf8.RuneCountInString(a.Text); n == 0 || n > maxText {
		return nil, fmt.Errorf("text must have 1 to %d characters, has %d", maxText, n)
	}
	if err := c.Type(ctx, o, a.VM, a.Text); err != nil {
		return nil, err
	}
	return mcp.Text("done"), nil
}

func (c *Controller) toolKey(ctx context.Context, o Owner, args json.RawMessage) (*mcp.Result, error) {
	var a struct {
		VM   string   `json:"vm"`
		Keys []string `json:"keys"`
	}
	if err := mcp.Decode(args, &a); err != nil {
		return nil, err
	}
	if len(a.Keys) == 0 || len(a.Keys) > maxCombos {
		return nil, fmt.Errorf("keys must have 1 to %d entries", maxCombos)
	}
	if err := c.Keys(ctx, o, a.VM, a.Keys); err != nil {
		return nil, err
	}
	return mcp.Text("done"), nil
}

func (c *Controller) toolExec(ctx context.Context, o Owner, args json.RawMessage) (*mcp.Result, error) {
	var a struct {
		VM             string   `json:"vm"`
		Argv           []string `json:"argv"`
		Stdin          string   `json:"stdin"`
		Env            []string `json:"env"`
		TimeoutSeconds int      `json:"timeout_seconds"`
	}
	if err := mcp.Decode(args, &a); err != nil {
		return nil, err
	}
	n := 0
	for _, s := range append(slices.Clone(a.Argv), a.Env...) {
		n += len(s)
	}
	switch {
	case len(a.Argv) == 0 || a.Argv[0] == "":
		return nil, errors.New("argv must name a program")
	case len(a.Argv)+len(a.Env) > maxArgv || n > maxArgvBytes:
		return nil, fmt.Errorf("argv and env may have %d entries and %d bytes together", maxArgv, maxArgvBytes)
	case len(a.Stdin) > maxStdin:
		return nil, fmt.Errorf("stdin may have %d bytes", maxStdin)
	case a.TimeoutSeconds < 0 || time.Duration(a.TimeoutSeconds)*time.Second > maxExecTimeout:
		return nil, fmt.Errorf("timeout_seconds must be 1 to %d", int(maxExecTimeout.Seconds()))
	}
	for _, e := range a.Env {
		if k, _, ok := strings.Cut(e, "="); !ok || k == "" {
			return nil, fmt.Errorf("env entry %q is not NAME=VALUE", e)
		}
	}
	timeout := defaultExecTimeout
	if a.TimeoutSeconds > 0 {
		timeout = time.Duration(a.TimeoutSeconds) * time.Second
	}
	res, err := c.Exec(ctx, o, a.VM, qga.ExecRequest{
		Argv: a.Argv, Env: a.Env, Stdin: []byte(a.Stdin), Timeout: timeout, MaxOutput: maxOutput,
	})
	if err != nil {
		return nil, err
	}
	out := map[string]any{
		"exit_code": res.ExitCode,
		"stdout":    strings.ToValidUTF8(string(res.Stdout), "�"),
		"stderr":    strings.ToValidUTF8(string(res.Stderr), "�"),
	}
	if res.Signal != 0 {
		out["signal"] = res.Signal
	}
	if res.Truncated {
		out["truncated"] = true
	}
	if res.TimedOut {
		out["timed_out"] = true
	}
	r, err := mcp.JSON(out)
	if err == nil && (res.TimedOut || res.ExitCode != 0) {
		r.IsError = true
	}
	return r, err
}

// checkPath checks a path in the VM.
func checkPath(p string) error {
	switch {
	case p == "" || !strings.HasPrefix(p, "/"):
		return fmt.Errorf("path %q is not absolute", p)
	case len(p) > maxPath || strings.ContainsRune(p, 0):
		return errors.New("path is too long or has a NUL byte")
	case path.Clean(p) == "/":
		return errors.New("path names the root directory")
	}
	return nil
}

func (c *Controller) toolCopyIn(ctx context.Context, o Owner, args json.RawMessage) (*mcp.Result, error) {
	var a struct {
		VM      string `json:"vm"`
		Path    string `json:"path"`
		Content string `json:"content_base64"`
		Append  bool   `json:"append"`
	}
	if err := mcp.Decode(args, &a); err != nil {
		return nil, err
	}
	if err := checkPath(a.Path); err != nil {
		return nil, err
	}
	if base64.StdEncoding.DecodedLen(len(a.Content)) > MaxCopy+3 {
		return nil, fmt.Errorf("content may have %d bytes; append the rest in further calls", MaxCopy)
	}
	data, err := base64.StdEncoding.DecodeString(a.Content)
	if err != nil {
		return nil, fmt.Errorf("content_base64: %w", err)
	}
	if len(data) > MaxCopy {
		return nil, fmt.Errorf("content may have %d bytes; append the rest in further calls", MaxCopy)
	}
	if err := c.CopyIn(ctx, o, a.VM, a.Path, data, a.Append); err != nil {
		return nil, err
	}
	return mcp.JSON(map[string]any{"written": len(data)})
}

func (c *Controller) toolCopyOut(ctx context.Context, o Owner, args json.RawMessage) (*mcp.Result, error) {
	var a struct {
		VM     string `json:"vm"`
		Path   string `json:"path"`
		Offset int64  `json:"offset"`
		Length int    `json:"length"`
	}
	if err := mcp.Decode(args, &a); err != nil {
		return nil, err
	}
	if err := checkPath(a.Path); err != nil {
		return nil, err
	}
	if a.Length == 0 {
		a.Length = MaxCopy
	}
	if a.Length < 0 || a.Length > MaxCopy || a.Offset < 0 {
		return nil, fmt.Errorf("length must be 1 to %d, offset not negative", MaxCopy)
	}
	data, eof, err := c.CopyOut(ctx, o, a.VM, a.Path, a.Offset, a.Length)
	if err != nil {
		return nil, err
	}
	return mcp.JSON(map[string]any{
		"offset": a.Offset, "length": len(data), "eof": eof,
		"content_base64": base64.StdEncoding.EncodeToString(data),
	})
}

type snapArgs struct {
	VM   string `json:"vm"`
	Name string `json:"name"`
}

func (c *Controller) toolSnapshot(ctx context.Context, o Owner, args json.RawMessage) (*mcp.Result, error) {
	var a snapArgs
	if err := mcp.Decode(args, &a); err != nil {
		return nil, err
	}
	if err := c.Snapshot(ctx, o, a.VM, a.Name); err != nil {
		return nil, err
	}
	return mcp.Text("done"), nil
}

func (c *Controller) toolRestore(ctx context.Context, o Owner, args json.RawMessage) (*mcp.Result, error) {
	var a snapArgs
	if err := mcp.Decode(args, &a); err != nil {
		return nil, err
	}
	if err := c.Restore(ctx, o, a.VM, a.Name); err != nil {
		return nil, err
	}
	return mcp.Text("done"), nil
}

func (c *Controller) toolSnapshotDelete(ctx context.Context, o Owner, args json.RawMessage) (*mcp.Result, error) {
	var a snapArgs
	if err := mcp.Decode(args, &a); err != nil {
		return nil, err
	}
	if err := c.DeleteSnapshot(ctx, o, a.VM, a.Name); err != nil {
		return nil, err
	}
	return mcp.Text("done"), nil
}

func (c *Controller) toolDestroy(ctx context.Context, o Owner, args json.RawMessage) (*mcp.Result, error) {
	var a vmArg
	if err := mcp.Decode(args, &a); err != nil {
		return nil, err
	}
	if err := c.Destroy(ctx, o, a.VM); err != nil {
		return nil, err
	}
	return mcp.Text("destroyed"), nil
}
