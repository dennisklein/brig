// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"errors"
	"fmt"

	"github.com/dennisklein/brig/internal/image"
	"github.com/dennisklein/brig/internal/vm"
)

// Exit statuses that tell scripts why brig failed. docs/exit-codes.md lists
// them; keep it in step. 2 is left out: brig policy diff uses it for an
// outcome.
const (
	exitFailure    = 1
	exitNotFound   = 3
	exitExists     = 4
	exitNotReady   = 5
	exitSyncFailed = 6
)

// exitReasons names the statuses in JSON errors.
var exitReasons = map[int]string{
	exitFailure:    "failed",
	exitNotFound:   "not_found",
	exitExists:     "exists",
	exitNotReady:   "not_ready",
	exitSyncFailed: "sync_failed",
}

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
