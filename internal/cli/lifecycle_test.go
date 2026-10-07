// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestWaitForWrapsLastError(t *testing.T) {
	sentinel := errors.New("not yet")
	err := waitFor(context.Background(), 10*time.Millisecond, func(context.Context) error { return sentinel })
	if !errors.Is(err, sentinel) || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("waitFor() = %v, want it to wrap both the last error and the deadline", err)
	}
}
