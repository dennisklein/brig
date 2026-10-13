// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

package collect

import (
	"context"
	"time"

	"github.com/dennisklein/brig/internal/activity"
	"github.com/dennisklein/brig/internal/ui/store"
)

const (
	// maxRuns is how many runs Activity keeps, the newest.
	maxRuns = 200
	// activityEvery is how often the log is checked for new lines.
	activityEvery = time.Second
)

// Activity follows the event log at path and keeps the store's runs up to
// date, until ctx ends. It reads the rotated copy, path.1, first, so that
// the runs before the last rotation show too. A missing log is an empty
// one.
func Activity(ctx context.Context, st *store.Store, path string) {
	red := activity.NewReducer(maxRuns)
	t := activity.NewTail(path, red)
	tick := time.NewTicker(activityEvery)
	defer tick.Stop()
	for {
		if t.Poll() {
			st.SetActivity(red.Runs())
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}
