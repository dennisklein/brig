// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"bytes"
	"strings"
	"testing"

	"github.com/GSI-HPC/go-clikit/progress/cliprogress"
)

// runSplit runs brig with progress drawn on a buffer, and returns standard
// output and error apart, and whether a display was started.
func runSplit(t *testing.T, args ...string) (stdout, stderr string, started bool, err error) {
	t.Helper()
	p := &progressRun{options: func(o *cliprogress.Options) {
		o.Stderr, o.OnTerminal = new(bytes.Buffer), false
	}}
	cmd := newRootCmd(p)
	var out, errb bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errb)
	cmd.SetArgs(args)
	err = cmd.Execute()
	p.end(err)
	return out.String(), errb.String(), p.run.Load() != nil, err
}

// TestNoProgressKeepsStdoutClean checks that shell-init, env and use start no
// display and print what they print without one, however progress is asked
// for: use's output is evaluated by the shell.
func TestNoProgressKeepsStdoutClean(t *testing.T) {
	a := testApp(t)
	v := testVM(t, a, "dev")
	v.OpenShellConfigs = []string{configDir(t)}
	if err := a.vms.Save(v); err != nil {
		t.Fatal(err)
	}
	t.Setenv(shellIntegrationEnv, "sh")
	for _, args := range [][]string{
		{"shell-init", "bash"},
		{"env", "dev", "--shell", "sh"},
		{"env", "--unset", "--shell", "sh"},
		{"use", "dev"},
		{"use", "--unset"},
		{"use"}, // a usage error, which must stay on standard error
	} {
		wantOut, wantErr, started, wantFail := runSplit(t, args...)
		if started || (wantFail == nil) != (wantOut != "") {
			t.Errorf("%q: stdout %q, err %v, display started %v", args, wantOut, wantFail, started)
		}
		for _, ask := range [][]string{{"--progress", "plain"}, {"--progress", "tty"}, {"--progress", "counter"}} {
			out, errOut, started, err := runSplit(t, append(append([]string{}, args...), ask...)...)
			if started || out != wantOut || errOut != wantErr || (err == nil) != (wantFail == nil) {
				t.Errorf("%q %q: stdout %q stderr %q started %v err %v; want stdout %q stderr %q err %v",
					args, ask, out, errOut, started, err, wantOut, wantErr, wantFail)
			}
		}
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			t.Setenv(progressVar, "plain")
			out, errOut, started, err := runSplit(t, args...)
			if started || out != wantOut || errOut != wantErr || (err == nil) != (wantFail == nil) {
				t.Errorf("%q with %s=plain: stdout %q stderr %q started %v err %v; want stdout %q stderr %q err %v",
					args, progressVar, out, errOut, started, err, wantOut, wantErr, wantFail)
			}
		})
	}
}
