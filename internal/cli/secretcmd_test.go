// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/dennisklein/brig/internal/ocsync"
)

// runIn is run with standard input.
func runIn(t *testing.T, stdin string, args ...string) (string, error) {
	t.Helper()
	cmd := newRootCmd(&progressRun{})
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetIn(strings.NewReader(stdin))
	cmd.SetArgs(args)
	err := cmd.Execute()
	return out.String(), err
}

// secretFixture is a VM with a config directory of providers, a secret-tool
// that keeps what it is asked to store, and an openshell that answers like a
// gateway that is up or not.
type secretFixture struct {
	a            *app
	dir          string
	tool         string
	toolLog      string
	stored       string // the secret the tool was given on standard input
	openshellLog string
}

func newSecretFixture(t *testing.T, gatewayUp bool) *secretFixture {
	t.Helper()
	f := &secretFixture{a: testApp(t)}
	v := testVM(t, f.a, "dev")
	f.dir = configDir(t)
	writeTestFile(t, filepath.Join(f.dir, "profiles", "gitlab.yaml"), "id: gitlab\nendpoints:\n  - host: gitlab.example.org\n    port: 443\n", 0o600)
	writeTestFile(t, filepath.Join(f.dir, "providers", "gitlab.yaml"), "name: gitlab\ntype: gitlab\ncredentials:\n  GL_TOKEN:\n    secret_tool:\n      lookup: [service, gitlab.example.org, user, it's me]\n", 0o600)
	v.OpenShellConfigs = []string{f.dir}
	if err := f.a.vms.Save(v); err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	f.toolLog, f.stored = filepath.Join(bin, "tool-log"), filepath.Join(bin, "stored")
	f.tool = filepath.Join(bin, "my-secret-tool")
	writeTestFile(t, f.tool, `#!/bin/sh
echo "$*" >>'`+f.toolLog+`'
case "$1" in
store) cat >'`+f.stored+`' ;;
lookup) if [ "$3" = github.com ] && [ -f '`+f.stored+`' ]; then cat '`+f.stored+`'; else exit 1; fi ;;
esac
`, 0o755)
	f.openshellLog = filepath.Join(bin, "openshell-log")
	status := `echo '{"status": "connected", "version": "0.1.2"}'`
	if !gatewayUp {
		status = "echo 'cannot connect' >&2; exit 1"
	}
	writeTestFile(t, filepath.Join(bin, "openshell"), `#!/bin/sh
echo "$*" >>'`+f.openshellLog+`'
case "$*" in
*"status -o json") `+status+` ;;
*"profile list -o json") echo '[{"id": "github", "scope": "platform"}, {"id": "gitlab", "scope": "platform"}]' ;;
*"provider list -o json") echo '{"providers": [], "next_page_token": ""}' ;;
esac
`, 0o755)
	writeTestFile(t, f.a.dirs.ConfigFile(), "openshell:\n  secret_tool: "+f.tool+"\n", 0o600)
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	return f
}

