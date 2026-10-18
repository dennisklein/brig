// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

package ui

import (
	"bytes"
	"context"
	"path/filepath"
	"testing"
	"time"
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
