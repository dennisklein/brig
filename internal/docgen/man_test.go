// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestManPages(t *testing.T) {
	t.Setenv("SOURCE_DATE_EPOCH", "1760000000")
	dir := t.TempDir()
	if err := writeMan(dir, "v1.2.3"); err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]string{
		"brig.1":                 `.TH "BRIG" "1" "Oct 2025" "brig v1.2.3"`,
		"brig-sandbox-create.1":  "Create an OpenShell sandbox",
		"brig-policy-allow.1":    "allow_HOST_PORT", // underscores are not emphasis
		"brig-concepts.7":        "Fence 2, the network profile",
		"brig-completion-bash.1": "bash",
		"brig-agent-init.1":      "DIR/agents/NAME/",
	} {
		data, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Error(err)
			continue
		}
		if !strings.Contains(strings.ReplaceAll(string(data), `\_`, "_"), want) {
			t.Errorf("%s lacks %q", name, want)
		}
	}
	for _, name := range []string{"brig-net-helper.1", "brig-help.1", "brig-topics.1"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err == nil {
			t.Errorf("there is a man page %s for a command that is hidden or only a help topic", name)
		}
	}
}
