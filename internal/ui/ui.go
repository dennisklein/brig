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
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/GSI-HPC/go-clikit/localweb"
	"github.com/GSI-HPC/go-clikit/localweb/launch"
	"github.com/GSI-HPC/go-clikit/localweb/session"

	"github.com/dennisklein/brig/internal/audit"
	"github.com/dennisklein/brig/internal/openshell"
	"github.com/dennisklein/brig/internal/paths"
	"github.com/dennisklein/brig/internal/ui/collect"
	"github.com/dennisklein/brig/internal/ui/demo"
	"github.com/dennisklein/brig/internal/ui/run"
	"github.com/dennisklein/brig/internal/ui/store"
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
	// GOOS and Getenv are what the browser is opened by: the operating
	// system, as runtime.GOOS, and a lookup of the environment, as
	// os.Getenv. A nil Getenv reads every variable as unset.
	GOOS   string
	Getenv func(string) string
	Stdout io.Writer
	Stderr io.Writer

	// start runs the desktop's opener; nil is launch.Detached.
	start func(name string, args ...string) error
}

const (
	// keepFinished is how long a command that succeeded stays among the
	// task cards.
	keepFinished = 10 * time.Second
	// interruptGrace is how long commands get to roll back after Ctrl-C.
	interruptGrace = 60 * time.Second
)

// Run serves brig ui until ctx ends. When another brig ui of the user runs,
// it opens that one's URL and returns.
func Run(ctx context.Context, o Options) error {
	log := slog.New(slog.NewTextHandler(o.Stderr, nil))
	var inst *launch.Instance
	if !o.Demo && o.RuntimeDir != "" {
		var err error
		if inst, err = launch.Acquire(ctx, o.RuntimeDir, "brig"); err != nil {
			return fmt.Errorf("preparing the runtime directory: %w", err)
		}
		// The lock is dropped once the record is written, or on an error.
		defer inst.Release()
		if r, ok := inst.Running(); ok {
			fmt.Fprintf(o.Stdout, "brig ui is already running: %s\n", r.URL)
			if !o.NoOpen {
				openBrowser(o, inst.Dir(), r.URL, log)
			}
			return nil
		}
	}

	// Only the user's own processes get through the listener: other users
	// can reach 127.0.0.1 too.
	bound, err := localweb.Listen(ctx, net.JoinHostPort("127.0.0.1", strconv.Itoa(o.Port)), localweb.ListenOptions{})
	if errors.Is(err, syscall.EADDRINUSE) {
		return fmt.Errorf("port %d is in use; pick another with --port, or leave it out", o.Port)
	}
	if err != nil {
		return err
	}
	defer func() { _ = bound.Close() }()
	l, err := localweb.PeerListener(bound.Listeners[0], os.Getuid(), func(err error) {
		log.Warn("refused a connection", "err", err)
	})
	if err != nil {
		return fmt.Errorf("restricting the listener to your processes: %w", err)
	}
	addr := bound.Host()
	token := session.NewToken()
	url := "http://" + addr + "/?token=" + token

	ctx, cancel := context.WithCancel(ctx)
	st := store.New()
	var wg sync.WaitGroup
	// The goroutines end with ctx, so cancel comes before the wait. This
	// holds on every return, the failing ones too.
	defer func() {
		cancel()
		wg.Wait()
	}()
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
		goRun(func() { collect.Drops(ctx, st, trail) })
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
		}
		// Also without a running command: Shutdown refuses the starts that
		// come after it, and waits for one that is just starting.
		starter.Shutdown(interruptGrace)
	}()

	srv, err := newServer(ctx, serverConfig{
		Store: st, Token: token, Bound: bound, Demo: o.Demo, Home: home,
		DevAssets: o.DevAssets, CheckHost: checkHost, Starter: starter, Files: files, History: history, Log: log,
		Go: goRun,
	})
	if err != nil {
		return err
	}
	if inst != nil {
		if err := inst.Publish(url); err != nil {
			return err
		}
		defer func() { _ = inst.Retire(ctx) }()
	}
	if o.Demo {
		fmt.Fprintln(o.Stdout, "brig ui is serving made-up VMs; nothing touches this host.")
	}
	fmt.Fprintf(o.Stdout, "brig ui: %s\nPress Ctrl-C to stop.\n", url)
	if !o.NoOpen {
		dir := ""
		if inst != nil {
			dir = inst.Dir()
		} else if dir, err = os.MkdirTemp("", "brig-ui-"); err != nil {
			log.Warn("could not make a directory for the page that opens brig ui; open the URL above yourself", "err", err)
		} else {
			// The browser reads the page while brig ui runs.
			defer func() { _ = os.RemoveAll(dir) }()
		}
		if dir != "" {
			openBrowser(o, dir, url, log)
		}
	}

	// The live update streams end with ctx; then the rest drains.
	return localweb.Serve(ctx, []net.Listener{l}, srv.Handler(), localweb.ServeOptions{
		ErrorLog: slog.NewLogLogger(log.Handler(), slog.LevelWarn),
	})
}

// openBrowser opens url in the user's browser, through a private file in
// dir, a directory of the user's alone. A failure is only logged: the URL is
// printed anyway.
func openBrowser(o Options, dir, url string, log *slog.Logger) {
	op := launch.Opener{Name: "brig ui", GOOS: o.GOOS, Getenv: o.Getenv, Dir: dir, Start: o.start}
	if why := op.Skip(); why != "" {
		fmt.Fprintf(o.Stderr, "brig ui opens no browser: %s. Open the URL above yourself.\n", why)
		return
	}
	if _, err := op.Open(url); err != nil {
		log.Warn("could not open a browser; open the URL above yourself", "err", err)
	}
}
