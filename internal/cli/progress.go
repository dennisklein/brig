// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"strings"
	"sync/atomic"

	"github.com/GSI-HPC/go-clikit/cobratree"
	"github.com/GSI-HPC/go-clikit/progress"
	"github.com/GSI-HPC/go-clikit/progress/cliprogress"
	"github.com/GSI-HPC/go-clikit/progress/display"
	"github.com/GSI-HPC/go-clikit/termtext"
	"github.com/spf13/cobra"
	"golang.org/x/sys/unix"

	"github.com/dennisklein/brig/internal/paths"
	"github.com/dennisklein/brig/internal/version"
)

const (
	progressVar    = "BRIG_PROGRESS"
	progressLogVar = "BRIG_PROGRESS_LOG"
	// noProgress marks commands that own the terminal or must keep standard
	// output and error untouched.
	noProgress = "brig.no-progress"
	// defaultLogHint says in the help where the log goes by default.
	defaultLogHint = "$XDG_STATE_HOME/brig/activity.jsonl"
)

// progressFlags are the values of the global progress flags.
type progressFlags struct{ mode, log string }

// progressRun is the progress of one invocation: the display, and the span of
// the command.
type progressRun struct {
	run  atomic.Pointer[cliprogress.Run] // atomic for tests that draw by hand
	span *progress.Span
	// vm is the VM the command works on, once it named one.
	vm string
	// options lets a test change what the display is drawn on.
	options func(*cliprogress.Options)
}

// runKey is the context key of the *cliprogress.Run of a command.
type runKey struct{}

// vmKey is the context key of where a command records the VM it works on.
type vmKey struct{}

// recordVM records that the command whose context is ctx works on the VM
// called name. The event log names it as the command's node, which tells
// brig audit which VM's history the command belongs to.
func recordVM(ctx context.Context, name string) {
	if vm, ok := ctx.Value(vmKey{}).(*string); ok {
		*vm = name
	}
}

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
	pf.StringVar(&f.log, "progress-log", "", "append a JSON event log of the work to this file, by default "+defaultLogHint+"; empty for none (env "+progressLogVar+")")
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

// theme returns the Classic theme drawn in as many colours as the terminal
// behind standard error shows. Where standard error is no terminal, or a dumb
// one, it is the zero Theme: plain lines for a log want no escape code.
func theme(onTerminal bool, getenv func(string) string) display.Theme {
	if !onTerminal || getenv("TERM") == "dumb" {
		return display.Theme{}
	}
	switch {
	case getenv("NO_COLOR") != "":
		return display.Classic.In(display.NoColours)
	case getenv("COLORTERM") != "" || strings.Contains(getenv("TERM"), "256"):
		return display.Classic.In(display.Colours256)
	}
	return display.Classic.In(display.Colours16)
}

// commandName names cmd for its span, such as "start dev".
func commandName(cmd *cobra.Command, args []string) string {
	name := strings.TrimPrefix(cmd.CommandPath(), cmd.Root().Name()+" ")
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		name += " " + termtext.Escape(args[0])
	}
	return name
}

// inSpan runs fn in a span of kind k called name, and ends the span with the
// error of fn.
func inSpan(ctx context.Context, k progress.Kind, name string, fn func(context.Context) error) error {
	ctx, span := progress.Start(ctx, k, name)
	err := fn(ctx)
	span.End(err)
	return err
}

// inOptionalSpan is inSpan for work whose failure the command goes on from,
// with a warning: the span then ends skipped, with the error as its reason.
// An error that comes with the end of ctx, such as an interrupt, still ends
// it as it is.
func inOptionalSpan(ctx context.Context, k progress.Kind, name string, fn func(context.Context) error) error {
	ctx2, span := progress.Start(ctx, k, name)
	err := fn(ctx2)
	if err != nil && ctx.Err() == nil {
		span.Skip(err.Error())
		return err
	}
	span.End(err)
	return err
}

