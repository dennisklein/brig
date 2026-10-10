// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"context"
	"os"
	"strings"
	"sync/atomic"

	"github.com/GSI-HPC/go-clikit/progress"
	"github.com/GSI-HPC/go-clikit/progress/cliprogress"
	"github.com/GSI-HPC/go-clikit/termtext"
	"github.com/spf13/cobra"
	"golang.org/x/sys/unix"

	"github.com/dennisklein/brig/internal/version"
)

const (
	progressVar    = "BRIG_PROGRESS"
	progressLogVar = "BRIG_PROGRESS_LOG"
	// noProgress marks commands that own the terminal or must keep standard
	// output and error untouched.
	noProgress = "brig.no-progress"
)

// progressFlags are the values of the global progress flags.
type progressFlags struct{ mode, log string }

// progressRun is the progress of one invocation: the display, and the span of
// the command.
type progressRun struct {
	run  atomic.Pointer[cliprogress.Run] // atomic for tests that draw by hand
	span *progress.Span
	// options lets a test change what the display is drawn on.
	options func(*cliprogress.Options)
}

// runKey is the context key of the *cliprogress.Run of a command.
type runKey struct{}

// progressMode returns the mode of the display that shows the command whose
// context is ctx, or cliprogress.ModeNone if there is none.
func progressMode(ctx context.Context) cliprogress.Mode {
	if r, ok := ctx.Value(runKey{}).(*cliprogress.Run); ok {
		return r.Mode()
	}
	return cliprogress.ModeNone
}

// register adds the progress flags to root.
func (f *progressFlags) register(root *cobra.Command) {
	pf := root.PersistentFlags()
	pf.StringVar(&f.mode, "progress", "auto", "how to show progress: auto, tty, counter, plain, none (env "+progressVar+")")
	pf.StringVar(&f.log, "progress-log", "", "append a JSON event log of the work to this file (env "+progressLogVar+")")
	_ = root.RegisterFlagCompletionFunc("progress", func(*cobra.Command, []string, string) ([]string, cobra.ShellCompDirective) {
		var words []string
		for _, m := range cliprogress.Modes() {
			words = append(words, m.String())
		}
		return words, cobra.ShellCompDirectiveNoFileComp
	})
}

// skipsProgress reports whether cmd or a parent is marked noProgress.
func skipsProgress(cmd *cobra.Command) bool {
	for c := cmd; c != nil; c = c.Parent() {
		if _, ok := c.Annotations[noProgress]; ok {
			return true
		}
	}
	return false
}

// isTerminal reports whether f is a terminal.
func isTerminal(f *os.File) bool {
	_, err := unix.IoctlGetTermios(int(f.Fd()), unix.TCGETS)
	return err == nil
}

// commandName names cmd for its span, such as "start dev".
func commandName(cmd *cobra.Command, args []string) string {
	name := strings.TrimPrefix(cmd.CommandPath(), cmd.Root().Name()+" ")
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		name += " " + termtext.Escape(args[0])
	}
	return name
}

// start runs as the root's PersistentPreRunE.
func (p *progressRun) start(cmd *cobra.Command, args []string, f *progressFlags) error {
	if skipsProgress(cmd) {
		return nil
	}
	o := cliprogress.Options{
		Program: "brig",
		Mode: cliprogress.Setting{Flag: "--progress", Given: cmd.Flags().Changed("progress"),
			Value: f.mode, Variable: progressVar, Env: os.Getenv(progressVar)},
		Log: cliprogress.Setting{Flag: "--progress-log", Given: cmd.Flags().Changed("progress-log"),
			Value: f.log, Variable: progressLogVar, Env: os.Getenv(progressLogVar)},
		LogOptions: progress.LogOptions{Version: version.Version()},
		Stderr:     os.Stderr,
		OnTerminal: isTerminal(os.Stderr),
		Dumb:       os.Getenv("TERM") == "dumb",
		IntoPipe:   cliprogress.IsPipe(os.Stdout),
		Size:       func() (int, int, error) { return cliprogress.TerminalSize(os.Stderr) },
		Foreground: func() bool { return cliprogress.InForeground(os.Stderr) },
		ASCII:      !cliprogress.UTF8Locale(os.Getenv),
	}
	if p.options != nil {
		p.options(&o)
	}
	run, err := cliprogress.Start(cmd.Context(), o)
	if err != nil {
		return err
	}
	ctx, span := progress.Start(run.Context(), progress.KindCommand, commandName(cmd, args))
	cmd.SetContext(context.WithValue(ctx, runKey{}, run))
	cmd.SetErr(run.Writer(cmd.ErrOrStderr()))
	// Only a standard output on the terminal can tear the display.
	if isTerminal(os.Stdout) {
		cmd.SetOut(run.Writer(cmd.OutOrStdout()))
	}
	p.run.Store(run)
	p.span = span
	return nil
}

// end ends the command with err and stops the display.
func (p *progressRun) end(err error) {
	run := p.run.Load()
	if run == nil {
		return
	}
	p.span.End(err)
	run.Finish(err != nil)
}
