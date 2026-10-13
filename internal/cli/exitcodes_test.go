// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"testing"

	"github.com/spf13/cobra"
)

func TestExitStatusTellsFailuresApart(t *testing.T) {
	a := testApp(t)
	v := testVM(t, a, "dev")
	v.OpenShellConfigs = []string{configDir(t)}
	if err := a.vms.Save(v); err != nil {
		t.Fatal(err)
	}
	fakeOpenShell(t, fakeGuest(t, ""), `{"status":"not_configured"}`)
	for _, tc := range []struct {
		args []string
		want int
	}{
		{[]string{"show", "nosuch"}, exitNotFound},
		{[]string{"create", "dev"}, exitExists},
		{[]string{"sync", "dev"}, exitNotReady},
		{[]string{"show", "-o", "yaml"}, exitFailure},
	} {
		_, err := run(t, tc.args...)
		if got := exitStatus(err); got != tc.want {
			t.Errorf("brig %q: exit status %d (%v), want %d", tc.args, got, err, tc.want)
		}
	}
	// Wrapping keeps the status and the message.
	err := fmt.Errorf("start: %w", errors.Join(syncFailed(errors.New("provider x: failed")), errors.New("save")))
	if got := exitStatus(err); got != exitSyncFailed || err.Error() != "start: provider x: failed\nsave" {
		t.Errorf("exitStatus(%q) = %d", err, got)
	}
}

func TestReportPrintsJSONErrorsForJSONOutput(t *testing.T) {
	newCmd := func(args ...string) *cobra.Command {
		var format outputFormat
		cmd := &cobra.Command{Use: "x"}
		addOutputFlag(cmd, &format)
		if err := cmd.ParseFlags(args); err != nil {
			t.Fatal(err)
		}
		return cmd
	}
	for _, tc := range []struct {
		cmd      *cobra.Command
		printed  bool
		wantJSON bool
	}{
		{newCmd("-o", "json"), false, true},
		{newCmd("-o", "json"), true, false}, // the command printed its own JSON
		{newCmd(), false, false},
		{nil, false, false},
	} {
		var out, errOut bytes.Buffer
		code := report(tc.cmd, notReadyf("gateway brig-dev is down"), tc.printed, &out, &errOut)
		if code != exitNotReady || errOut.String() != "Error: gateway brig-dev is down\n" {
			t.Errorf("code %d, stderr %q", code, &errOut)
		}
		if !tc.wantJSON {
			if out.Len() > 0 {
				t.Errorf("stdout %q, want none", &out)
			}
			continue
		}
		var got jsonError
		if err := json.Unmarshal(out.Bytes(), &got); err != nil {
			t.Fatalf("stdout %q: %v", &out, err)
		}
		if want := (jsonError{Error: "gateway brig-dev is down", Reason: "not_ready", ExitCode: exitNotReady}); got != want {
			t.Errorf("JSON error %+v, want %+v", got, want)
		}
	}
	if code := report(nil, nil, false, nil, nil); code != 0 {
		t.Errorf("no error: exit status %d", code)
	}
}
