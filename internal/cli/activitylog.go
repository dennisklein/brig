// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"errors"
	"os"
	"syscall"

	"github.com/dennisklein/brig/internal/paths"
)

// activityLogLimit is the size above which brig moves the activity log aside
// when a command starts. One old copy is kept, so the log stays under about
// twice this size.
const activityLogLimit = 8 << 20

// activityLogName is what the notes about a log that cannot be used call the
// default log.
const activityLogName = "the default activity log"

// defaultActivityLog prepares the activity log that a command writes when no
// flag or variable names a log, and returns its path, or "" if the state
// directory is unusable: the log is a convenience, so a read-only home must
// not make every command complain.
func defaultActivityLog(dirs paths.Dirs) string {
	if paths.EnsurePrivate(dirs.State) != nil {
		return ""
	}
	path := dirs.ActivityLog()
	_ = rotateLog(path, activityLogLimit)
	return path
}

// rotateLog moves the log at path to path+".1", replacing an older copy, if
// it is larger than limit bytes. Commands that run at once rotate it only
// once, and one that has the log open goes on writing to the old copy.
func rotateLog(path string, limit int64) error {
	if fi, err := os.Stat(path); err != nil || fi.Size() <= limit {
		return nil // missing, or small enough
	}
	lock, err := os.OpenFile(path+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	defer func() { _ = lock.Close() }()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		return err
	}
	// Another command may have rotated it while this one waited.
	if fi, err := os.Stat(path); err != nil || fi.Size() <= limit {
		return nil
	}
	if err := os.Rename(path, path+".1"); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}
