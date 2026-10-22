// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"testing"

	"github.com/GSI-HPC/go-clikit/cobratree"
	"github.com/GSI-HPC/go-clikit/progress"
	"github.com/GSI-HPC/go-clikit/progress/cliprogress"
	"github.com/spf13/cobra"
)

// A child that an interrupt killed ends its span canceled; every other
// failure after the interrupt stays the target's.
func TestProgressClass(t *testing.T) {
	killed := exec.Command("sh", "-c", "kill -INT $$").Run()
	if ee, ok := errors.AsType[*exec.ExitError](killed); !ok || ee.ExitCode() != -1 {
		t.Fatalf("the test child did not die of a signal: %v", killed)
	}
	failed := exec.Command("sh", "-c", "exit 3").Run()

	interrupted, stop := context.WithCancel(context.Background())
	stop()
	for _, tt := range []struct {
		name string
		ctx  context.Context
		err  error
		want progress.Class
	}{
		{"killed child after the interrupt", interrupted, fmt.Errorf("podman: %w", killed), progress.ClassCanceled},
		{"killed child without an interrupt", context.Background(), killed, progress.ClassNone},
		{"failed child after the interrupt", interrupted, failed, progress.ClassNone},
		{"usage error", context.Background(), cobratree.Usagef("takes no argument"), progress.ClassUsage},
		{"other failure after the interrupt", interrupted, errors.New("rollback failed"), progress.ClassNone},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := progressClass(tt.ctx)(tt.err); got != tt.want {
				t.Errorf("progressClass = %v, want %v", got, tt.want)
			}
			// The kit puts the fallback behind its own rules.
			if got := progress.Classify(tt.err, progressClass(tt.ctx)); tt.want == progress.ClassCanceled && got != progress.ClassCanceled {
				t.Errorf("Classify = %v, want canceled", got)
			}
		})
	}
}

// The command line's context is the one that the classes follow, so that an
// interrupted command ends its run canceled in the event log.
func TestStartSetsClassify(t *testing.T) {
	var classify func(error) progress.Class
	p := &progressRun{options: func(o *cliprogress.Options) {
		classify = o.Classify
		o.Mode.Value, o.Log = "none", cliprogress.Setting{}
	}}
	ctx, interrupt := context.WithCancel(context.Background())
	cmd := &cobra.Command{Use: "x"}
	cmd.SetContext(ctx)
	f := &progressFlags{}
	cmd.Flags().String("progress", "", "")
	cmd.Flags().String("progress-log", "", "")
	if err := p.start(cmd, nil, f); err != nil {
		t.Fatal(err)
	}
	defer p.end(nil)
	if classify == nil {
		t.Fatal("start set no Classify")
	}
	killed := exec.Command("sh", "-c", "kill -INT $$").Run()
	if classify(killed) != progress.ClassNone {
		t.Error("a killed child is canceled before the interrupt")
	}
	interrupt()
	if classify(killed) != progress.ClassCanceled {
		t.Error("a killed child is not canceled after the interrupt")
	}
}
