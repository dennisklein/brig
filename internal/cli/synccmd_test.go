// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dennisklein/brig/internal/ocsync"
)

func writeTestFile(t *testing.T, path, data string, perm os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(data), perm); err != nil {
		t.Fatal(err)
	}
}

// configDir writes an OpenShell config directory with a GitHub provider.
func configDir(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "it's  mine")
	writeTestFile(t, filepath.Join(dir, "profiles", "github.yaml"), "id: github\ndisplay_name: GitHub\nendpoints:\n  - host: api.github.com\n    port: 443\n", 0o600)
	writeTestFile(t, filepath.Join(dir, "providers", "github.yaml"), `name: github
type: github
credentials:
  GH_TOKEN:
    secret_tool:
      lookup: [service, github.com]
`, 0o600)
	writeTestFile(t, filepath.Join(dir, "policies", "default.yaml"), "version: 1\n", 0o600)
	return dir
}

func TestCreateChecksOpenShellConfigsBeforeBuildingAnImage(t *testing.T) {
	bad := t.TempDir()
	writeTestFile(t, filepath.Join(bad, "providers", "p.yaml"), "name: p\ntype: github\ncredentials:\n  T:\n    value: s3cret\n", 0o600)
	for _, tc := range []struct {
		args    []string
		wantErr string
	}{
		{[]string{"--openshell-config", filepath.Join(bad, "missing")}, "no such file"},
		{[]string{"--openshell-config", bad}, "never values"},
		{[]string{"--openshell-config", bad, "--no-openshell-config"}, "none of the others"},
	} {
		testApp(t)
		out, err := run(t, append([]string{"create", "dev"}, tc.args...)...)
		if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
			t.Errorf("create %q: %v, want an error containing %q", tc.args, err, tc.wantErr)
		}
		if strings.Contains(out, "building") {
			t.Errorf("create %q started to build an image:\n%s", tc.args, out)
		}
	}
}

func TestEnvExportsTheDefaultPolicy(t *testing.T) {
	a := testApp(t)
	testVM(t, a, "plain")
	v := testVM(t, a, "dev")
	dir := configDir(t)
	v.OpenShellConfigs = []string{dir}
	if err := a.vms.Save(v); err != nil {
		t.Fatal(err)
	}
	policy := filepath.Join(dir, "policies", "default.yaml")
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("no shell")
	}
	// What a shell makes of eval "$(brig env NAME)", after switching from
	// the VM with a default policy to one without.
	script := ""
	for _, name := range []string{"dev", "plain"} {
		out, err := run(t, "env", name)
		if err != nil {
			t.Fatal(err)
		}
		script += `eval "$(cat <<'EOF'` + "\n" + out + "EOF\n" + `)"; printf '%s|%s|' "$OPENSHELL_GATEWAY" "${OPENSHELL_SANDBOX_POLICY-unset}"` + "\n"
	}
	got, err := exec.Command(sh, "-c", script).Output()
	if err != nil {
		t.Fatal(err)
	}
	if want := "brig-dev|" + policy + "|brig-plain|unset|"; string(got) != want {
		t.Errorf("shell saw %q, want %q", got, want)
	}
}

func TestUpdateChecksOpenShellConfigs(t *testing.T) {
	a := testApp(t)
	testVM(t, a, "dev")
	if _, err := run(t, "update", "dev", "--remove-openshell-config", "/nowhere"); err == nil || !strings.Contains(err.Error(), "has no OpenShell config directory /nowhere") {
		t.Errorf("removing an unknown directory: %v", err)
	}
	if _, err := run(t, "update", "dev", "--add-openshell-config", "/nowhere"); err == nil || !strings.Contains(err.Error(), "no such file") {
		t.Errorf("adding a missing directory: %v", err)
	}
}

func TestSyncWithoutConfigs(t *testing.T) {
	a := testApp(t)
	testVM(t, a, "dev")
	if _, err := run(t, "sync", "dev"); err == nil || !strings.Contains(err.Error(), "--add-openshell-config") {
		t.Errorf("sync without configs: %v", err)
	}
}

