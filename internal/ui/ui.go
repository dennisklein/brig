// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

// Package ui implements brig ui, a local web UI for brig's VMs.
//
// It serves server-rendered pages (html/template) with htmx and one
// Server-Sent Events stream per tab for live updates, all embedded in the
// binary. It listens on 127.0.0.1 only, and lets in only the user's own
// processes holding the per-run token from the URL it prints.
package ui

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io"
	"log/slog"
	"net"
	"net/http"
	neturl "net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/dennisklein/brig/internal/audit"
	"github.com/dennisklein/brig/internal/openshell"
	"github.com/dennisklein/brig/internal/paths"
	"github.com/dennisklein/brig/internal/ui/collect"
	"github.com/dennisklein/brig/internal/ui/demo"
	"github.com/dennisklein/brig/internal/ui/run"
	"github.com/dennisklein/brig/internal/ui/store"
	"github.com/dennisklein/brig/internal/vm"
)

// clock is the time the pages measure from. It is the system clock, except
// in a still demo, where Run sets it once, before it serves.
var clock = time.Now

// Options configure Run.
type Options struct {
	// Port to listen on; 0 picks a free one.
	Port int
	// NoOpen keeps the browser closed; the URL is printed either way.
	NoOpen bool
	// Demo serves made-up VMs and touches nothing on the host.
	Demo bool
	// Still stops the demo's clock at demo.Epoch and its meters and
	// requests with it, so that the pages look the same on every run. It
	// is for screenshots, and needs Demo.
	Still bool
	// DevAssets serves the templates and static files from this directory,
	// read again on every request.
	DevAssets string
	// Dirs are brig's directories.
	Dirs paths.Dirs
	// ActivityLog is the event log that Activity shows, empty for none.
	ActivityLog string
	// Brig is the brig executable that runs the commands of dialogs;
	// empty for this one.
	Brig string
	// RuntimeDir is $XDG_RUNTIME_DIR, where the running UI is recorded so
	// that a second brig ui opens it instead of starting another. Empty
	// records nothing.
	RuntimeDir string
	Stdout     io.Writer
	Stderr     io.Writer
}

const (
	// keepFinished is how long a command that succeeded stays among the
	// task cards.
	keepFinished = 10 * time.Second
	// interruptGrace is how long commands get to roll back after Ctrl-C.
	interruptGrace = 60 * time.Second
)

// runtimeRecord is what $XDG_RUNTIME_DIR/brig/ui.json holds.
type runtimeRecord struct {
	URL string `json:"url"`
	PID int    `json:"pid"`
}

// Run serves brig ui until ctx ends. When another brig ui of the user runs,
// it opens that one's URL and returns.
func Run(ctx context.Context, o Options) error {
	log := slog.New(slog.NewTextHandler(o.Stderr, nil))
	record := ""
	if !o.Demo && o.RuntimeDir != "" {
		record = filepath.Join(o.RuntimeDir, "brig", "ui.json")
		if r, ok := running(record); ok {
			fmt.Fprintf(o.Stdout, "brig ui is already running: %s\n", r.URL)
			if !o.NoOpen {
				openBrowser(o.RuntimeDir, r.URL, log)
			}
			return nil
		}
	}

	l, err := listen(o.Port, log)
	if errors.Is(err, syscall.EADDRINUSE) {
		return fmt.Errorf("port %d is in use; pick another with --port, or leave it out", o.Port)
	}
	if err != nil {
		return err
	}
	defer func() { _ = l.Close() }()
	port := l.Addr().(*net.TCPAddr).Port
	addr := "127.0.0.1:" + strconv.Itoa(port)
	token := newSecret()
	url := "http://" + addr + "/?token=" + token

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	st := store.New()
	var wg sync.WaitGroup
	defer wg.Wait()
	goRun := func(f func()) {
		wg.Add(1)
		go func() { defer wg.Done(); f() }()
	}
	home, _ := os.UserHomeDir()
	checkHost := collect.Doctor
	var starter run.Starter
	var files FileLister
	var history audit.Reader
	if o.Demo {
		home = demo.Home
		checkHost = demo.CheckHost
		host := demo.New(st)
		starter, files = host, host
		if o.Still {
			host.Still()
			clock = func() time.Time { return demo.Epoch }
		}
		history = demo.History(clock())
		goRun(func() { host.Run(ctx, addr) })
	} else {
		brig := o.Brig
		if brig == "" {
			if brig, err = os.Executable(); err != nil {
				return fmt.Errorf("finding brig's own executable: %w", err)
			}
		}
		collector := collect.NewBrig(o.Dirs)
		// Without the openshell CLI, brig ui still shows gateways; its
		// dialogs for sandboxes say what is missing.
		cli, _ := openshell.Find()
		starter = &run.Runner{
			Brig: brig, OpenShell: cli, ActivityLog: o.ActivityLog, Store: st, Ended: collector.Refresh, KeepOK: keepFinished,
		}
		st.SetHost(store.Host{Addr: addr})
		goRun(func() { collect.HostRun(ctx, st, addr) })
		goRun(func() { collector.Run(ctx, st) })
		trail := audit.NewStore(o.Dirs)
		history = trail
		goRun(func() { collect.Audit(ctx, st, trail) })
		if home, err := openshell.ConfigHome(); err != nil {
			log.Warn("brig ui cannot read OpenShell gateways", "err", err)
		} else {
			gws := collect.NewGateways(home)
			gws.Log = log
			goRun(func() { gws.Run(ctx, st) })
			files = collect.NewFiles(st, home)
		}
		if o.ActivityLog != "" {
			goRun(func() { collect.Activity(ctx, st, o.ActivityLog) })
		}
	}

	// Commands that dialogs started get Ctrl-C too, so that brig rolls back
	// what it began.
	defer func() {
		if n := starter.Running(); n > 0 {
			fmt.Fprintf(o.Stderr, "Interrupting %d running brig command(s); they roll back what they began.\n", n)
			starter.Shutdown(interruptGrace)
		}
	}()

	srv, err := newServer(ctx, serverConfig{
		Store: st, Token: token, Port: port, Demo: o.Demo, Home: home,
		DevAssets: o.DevAssets, CheckHost: checkHost, Starter: starter, Files: files, History: history, Log: log,
	})
	if err != nil {
		return err
	}
	hs := &http.Server{
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		ErrorLog:          slog.NewLogLogger(log.Handler(), slog.LevelWarn),
	}

	if record != "" {
		if err := writeRecord(record, runtimeRecord{URL: url, PID: os.Getpid()}); err != nil {
			return err
		}
		defer func() { _ = os.Remove(record) }()
	}
	if o.Demo {
		fmt.Fprintln(o.Stdout, "brig ui is serving made-up VMs; nothing touches this host.")
	}
	fmt.Fprintf(o.Stdout, "brig ui: %s\nPress Ctrl-C to stop.\n", url)
	if !o.NoOpen {
		openBrowser(o.RuntimeDir, url, log)
	}

	errc := make(chan error, 1)
	go func() { errc <- hs.Serve(l) }()
	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	// The live update streams end with ctx; then the rest drains.
	cancel()
	sctx, scancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer scancel()
	return hs.Shutdown(sctx)
}