func TestSecretSetStoresAndSyncsOnlyThatProvider(t *testing.T) {
	f := newSecretFixture(t, true)
	out, err := runIn(t, "ghp_s3cret\n", "secret", "set", "dev", "github")
	if err != nil {
		t.Fatalf("secret set: %v\n%s", err, out)
	}
	// The sync then looks the secret up, and only that one.
	if got, _ := os.ReadFile(f.toolLog); string(got) != "store --label=brig: github GH_TOKEN service github.com\nlookup service github.com\n" {
		t.Errorf("secret-tool was run as %q", got)
	}
	if got, _ := os.ReadFile(f.stored); string(got) != "ghp_s3cret" {
		t.Errorf("stored %q, want the secret without its newline", got)
	}
	if strings.Contains(out, "ghp_s3cret") {
		t.Errorf("output shows the secret:\n%s", out)
	}
	calls, _ := os.ReadFile(f.openshellLog)
	if !strings.Contains(string(calls), "provider create --name github --type github --credential GH_TOKEN") {
		t.Errorf("github was not synced:\n%s", calls)
	}
	if strings.Contains(string(calls), "--name gitlab") {
		t.Errorf("gitlab was synced too:\n%s", calls)
	}
	for _, want := range []string{"Stored the secret of GH_TOKEN of provider github", "provider github: created", "keep the old token"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	// A sync of one provider is no full sync.
	v, err := f.a.vms.Load("dev")
	if err != nil {
		t.Fatal(err)
	}
	st, err := ocsync.LoadState(f.a.vmFile(v, ocsync.StateFile))
	if err != nil || st.LastSync != nil || st.Workspace("default").Providers["github"].Type != "github" {
		t.Errorf("state: %+v, %v", st, err)
	}
}

func TestSecretSetWithoutARunningVMOrWithNoSync(t *testing.T) {
	f := newSecretFixture(t, false)
	out, err := runIn(t, "tok", "secret", "set", "dev", "github")
	if err != nil || !strings.Contains(out, "does not answer") || !strings.Contains(out, "brig start dev") {
		t.Errorf("stopped VM: %v\n%s", err, out)
	}
	if got, _ := os.ReadFile(f.stored); string(got) != "tok" {
		t.Errorf("stored %q", got)
	}

	f = newSecretFixture(t, true)
	out, err = runIn(t, "tok", "secret", "set", "dev", "github", "--no-sync")
	if err != nil || !strings.Contains(out, "brig sync dev --workspace default") {
		t.Errorf("--no-sync: %v\n%s", err, out)
	}
	if calls, _ := os.ReadFile(f.openshellLog); strings.Contains(string(calls), "provider") {
		t.Errorf("--no-sync still synced:\n%s", calls)
	}
}

func TestSecretSetArguments(t *testing.T) {
	f := newSecretFixture(t, false)
	// The VM that brig use selected, and PROVIDER CREDENTIAL.
	t.Setenv(gatewayEnv, "brig-dev")
	for _, args := range [][]string{{"github"}, {"github", "GH_TOKEN"}, {"dev", "github"}, {"dev", "github", "GH_TOKEN"}} {
		out, err := runIn(t, "tok", append([]string{"secret", "set"}, args...)...)
		if err != nil || !strings.Contains(out, "Stored the secret of GH_TOKEN of provider github") {
			t.Errorf("secret set %q: %v\n%s", args, err, out)
		}
	}
	if got, _ := os.ReadFile(f.toolLog); strings.Count(string(got), "store ") != 4 {
		t.Errorf("secret-tool calls:\n%s", got)
	}

	for _, tc := range []struct {
		args    []string
		wantErr string
	}{
		{[]string{"nosuch"}, "define no provider nosuch in workspace default; they define github, gitlab"},
		{[]string{"github", "OTHER"}, "has no credential OTHER; it has GH_TOKEN"},
		{[]string{"dev", "github", "GH_TOKEN", "x"}, "accepts between 1 and 3"},
		{[]string{"github", "-w", "team"}, "workspace team is not defined"},
	} {
		if out, err := runIn(t, "tok", append([]string{"secret", "set"}, tc.args...)...); err == nil || !strings.Contains(err.Error(), tc.wantErr) {
			t.Errorf("secret set %q: %v\n%s, want an error with %q", tc.args, err, out, tc.wantErr)
		}
	}

	t.Setenv(gatewayEnv, "")
	if _, err := runIn(t, "tok", "secret", "set", "dve", "github"); err == nil || !strings.Contains(err.Error(), "no VM called dve") {
		t.Errorf("unknown VM: %v", err)
	}
}

func TestSecretSetRefuses(t *testing.T) {
	f := newSecretFixture(t, false)
	writeTestFile(t, filepath.Join(f.dir, "providers", "ci.yaml"), "name: ci\ntype: github\ncredentials:\n  A:\n    env: BRIG_SECRET_A\n  B:\n    secret_tool: {lookup: [k, v]}\n", 0o600)
	for _, tc := range []struct {
		stdin   string
		args    []string
		wantErr string
	}{
		{"tok", []string{"ci", "A"}, "reads the environment variable BRIG_SECRET_A"},
		{"tok", []string{"ci"}, "has the credentials A, B; name one"},
		{"", []string{"github"}, "no secret on standard input"},
		{"\n", []string{"github"}, "no secret on standard input"},
		{strings.Repeat("x", maxSecretInput+1), []string{"github"}, "larger than"},
	} {
		_, err := runIn(t, tc.stdin, append([]string{"secret", "set", "dev"}, tc.args...)...)
		if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
			t.Errorf("secret set %q: %v, want an error with %q", tc.args, err, tc.wantErr)
		}
	}
	if _, err := os.Stat(f.toolLog); err == nil {
		t.Error("secret-tool ran")
	}

	// A wrapper without a store verb.
	writeTestFile(t, f.tool, "#!/bin/sh\necho \"usage: $0 lookup ATTR VALUE\" >&2\nexit 2\n", 0o755)
	_, err := runIn(t, "hunter2", "secret", "set", "dev", "github")
	if err == nil || !strings.Contains(err.Error(), "cannot store secrets") || !strings.Contains(err.Error(), "service github.com") {
		t.Fatalf("wrapper without store: %v", err)
	}
	if strings.Contains(err.Error(), "hunter2") {
		t.Errorf("error shows the secret: %v", err)
	}
}

