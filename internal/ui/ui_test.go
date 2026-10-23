// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

package ui

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/GSI-HPC/go-clikit/localweb/launch"
)

// Run returns when it fails after it started its goroutines. It used to
// wait for goroutines that only its own cancel could end.
func TestRunReturnsOnError(t *testing.T) {
	done := make(chan error, 1)
	go func() {
		var out bytes.Buffer
		done <- Run(context.Background(), Options{
			Demo: true, NoOpen: true, DevAssets: filepath.Join(t.TempDir(), "missing"),
			Stdout: &out, Stderr: &out,
		})
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Error("Run succeeded with assets that are missing")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not return after an error")
	}
}

// listener serves on a free port of 127.0.0.1 until the test ends, and
// returns that port.
func listener(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.Close() })
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			_ = c.Close()
		}
	}()
	return l.Addr().(*net.TCPAddr).Port
}

// runningUI records a brig ui of this user that runs, in a runtime
// directory it returns. The process of the record is the parent of the
// test, as the process of a second brig ui is another than the first.
func runningUI(t *testing.T, url string) (runtimeDir, brigDir string) {
	t.Helper()
	runtimeDir = t.TempDir()
	inst, err := launch.Acquire(t.Context(), runtimeDir, "brig")
	if err != nil {
		t.Fatal(err)
	}
	inst.Release()
	data, _ := json.Marshal(launch.Record{URL: url, PID: os.Getppid()})
	brigDir = inst.Dir()
	if err := os.WriteFile(filepath.Join(brigDir, launch.RecordFile), data, 0o600); err != nil {
		t.Fatal(err)
	}
	return runtimeDir, brigDir
}

type opened struct {
	name string
	args []string
}

// A second brig ui prints the address of the first and opens it through a
// private page, so that the token stands in no command line.
func TestRunOpensTheRunningUI(t *testing.T) {
	port := strconv.Itoa(listener(t))
	url := "http://127.0.0.1:" + port + "/?token=secret"
	runtimeDir, brigDir := runningUI(t, url)
	var got []opened
	var out bytes.Buffer
	err := Run(t.Context(), Options{
		RuntimeDir: runtimeDir, GOOS: "linux",
		Getenv: func(k string) string {
			if k == "DISPLAY" {
				return ":0"
			}
			return ""
		},
		Stdout: &out, Stderr: io.Discard,
		start: func(name string, args ...string) error {
			got = append(got, opened{name, args})
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if want := "brig ui is already running: " + url + "\n"; out.String() != want {
		t.Errorf("output %q, want %q", out.String(), want)
	}
	page := filepath.Join(brigDir, "open-"+port+".html")
	if len(got) != 1 || got[0].name != "xdg-open" || len(got[0].args) != 1 || got[0].args[0] != page {
		t.Fatalf("opener ran as %+v, want xdg-open with %s", got, page)
	}
	data, err := os.ReadFile(page)
	if err != nil || !strings.Contains(string(data), url) {
		t.Errorf("page: %q, %v; want the address in it", data, err)
	}
	if strings.Contains(strings.Join(got[0].args, " "), "secret") {
		t.Error("the token stands in the command line")
	}
}

// --no-open, a session under SSH and a process without a display open no
// browser.
func TestRunOpensNoBrowser(t *testing.T) {
	url := "http://127.0.0.1:" + strconv.Itoa(listener(t)) + "/?token=secret"
	for _, c := range []struct {
		name   string
		noOpen bool
		env    map[string]string
		say    string
	}{
		{"no-open", true, map[string]string{"DISPLAY": ":0"}, ""},
		{"ssh", false, map[string]string{"DISPLAY": ":0", "SSH_CONNECTION": "a b c d"}, "SSH"},
		{"no display", false, nil, "DISPLAY"},
	} {
		t.Run(c.name, func(t *testing.T) {
			runtimeDir, _ := runningUI(t, url)
			var out, errs bytes.Buffer
			err := Run(t.Context(), Options{
				RuntimeDir: runtimeDir, NoOpen: c.noOpen, GOOS: "linux",
				Getenv: func(k string) string { return c.env[k] },
				Stdout: &out, Stderr: &errs,
				start: func(name string, _ ...string) error {
					t.Errorf("%s ran", name)
					return nil
				},
			})
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(out.String(), "already running") {
				t.Errorf("output %q", out.String())
			}
			if !strings.Contains(errs.String(), c.say) || c.say == "" && errs.Len() > 0 {
				t.Errorf("stderr %q, want a note with %q", errs.String(), c.say)
			}
		})
	}
}