// running returns the record of a brig ui that still runs.
func running(path string) (runtimeRecord, bool) {
	var r runtimeRecord
	data, err := os.ReadFile(path)
	if err != nil || json.Unmarshal(data, &r) != nil || r.PID <= 0 || r.URL == "" {
		return r, false
	}
	// Signal 0 checks that the process exists and is ours; a PID used
	// again by another process does not listen at the URL.
	if syscall.Kill(r.PID, 0) != nil {
		return r, false
	}
	u, err := neturl.Parse(r.URL)
	if err != nil {
		return r, false
	}
	c, err := net.DialTimeout("tcp4", u.Host, time.Second)
	if err != nil {
		return r, false
	}
	_ = c.Close()
	return r, true
}

// writeRecord writes the runtime record, readable only by the user.
func writeRecord(path string, r runtimeRecord) error {
	if err := paths.EnsurePrivate(filepath.Dir(path)); err != nil {
		return err
	}
	data, err := json.Marshal(r)
	if err != nil {
		return err
	}
	return vm.WriteFileAtomic(path, append(data, '\n'), 0o600)
}

// openTemplate is a page that sends the browser on to the URL. The browser
// opens this file rather than the URL itself, so that the token never
// stands in a command line, which other users can read in /proc.
var openTemplate = template.Must(template.New("").Parse(`<!doctype html>
<html><head><meta charset="utf-8"><meta http-equiv="refresh" content="0;url={{.}}"><title>brig ui</title></head>
<body><p><a href="{{.}}">Open brig ui</a></p></body></html>
`))

// openBrowser opens url in the user's browser, through a private file in
// runtimeDir when there is one. A failure is only logged: the URL is
// printed anyway.
func openBrowser(runtimeDir, url string, log *slog.Logger) {
	target := url
	if runtimeDir != "" {
		path := filepath.Join(runtimeDir, "brig", "ui-open.html")
		if err := writeOpenFile(path, url); err != nil {
			log.Warn("could not write the page that opens brig ui", "err", err)
		} else {
			target = path
		}
	}
	cmd := exec.Command("xdg-open", target)
	if err := cmd.Start(); err != nil {
		log.Warn("could not open a browser; open the URL above yourself", "err", err)
		return
	}
	go func() { _ = cmd.Wait() }()
}

func writeOpenFile(path, url string) error {
	if err := paths.EnsurePrivate(filepath.Dir(path)); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".ui-open-*")
	if err != nil {
		return err
	}
	if err := openTemplate.Execute(f, template.URL(url)); err != nil { //nolint:gosec // our own URL
		_ = f.Close()
		_ = os.Remove(f.Name())
		return err
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(f.Name())
		return err
	}
	if err := os.Rename(f.Name(), path); err != nil {
		_ = os.Remove(f.Name())
		return err
	}
	return nil
}