// logSetting returns where the event log goes: the flag's file, else the
// variable's, else the activity log in the state directory. An empty flag or
// variable, which is given but names no file, turns the log off.
func logSetting(cmd *cobra.Command, f *progressFlags) cliprogress.Setting {
	s := cliprogress.Setting{
		Flag: "--progress-log", Given: cmd.Flags().Changed("progress-log"),
		Value: f.log, Variable: progressLogVar,
	}
	env, set := os.LookupEnv(progressLogVar)
	s.Env = env
	if !s.Given && !set {
		if dirs, err := paths.Default(); err == nil {
			s.Env, s.Variable = defaultActivityLog(dirs), activityLogName
		}
	}
	return s
}

// inheritedTrace returns the trace context that the caller handed on in
// TRACEPARENT and TRACESTATE, so that the event log tells which run belongs
// to the caller's task. It takes both out of the environment, since brig
// hands none on to the programs it starts.
func inheritedTrace() progress.TraceContext {
	tc, _ := progress.ParseTraceContext(os.Getenv("TRACEPARENT"), os.Getenv("TRACESTATE"))
	_ = os.Unsetenv("TRACEPARENT")
	_ = os.Unsetenv("TRACESTATE")
	return tc
}

// progressClass returns the fallback rule of the classes the spans of a
// command end with, for an error that the kit's own rules leave open. The
// kit tells an error that wraps context.Canceled by itself. A usage error
// is the command line's. A child that the
// interrupt killed fails with "signal: interrupt" alone, so once interrupt,
// the context of the command line, has ended, a failure of a child that a
// signal ended is canceled. Any other failure stays the target's: a
// rollback that fails after the interrupt is a failure, not work that the
// interrupt stopped.
func progressClass(interrupt context.Context) func(error) progress.Class {
	return func(err error) progress.Class {
		if cobratree.IsUsage(err) {
			return progress.ClassUsage
		}
		if ee, ok := errors.AsType[*exec.ExitError](err); ok && interrupt.Err() != nil && ee.ExitCode() == -1 {
			return progress.ClassCanceled
		}
		return progress.ClassNone
	}
}

// start runs as the root's PersistentPreRunE.
func (p *progressRun) start(cmd *cobra.Command, args []string, f *progressFlags) error {
	if skipsProgress(cmd) {
		return nil
	}
	o := cliprogress.Options{
		Program: "brig",
		Mode: cliprogress.Setting{
			Flag: "--progress", Given: cmd.Flags().Changed("progress"),
			Value: f.mode, Variable: progressVar, Env: os.Getenv(progressVar),
		},
		Log:        logSetting(cmd, f),
		LogOptions: progress.LogOptions{Version: version.Version()},
		Trace:      inheritedTrace,
		Stderr:     os.Stderr,
		OnTerminal: isTerminal(os.Stderr),
		Dumb:       os.Getenv("TERM") == "dumb",
		IntoPipe:   cliprogress.IsPipe(os.Stdout),
		Size:       func() (int, int, error) { return cliprogress.TerminalSize(os.Stderr) },
		Foreground: func() bool { return cliprogress.InForeground(os.Stderr) },
		ASCII:      !cliprogress.UTF8Locale(os.Getenv),
		Theme:      theme(isTerminal(os.Stderr), os.Getenv),
		Classify:   progressClass(cmd.Context()),
	}
	if p.options != nil {
		p.options(&o)
	}
	run, err := cliprogress.Start(cmd.Context(), o)
	if err != nil {
		return err
	}
	ctx, span := progress.Start(run.Context(), progress.KindCommand, commandName(cmd, args))
	cmd.SetContext(context.WithValue(context.WithValue(ctx, runKey{}, run), vmKey{}, &p.vm))
	// brig still asks before it deletes: confirmer sees the terminal under
	// the display's writer.
	stderr := cmd.ErrOrStderr()
	cmd.SetErr(overFile(run.Writer(stderr), stderr))
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
	var opts []progress.Option
	if p.vm != "" {
		opts = append(opts, progress.Node(p.vm))
	}
	p.span.End(err, opts...)
	run.Finish(err != nil)
}