// TestSyncPrunesAfterLastConfigRemoved checks that --prune still deletes what
// brig created once the VM has no config directories left.
func TestSyncPrunesAfterLastConfigRemoved(t *testing.T) {
	a := testApp(t)
	v := testVM(t, a, "dev")
	st := &ocsync.State{
		Providers: map[string]ocsync.ProviderState{"github": {Type: "github", Created: true}},
		LastSync:  &ocsync.Record{Time: time.Now().UTC()},
	}
	if err := st.Save(a.vmFile(v, syncStateFile)); err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	log := filepath.Join(bin, "log")
	writeTestFile(t, filepath.Join(bin, "openshell"), `#!/bin/sh
echo "$*" >>'`+log+`'
case "$*" in
*"status -o json") echo '{"status": "connected", "version": "0.1.2"}' ;;
*"profile list -o json") echo '[]' ;;
*"provider list -o json") echo '{"providers": [{"name": "github", "type": "github"}], "next_page_token": ""}' ;;
esac
`, 0o755)
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

	out, err := run(t, "sync", "dev", "--prune")
	if err != nil {
		t.Fatalf("sync --prune: %v\n%s", err, out)
	}
	argv, _ := os.ReadFile(log)
	if !strings.Contains(string(argv), "-g brig-dev provider delete github\n") {
		t.Errorf("openshell calls:\n%s", argv)
	}
}

// TestSync runs brig sync against a fake openshell CLI and secret-tool.
func TestSync(t *testing.T) {
	a := testApp(t)
	v := testVM(t, a, "dev")
	v.OpenShellConfigs = []string{configDir(t)}
	if err := a.vms.Save(v); err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	log, secretSeen := filepath.Join(bin, "log"), filepath.Join(bin, "secret")
	writeTestFile(t, filepath.Join(bin, "openshell"), `#!/bin/sh
echo "$*" >>'`+log+`'
case "$*" in
*"status -o json") echo '{"status": "connected", "version": "0.1.2"}' ;;
*"profile list -o json") echo '[]' ;;
*"provider list -o json") echo '{"providers": [], "next_page_token": ""}' ;;
*"provider create"*) printf '%s' "$GH_TOKEN" >'`+secretSeen+`' ;;
esac
`, 0o755)
	tool := filepath.Join(bin, "my-secret-tool")
	writeTestFile(t, tool, "#!/bin/sh\n[ \"$*\" = 'lookup service github.com' ] && printf 'tok\\n'\n", 0o755)
	writeTestFile(t, a.dirs.ConfigFile(), "openshell:\n  secret_tool: "+tool+"\n", 0o600)
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

	out, err := run(t, "sync", "dev", "--dry-run")
	if err != nil || !strings.Contains(out, "provider github: would create (type github); GH_TOKEN may be sent to api.github.com:443") {
		t.Fatalf("dry run: %v\n%s", err, out)
	}
	if _, err := os.Stat(a.vmFile(v, syncStateFile)); err == nil {
		t.Error("the dry run saved state")
	}

	out, err = run(t, "sync", "dev")
	if err != nil {
		t.Fatalf("sync: %v\n%s", err, out)
	}
	argv, _ := os.ReadFile(log)
	if !strings.Contains(string(argv), "-g brig-dev provider create --name github --type github --credential GH_TOKEN\n") {
		t.Errorf("openshell calls:\n%s", argv)
	}
	if strings.Contains(string(argv), "tok") {
		t.Error("the secret appeared on openshell's command line")
	}
	if seen, _ := os.ReadFile(secretSeen); string(seen) != "tok" {
		t.Errorf("openshell got the secret %q", seen)
	}
	st, err := ocsync.LoadState(a.vmFile(v, syncStateFile))
	if err != nil || st.LastSync == nil || st.LastSync.Error != "" || st.Providers["github"].Type != "github" {
		t.Errorf("state %+v, %v", st, err)
	}
	if out, _ := run(t, "show", "dev"); !strings.Contains(out, "Last sync:") || !strings.Contains(out, ", ok") {
		t.Errorf("show:\n%s", out)
	}
	// A secret-tool given on the command line wins over config.yaml.
	if _, err := run(t, "sync", "dev", "--refresh-secrets", "--secret-tool", filepath.Join(bin, "missing-tool")); err == nil {
		t.Error("sync with a missing --secret-tool succeeded")
	}
	if out, _ := run(t, "show", "dev"); !strings.Contains(out, "failed:") {
		t.Errorf("show after a failed sync:\n%s", out)
	}
}
