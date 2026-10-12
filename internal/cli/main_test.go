// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"os"
	"testing"
)

// TestMain keeps the tests' commands from writing the activity log into the
// real state directory.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "brig-cli-test-state")
	if err != nil {
		panic(err)
	}
	_ = os.Setenv("XDG_STATE_HOME", dir)
	code := m.Run()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}
