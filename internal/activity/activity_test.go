// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

package activity

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/GSI-HPC/go-clikit/progress"

	"github.com/dennisklein/brig/internal/ui/store"
)

// logRun writes the event log of a brig command with clikit itself: the
// command span named command, and the work of work below it.
func logRun(t *testing.T, command string, work func(ctx context.Context) error) []byte {
	t.Helper()
	return logRunOn(t, "", command, work)
}

// logRunOn logs a command that names the VM it works on as its node.
func logRunOn(t *testing.T, vm, command string, work func(ctx context.Context) error) []byte {
	t.Helper()
	var out bytes.Buffer
	log := progress.NewLog(&out, progress.LogOptions{Program: "brig", Version: "v0.6.0"})
	bus := progress.NewBus(progress.BusOptions{Sinks: []progress.Sink{log}})
	ctx, span := progress.Start(progress.WithBus(context.Background(), bus), progress.KindCommand, command)
	var opts []progress.Option
	if vm != "" {
		opts = append(opts, progress.Node(vm))
	}
	span.End(work(ctx), opts...)
	bus.Close()
	if err := log.Close(); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

func step(ctx context.Context, name string, err error) {
	_, s := progress.Start(ctx, progress.KindStep, name)
	s.End(err)
}

func startDev(ctx context.Context) error {
	step(ctx, "Boot domain brig-dev", nil)
	sctx, s := progress.Start(ctx, progress.KindStep, "Apply OpenShell config directories")
	_, t := progress.Start(sctx, progress.KindTarget, "provider gitlab")
	t.Skip("no secret in keyring")
	_, h := progress.Start(sctx, progress.KindCall, "secret-tool", progress.WithFlags(progress.Hidden))
	h.End(nil)
	s.Skip("1 provider skipped")
	return nil
}

func failingStop(ctx context.Context) error {
	err := errors.New("guest did not shut down\x1b[2J")
	step(ctx, "Ask the guest to shut down", err)
	return err
}

func reduce(logs ...[]byte) []store.Run {
	red := NewReducer(200)
	readLines(bytes.NewReader(bytes.Join(logs, nil)), red, nil)
	return red.Runs()
}

func TestReducer(t *testing.T) {
	runs := reduce(logRun(t, "start dev", startDev), logRun(t, "stop dev", failingStop))
	if len(runs) != 2 {
		t.Fatalf("got %d runs", len(runs))
	}
	stop, start := runs[0], runs[1]
	if start.Title != "brig start dev" || start.Status != "ok" || !start.Done() || start.Version != "v0.6.0" {
		t.Errorf("start: %+v", start)
	}
	if len(start.Steps) != 2 || start.Steps[0].Name != "Boot domain brig-dev" || start.Steps[0].Status != "ok" {
		t.Fatalf("start steps: %+v", start.Steps)
	}
	apply := start.Steps[1]
	if apply.Status != "skipped" || apply.Err != "1 provider skipped" {
		t.Errorf("apply: %+v", apply)
	}
	// The hidden call is left out, as a terminal display leaves it out.
	if len(apply.Steps) != 1 || apply.Steps[0].Name != "provider gitlab" || apply.Steps[0].Err != "no secret in keyring" {
		t.Errorf("apply steps: %+v", apply.Steps)
	}
	if stop.Title != "brig stop dev" || stop.Status != "failed" {
		t.Errorf("stop: %+v", stop)
	}
	if strings.ContainsRune(stop.Err, 0x1b) || strings.ContainsRune(stop.Steps[0].Err, 0x1b) {
		t.Errorf("control characters kept: %q", stop.Err)
	}
}

func TestReducerTrace(t *testing.T) {
	const traceparent = "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"
	tc, ok := progress.ParseTraceContext(traceparent, "")
	if !ok {
		t.Fatal("trace context does not parse")
	}
	var out bytes.Buffer
	log := progress.NewLog(&out, progress.LogOptions{Program: "brig"})
	bus := progress.NewBus(progress.BusOptions{Sinks: []progress.Sink{log}, Trace: tc})
	_, span := progress.Start(progress.WithBus(context.Background(), bus), progress.KindCommand, "start dev")
	span.End(nil)
	bus.Close()
	if err := log.Close(); err != nil {
		t.Fatal(err)
	}
	runs := reduce(out.Bytes())
	if len(runs) != 1 || runs[0].Trace != "4bf92f3577b34da6a3ce929d0e0e4736" {
		t.Errorf("runs: %+v", runs)
	}
}

func TestReducerInterleavedAndUnfinished(t *testing.T) {
	a := bytes.SplitAfter(logRun(t, "start dev", startDev), []byte("\n"))
	b := bytes.SplitAfter(logRun(t, "stop dev", failingStop), []byte("\n"))
	var mixed [][]byte
	for i := 0; i < max(len(a), len(b)); i++ {
		if i < len(a) {
			mixed = append(mixed, a[i])
		}
		if i < len(b) {
			mixed = append(mixed, b[i])
		}
	}
	if runs := reduce(mixed...); len(runs) != 2 || runs[0].Title != "brig stop dev" {
		t.Errorf("interleaved: %+v", runs)
	}
	// Without its last lines a run is still going.
	lines := bytes.SplitAfter(logRun(t, "start dev", startDev), []byte("\n"))
	runs := reduce(bytes.Join(lines[:len(lines)-3], nil))
	if len(runs) != 1 || runs[0].Done() {
		t.Errorf("unfinished: %+v", runs)
	}
}

func TestReducerSkipsJunk(t *testing.T) {
	junk := []byte("not json\n{\"v\":2,\"run\":\"x\",\"type\":\"start\",\"span\":\"1\",\"kind\":\"command\",\"name\":\"future\"}\n{}\n")
	runs := reduce(junk, logRun(t, "start dev", startDev))
	if len(runs) != 1 || runs[0].Title != "brig start dev" {
		t.Errorf("got %+v", runs)
	}
}

func TestReducerKeepsNewest(t *testing.T) {
	const maxRuns = 20
	red := NewReducer(maxRuns)
	for range maxRuns + 5 {
		// Each log draws a run of its own.
		readLines(bytes.NewReader(logRun(t, "list", func(context.Context) error { return nil })), red, nil)
	}
	if n := len(red.Runs()); n != maxRuns {
		t.Errorf("kept %d runs, want %d", n, maxRuns)
	}
}

func TestTail(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "activity.jsonl")
	tl := NewTail(path, NewReducer(200))
	if !tl.Poll() || len(tl.red.Runs()) != 0 {
		t.Fatal("a missing log is not an empty one")
	}
	if tl.Poll() {
		t.Error("a missing log changed")
	}

	start := logRun(t, "start dev", startDev)
	half := bytes.LastIndexByte(start[:len(start)-1], '\n') + 10
	if err := os.WriteFile(path, start[:half], 0o600); err != nil {
		t.Fatal(err)
	}
	if !tl.Poll() || len(tl.red.Runs()) != 1 || tl.red.Runs()[0].Done() {
		t.Fatalf("first part: %+v", tl.red.Runs())
	}
	appendFile(t, path, start[half:])
	if !tl.Poll() || !tl.red.Runs()[0].Done() {
		t.Fatalf("rest: %+v", tl.red.Runs())
	}
	if tl.Poll() {
		t.Error("an unchanged log changed")
	}

	// Rotation: the log moves to activity.jsonl.1 and starts again.
	if err := os.Rename(path, path+".1"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, logRun(t, "stop dev", failingStop), 0o600); err != nil {
		t.Fatal(err)
	}
	if !tl.Poll() || len(tl.red.Runs()) != 2 {
		t.Fatalf("after rotation: %+v", tl.red.Runs())
	}

	// A new reader finds both, the rotated copy first.
	fresh := NewTail(path, NewReducer(200))
	fresh.Poll()
	if runs := fresh.red.Runs(); len(runs) != 2 || runs[0].Title != "brig stop dev" {
		t.Errorf("fresh reader: %+v", runs)
	}
}

