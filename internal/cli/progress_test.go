// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"testing"

	"github.com/GSI-HPC/go-clikit/progress/display"
)

func TestTheme(t *testing.T) {
	tests := []struct {
		name       string
		onTerminal bool
		env        map[string]string
		want       display.Theme
	}{
		{"no terminal", false, map[string]string{"TERM": "xterm-256color"}, display.Theme{}},
		{"dumb terminal", true, map[string]string{"TERM": "dumb"}, display.Theme{}},
		{"NO_COLOR", true, map[string]string{"TERM": "xterm-256color", "NO_COLOR": "1"}, display.Classic.In(display.NoColours)},
		{"256 colours", true, map[string]string{"TERM": "xterm-256color"}, display.Classic.In(display.Colours256)},
		{"truecolor", true, map[string]string{"TERM": "xterm", "COLORTERM": "truecolor"}, display.Classic.In(display.Colours256)},
		{"16 colours", true, map[string]string{"TERM": "linux"}, display.Classic.In(display.Colours16)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := theme(tt.onTerminal, func(k string) string { return tt.env[k] }); got != tt.want {
				t.Errorf("theme() = %+v, want %+v", got, tt.want)
			}
		})
	}
}
