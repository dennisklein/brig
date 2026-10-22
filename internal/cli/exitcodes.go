// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/dennisklein/brig/internal/image"
	"github.com/dennisklein/brig/internal/vm"
)

// Exit statuses that tell scripts why brig failed. exitCodes lists them for
// brig --help and the reference. 7 is left out: brig policy diff uses it for
// an outcome.
const (
	exitFailure    = 1
	exitNotFound   = 3
	exitExists     = 4
	exitNotReady   = 5
	exitSyncFailed = 6
)

// ExitCode is an exit status of brig and what it tells a script.
type ExitCode struct {
	Code int
	// Reason names the status in JSON errors; success has none.
	Reason string
	// Short completes "Exit status: CODE ..." in brig --help.
	Short string
	// Meaning says in a sentence or two when brig exits with it.
	Meaning string
}

// exitCodes is the one list of the statuses that brig itself exits with.
// brig --help, the JSON errors and the reference all render it. Success
// comes first and the catch-all last, as brig --help lists them.
var exitCodes = []ExitCode{
	{0, "", "success", "The command did what you asked."},
	{exitNotFound, "not_found", "not found", "The VM, image or other object that you named does not exist."},
	{exitExists, "exists", "exists already", "The VM or other object that you asked to create exists already."},
	{exitNotReady, "not_ready", "not ready (VM, gateway or sandbox)", "The VM does not run, its gateway does not answer, or the sandbox is not ready."},
	{exitSyncFailed, "sync_failed", "OpenShell config sync failed", "Applying the OpenShell config directories to the VM's gateway failed. The VM itself did what you asked."},
	{exitFailure, "failed", "any other failure", "Any other failure. The message on standard error says which."},
}

// ExitCodes returns the exit statuses that brig itself exits with, success
// first.
func ExitCodes() []ExitCode { return slices.Clone(exitCodes) }

// exitReasons names the failure statuses in JSON errors.
var exitReasons = func() map[int]string {
	m := map[int]string{}
	for _, c := range exitCodes {
		if c.Reason != "" {
			m[c.Code] = c.Reason
		}
	}
	return m
}()

// exitStatusHelp lists the exit statuses for brig --help, wrapped like the
// text around it.
func exitStatusHelp() string {
	parts := make([]string, len(exitCodes))
	for i, c := range exitCodes {
		parts[i] = fmt.Sprintf("%d %s", c.Code, c.Short)
	}
	var b strings.Builder
	line := ""
	for _, w := range strings.Fields("Exit status: " + strings.Join(parts, ", ") + ".") {
		if line != "" && len(line)+1+len(w) > helpWidth {
			b.WriteString(line + "\n")
			line = ""
		}
		if line != "" {
			line += " "
		}
		line += w
	}
	return b.String() + line
}

// helpWidth is the width that brig --help wraps its texts at.
const helpWidth = 75

// codedError gives err an exit status without changing its message.
type codedError struct {
	code int
	err  error
}

func (e codedError) Error() string { return e.err.Error() }
func (e codedError) Unwrap() error { return e.err }

func withCode(code int, err error) error {
	if err == nil {
		return nil
	}
	return codedError{code, err}
}

// alreadyExists marks err as about something that exists already.
func alreadyExists(err error) error { return withCode(exitExists, err) }

// notReady marks err as about a VM that does not run, a gateway that does
// not answer, or a sandbox that is not ready.
func notReady(err error) error { return withCode(exitNotReady, err) }

// syncFailed marks err as a failed sync of OpenShell config directories.
func syncFailed(err error) error { return withCode(exitSyncFailed, err) }

// notReadyf formats a notReady error.
func notReadyf(format string, args ...any) error { return notReady(fmt.Errorf(format, args...)) }

// exitStatus returns the exit status for the error that a command failed with.
func exitStatus(err error) int {
	var c codedError
	switch {
	case errors.As(err, &c):
		return c.code
	case errors.Is(err, vm.ErrNotFound), errors.Is(err, image.ErrNotFound):
		return exitNotFound
	}
	return exitFailure
}
