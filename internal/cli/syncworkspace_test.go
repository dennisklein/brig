// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dennisklein/brig/internal/ocsync"
)

// TestSyncWorkspaceFlag checks that --workspace limits a sync to the named
// workspaces, which brig creates on the gateway when it lacks them.
func TestSyncWorkspaceFlag(t *testing.T) {
	a := testApp(t)
	v := testVM(t, a, "dev")
	dir := configDir(t)
	team := filepath.Join(dir, "workspaces", "team")
	writeTestFile(t, filepath.Join(team, "providers", "gl.yaml"), "name: gl\ntype: github\ncredentials:\n  T:\n    secret_tool:\n      lookup: [service, gitlab]\n", 0o600)
	v.OpenShellConfigs = []string{dir}
	if err := a.vms.Save(v); err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	log := filepath.Join(bin, "log")
	writeTestFile(t, filepath.Join(bin, "openshell"), `#!/bin/sh
echo "$*" >>'`+log+`'
case "$*" in
*"status -o json") echo '{"status": "connected", "version": "0.1.2"}' ;;
*"workspace list"*) echo '{"workspaces":[{"name":"default"}],"next_page_token":""}' ;;
*"profile list -o json") echo '[{"id": "github", "scope": "platform"}]' ;;
*"provider list -o json") echo '{"providers": [], "next_page_token": ""}' ;;
esac
`, 0o755)
	tool := filepath.Join(bin, "my-secret-tool")
	writeTestFile(t, tool, "#!/bin/sh\nprintf 'tok\\n'\n", 0o755)
	writeTestFile(t, a.dirs.ConfigFile(), "openshell:\n  secret_tool: "+tool+"\n", 0o600)
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

	out, err := run(t, "sync", "dev", "--workspace", "team")
	if err != nil {
		t.Fatalf("sync: %v\n%s", err, out)
	}
	argv, _ := os.ReadFile(log)
	for _, want := range []string{
		"-g brig-dev workspace create --name team\n",
		"-g brig-dev --workspace team provider create --name gl --type github --credential T\n",
	} {
		if !strings.Contains(string(argv), want) {
			t.Errorf("openshell calls lack %q:\n%s", want, argv)
		}
	}
	if strings.Contains(string(argv), "--workspace default") {
		t.Errorf("the default workspace was synced:\n%s", argv)
	}
	st, err := ocsync.LoadState(a.vmFile(v, ocsync.StateFile))
	if err != nil || !st.Workspaces["team"].Created || st.Workspaces["team"].Providers["gl"].Type != "github" || st.Workspaces["default"] != nil {
		t.Errorf("state %+v, %v", st, err)
	}

	if out, err := run(t, "sync", "dev", "-w", "nosuch"); err == nil || !strings.Contains(out+err.Error(), "workspace nosuch is not defined") {
		t.Errorf("unknown workspace: %v\n%s", err, out)
	}

	// BRIG_WORKSPACE is the default of -w.
	t.Setenv(workspaceEnv, "nosuch")
	if out, err := run(t, "sync", "dev"); err == nil || !strings.Contains(out+err.Error(), "workspace nosuch is not defined") {
		t.Errorf("sync in BRIG_WORKSPACE=nosuch: %v\n%s", err, out)
	}
	if out, err := run(t, "sync", "dev", "-w", "team"); err != nil {
		t.Errorf("-w over BRIG_WORKSPACE: %v\n%s", err, out)
	}
	t.Setenv(workspaceEnv, "Not_Valid")
	if _, err := run(t, "sync", "dev"); err == nil || !strings.Contains(err.Error(), workspaceEnv+": invalid workspace name") {
		t.Errorf("sync in an invalid BRIG_WORKSPACE: %v", err)
	}
}