// A writer that has the log open when it is rotated goes on writing to the
// rotated copy. A poll after the rotation reads that too.
func TestTailFinishesRotatedLog(t *testing.T) {
	path := filepath.Join(t.TempDir(), "activity.jsonl")
	start := logRun(t, "start dev", startDev)
	half := bytes.LastIndexByte(start[:len(start)-1], '\n') + 10
	if err := os.WriteFile(path, start[:half], 0o600); err != nil {
		t.Fatal(err)
	}
	tl := NewTail(path, NewReducer(200))
	if !tl.Poll() || len(tl.red.Runs()) != 1 || tl.red.Runs()[0].Done() {
		t.Fatalf("first part: %+v", tl.red.Runs())
	}

	// Rotate, then the old writer ends its run in the old file.
	if err := os.Rename(path, path+".1"); err != nil {
		t.Fatal(err)
	}
	appendFile(t, path+".1", start[half:])
	if err := os.WriteFile(path, logRun(t, "stop dev", failingStop), 0o600); err != nil {
		t.Fatal(err)
	}
	if !tl.Poll() {
		t.Fatal("no change after rotation")
	}
	runs := tl.red.Runs()
	if len(runs) != 2 || runs[0].Title != "brig stop dev" || runs[1].Title != "brig start dev" || !runs[1].Done() {
		t.Errorf("after rotation: %+v", runs)
	}
}

func appendFile(t *testing.T, path string, data []byte) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestReadLinesSkipsLongLines(t *testing.T) {
	red := NewReducer(200)
	long := append(bytes.Repeat([]byte("x"), maxLine+10), '\n')
	n, partial := readLines(bytes.NewReader(append(long, logRun(t, "start dev", startDev)...)), red, nil)
	if len(red.Runs()) != 1 || len(partial) != 0 || n <= int64(len(long)) {
		t.Errorf("runs %d, partial %d, read %d", len(red.Runs()), len(partial), n)
	}
}

func TestReducerVM(t *testing.T) {
	runs := reduce(logRunOn(t, "dev", "policy allow dev", func(context.Context) error { return nil }), logRun(t, "list", func(context.Context) error { return nil }))
	if len(runs) != 2 || runs[1].VM != "dev" || runs[0].VM != "" {
		t.Errorf("runs: %+v", runs)
	}
}
