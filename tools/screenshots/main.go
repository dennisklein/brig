// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

// Command screenshots takes the screenshots of brig ui that the docs show.
//
// It starts brig ui --demo --still, whose pages are the same on every run,
// drives an installed Chromium through the shots of docs/images/shots.yaml,
// and writes one image per shot, theme and size. It is a module of its own,
// so that brig does not depend on a browser driver.
//
//	cd tools/screenshots && go run . -brig ../../brig
package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"time"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "screenshots:", err)
		os.Exit(1)
	}
}

func run() error {
	var o options
	flag.StringVar(&o.Brig, "brig", "brig", "the brig executable to take the screenshots of")
	flag.StringVar(&o.Chromium, "chromium", "", "the Chromium or Chrome executable (default: $CHROMIUM, else the first of chromium, chromium-browser, google-chrome on PATH)")
	flag.StringVar(&o.Shots, "shots", "../../docs/images/shots.yaml", "the shot list")
	flag.StringVar(&o.Out, "out", "", "the directory for the images (default: the directory of the shot list)")
	flag.BoolVar(&o.NoSandbox, "no-sandbox", os.Geteuid() == 0, "start Chromium without its sandbox (default: when running as root, as in a container)")
	flag.Var(&o.Only, "only", "take only the shot with this name (repeatable)")
	flag.Parse()
	if flag.NArg() > 0 {
		return fmt.Errorf("unexpected argument %q", flag.Arg(0))
	}
	if o.Out == "" {
		o.Out = filepath.Dir(o.Shots)
	}
	list, err := loadShots(o.Shots)
	if err != nil {
		return err
	}
	if o.Chromium, err = findChromium(o.Chromium); err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	ui, err := startUI(ctx, o.Brig)
	if err != nil {
		return err
	}
	defer ui.stop()
	return takeShots(ctx, o, list, ui.url)
}

// options are the flags.
type options struct {
	Brig, Chromium, Shots, Out string
	NoSandbox                  bool
	Only                       names
}

// names is a repeatable string flag.
type names []string

func (n *names) String() string     { return strings.Join(*n, ",") }
func (n *names) Set(v string) error { *n = append(*n, v); return nil }

// findChromium returns the browser to drive.
func findChromium(given string) (string, error) {
	if given == "" {
		given = os.Getenv("CHROMIUM")
	}
	if given != "" {
		return given, nil
	}
	for _, name := range []string{"chromium", "chromium-browser", "google-chrome", "google-chrome-stable"} {
		if p, err := exec.LookPath(name); err == nil {
			return p, nil
		}
	}
	return "", errors.New("found no Chromium; install one, or name it with -chromium or $CHROMIUM")
}

// ui is a running brig ui --demo --still.
type ui struct {
	cmd *exec.Cmd
	url string
}

// startUI starts the still demo on a free port and returns when it prints
// its URL.
func startUI(ctx context.Context, brig string) (*ui, error) {
	cmd := exec.CommandContext(ctx, brig, "ui", "--demo", "--still", "--no-open")
	cmd.Cancel = func() error { return cmd.Process.Signal(os.Interrupt) }
	cmd.WaitDelay = 5 * time.Second
	cmd.Stderr = os.Stderr
	out, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("starting %s ui: %w", brig, err)
	}
	u := &ui{cmd: cmd}
	found := make(chan string, 1)
	go func() {
		sc := bufio.NewScanner(out)
		for sc.Scan() {
			if _, url, ok := strings.Cut(sc.Text(), "brig ui: "); ok {
				found <- url
				break
			}
		}
		_, _ = io.Copy(io.Discard, out)
	}()
	select {
	case u.url = <-found:
		return u, nil
	case <-time.After(30 * time.Second):
		u.stop()
		return nil, errors.New("brig ui printed no URL")
	case <-ctx.Done():
		u.stop()
		return nil, ctx.Err()
	}
}

// stop interrupts brig ui and waits for it.
func (u *ui) stop() {
	_ = u.cmd.Process.Signal(os.Interrupt)
	_ = u.cmd.Wait()
}