func TestSecretList(t *testing.T) {
	f := newSecretFixture(t, false)
	writeTestFile(t, filepath.Join(f.dir, "providers", "ci.yaml"), "name: ci\ntype: github\ncredentials:\n  A:\n    env: BRIG_SECRET_CI_A\n", 0o600)
	t.Setenv("BRIG_SECRET_CI_A", "envsecret")
	if out, err := runIn(t, "ghp_s3cret", "secret", "set", "dev", "github", "--no-sync"); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}

	out, err := run(t, "secret", "list", "dev")
	if err != nil {
		t.Fatalf("secret list: %v\n%s", err, out)
	}
	for _, want := range []string{
		"PROVIDER  CREDENTIAL  SOURCE",
		"ci        A           env BRIG_SECRET_CI_A",
		"github    GH_TOKEN    secret-tool service github.com  ",
		"stored",
		"missing",
		"brig secret set dev gitlab",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	for _, leak := range []string{"ghp_s3cret", "envsecret"} {
		if strings.Contains(out, leak) {
			t.Errorf("output shows %q:\n%s", leak, out)
		}
	}

	out, err = run(t, "secret", "list", "dev", "-o", "json")
	if err != nil {
		t.Fatal(err)
	}
	var rep secretListReport
	if err := json.Unmarshal([]byte(out), &rep); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	status := map[string]string{}
	for _, c := range rep.Credentials {
		status[c.Provider+"/"+c.Credential] = c.Status
	}
	if rep.VM != "dev" || rep.Workspace != "default" || len(status) != 3 ||
		status["ci/A"] != "stored" || status["github/GH_TOKEN"] != "stored" || status["gitlab/GL_TOKEN"] != "missing" {
		t.Errorf("report %+v", rep)
	}

	// A keyring that fails is an error, not a missing secret.
	writeTestFile(t, f.tool, "#!/bin/sh\necho 'Cannot autolaunch D-Bus' >&2\nexit 1\n", 0o755)
	out, err = run(t, "secret", "list", "dev")
	if err == nil || !strings.Contains(err.Error(), "could not check") || !strings.Contains(out, "D-Bus") {
		t.Errorf("failing keyring: %v\n%s", err, out)
	}
}

func TestReadSecretFromTerminalDoesNotEcho(t *testing.T) {
	master, slave := openPTY(t)

	var prompts bytes.Buffer
	go func() {
		// The terminal's echo is off once the prompt is out.
		time.Sleep(200 * time.Millisecond)
		_, _ = master.WriteString("s3crét\n")
	}()
	secret, err := readSecretFromTerminal(context.Background(), slave, &prompts, "Secret: ")
	if err != nil || secret != "s3crét" {
		t.Fatalf("readSecretFromTerminal() = %q, %v", secret, err)
	}
	if got := prompts.String(); got != "Secret: \nRead 6 characters.\n" {
		t.Errorf("prompts: %q", got)
	}
	// What the terminal echoed to its master: nothing of the secret. Reading
	// the master blocks, so a goroutine does it.
	echoed := make(chan string, 1)
	go func() {
		buf := make([]byte, 256)
		m, _ := master.Read(buf)
		echoed <- string(buf[:m])
	}()
	_, _ = master.WriteString("\n") // something to read on the master, after the echo
	select {
	case got := <-echoed:
		if strings.Contains(got, "s3cr") {
			t.Errorf("the terminal echoed the secret: %q", got)
		}
	case <-time.After(2 * time.Second):
		t.Error("nothing on the master")
	}
	// Echo is back on.
	if tio, err := unix.IoctlGetTermios(int(slave.Fd()), unix.TCGETS); err != nil || tio.Lflag&unix.ECHO == 0 {
		t.Errorf("echo is off after reading: %v", err)
	}
}

// TestReadSecretFromTerminalRefusesWhatItCannotRead checks that a line the
// terminal cuts, and a paste of more lines, are refused, and that the rest of
// such a paste is not left for the shell to read.
func TestReadSecretFromTerminalRefusesWhatItCannotRead(t *testing.T) {
	for _, tc := range []struct{ typed, want string }{
		{strings.Repeat("x", 6000) + "\n", "too long for the terminal"},
		{strings.Repeat("x", maxTerminalLine-1) + "\n", "too long for the terminal"},
		{"first\nrm -rf ~\nthird\n", "more than one line"},
	} {
		master, slave := openPTY(t)
		go func() {
			buf := make([]byte, 4096)
			for {
				if _, err := master.Read(buf); err != nil {
					return
				}
			}
		}()
		go func() {
			time.Sleep(200 * time.Millisecond)
			_, _ = master.WriteString(tc.typed)
		}()
		secret, err := readSecretFromTerminal(context.Background(), slave, io.Discard, "Secret: ")
		if err == nil || !strings.Contains(err.Error(), tc.want) || secret != "" {
			t.Errorf("%.20q: readSecretFromTerminal() = %.20q, %v; want %q", tc.typed, secret, err, tc.want)
		}
		if n, err := unix.IoctlGetInt(int(slave.Fd()), unix.TIOCINQ); err != nil || n != 0 {
			t.Errorf("%.20q: %d bytes left for the shell, %v", tc.typed, n, err)
		}
	}
	// The longest line the terminal does not cut is taken.
	master, slave := openPTY(t)
	go func() {
		time.Sleep(200 * time.Millisecond)
		_, _ = master.WriteString(strings.Repeat("x", maxTerminalLine-2) + "\n")
	}()
	if secret, err := readSecretFromTerminal(context.Background(), slave, io.Discard, "Secret: "); err != nil || len(secret) != maxTerminalLine-2 {
		t.Errorf("longest line: %d bytes, %v", len(secret), err)
	}
}
