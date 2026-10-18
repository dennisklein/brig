// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/dennisklein/brig/internal/vm"
)

func TestAgentInitWritesFilesWithoutAVM(t *testing.T) {
	a := testApp(t)
	dir, secrets := filepath.Join(t.TempDir(), "shared"), filepath.Join(t.TempDir(), "personal")
	args := []string{"agent", "init", "claude", "--dir", dir, "--secrets-dir", secrets, "--github", "alice"}
	out, err := run(t, args...)
	if err != nil {
		t.Fatalf("agent init: %v\n%s", err, out)
	}
	for _, want := range []string{
		filepath.Join(dir, "agents/claude/Containerfile"),
		filepath.Join(dir, "agents/claude/agent.yaml"),
		filepath.Join(dir, "profiles/claude-code.yaml"),
		filepath.Join(dir, "policies/default.yaml"),
		filepath.Join(secrets, "providers/claude-code.yaml"),
		filepath.Join(secrets, "providers/github.yaml"),
	} {
		if _, err := os.Stat(want); err != nil {
			t.Errorf("missing %s", want)
		}
	}
	// What to do next, by hand.
	for _, want := range []string{
		"brig update VM --add-openshell-config " + dir + " --add-openshell-config " + secrets,
		"# Run claude setup-token first",
		"brig secret set VM claude-code",
		"brig secret set VM github",
		"brig sync VM",
		"brig agent build VM claude",
		"brig sandbox create VM --connect -- --name webapp --template claude --provider claude-code --provider github",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	if _, err := os.Stat(a.dirs.ConfigFile()); err == nil {
		t.Error("brig's config file was written")
	}

	// Again: nothing changes. An edit stops it, unless forced.
	if out, err = run(t, args...); err != nil || strings.Contains(out, "created") || !strings.Contains(out, "unchanged") {
		t.Errorf("second run: %v\n%s", err, out)
	}
	cf := filepath.Join(dir, "agents/claude/Containerfile")
	writeTestFile(t, cf, "mine\n", 0o644)
	if _, err = run(t, args...); err == nil || !strings.Contains(err.Error(), cf) || !strings.Contains(err.Error(), "--force") {
		t.Errorf("conflict: %v", err)
	}
	if out, err = run(t, append(args, "--force")...); err != nil || !strings.Contains(out, "overwritten") {
		t.Errorf("--force: %v\n%s", err, out)
	}
	if got, _ := os.ReadFile(cf); string(got) == "mine\n" {
		t.Error("--force did not overwrite")
	}
}

func TestAgentInitLeavesOutWhatAnotherDirectoryDefines(t *testing.T) {
	testApp(t)
	dir, secrets, team := filepath.Join(t.TempDir(), "shared"), filepath.Join(t.TempDir(), "personal"), t.TempDir()
	writeTestFile(t, filepath.Join(team, "profiles", "llm.yaml"), "id: llm\nendpoints:\n  - host: llm.corp.example\n    port: 443\n", 0o600)
	writeTestFile(t, filepath.Join(team, "policies", "default.yaml"), "version: 1\n", 0o600)
	// The team directory is one of the VM's, so it counts; the VM is given.
	a, err := newApp()
	if err != nil {
		t.Fatal(err)
	}
	v := testVM(t, a, "dev")
	v.OpenShellConfigs = []string{team}
	if err := a.vms.Save(v); err != nil {
		t.Fatal(err)
	}
	stubSteps(t, nil)
	fakeOpenShellDown(t)
	out, _ := run(t, "agent", "init", "pi", "--dir", dir, "--secrets-dir", secrets, "--vm", "dev",
		"--endpoint", "https://llm.example.com/v1", "--model", "m")
	for _, want := range []string{
		"skipped", filepath.Join(dir, "profiles/llm.yaml"), "profile llm is defined in " + filepath.Join(team, "profiles/llm.yaml") + " already",
		"default.yaml", "policy default is defined in " + filepath.Join(team, "policies/default.yaml"),
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	for _, gone := range []string{filepath.Join(dir, "profiles/llm.yaml"), filepath.Join(dir, "policies/default.yaml")} {
		if _, err := os.Stat(gone); err == nil {
			t.Errorf("%s was written", gone)
		}
	}
	if _, err := os.Stat(filepath.Join(secrets, "providers/llm.yaml")); err != nil {
		t.Errorf("the provider was not written: %v", err)
	}
}

func TestAgentInitRequiresADirectory(t *testing.T) {
	a := testApp(t)
	if _, err := run(t, "agent", "init", "claude"); err == nil || !strings.Contains(err.Error(), "--dir") {
		t.Errorf("no --dir: %v", err)
	}
	if _, err := run(t, "agent", "init", "nosuch", "--dir", t.TempDir()); err == nil || !strings.Contains(err.Error(), "claude, pi and opencode") {
		t.Errorf("unknown agent: %v", err)
	}
	if _, err := run(t, "agent", "init", "pi", "--dir", t.TempDir()); err == nil || !strings.Contains(err.Error(), "--endpoint") {
		t.Errorf("pi without endpoint: %v", err)
	}
	// With --vm, the one directory without providers/ is the shared one.
	shared, personal := t.TempDir(), t.TempDir()
	writeTestFile(t, filepath.Join(personal, "providers", "x.yaml"), "name: x\ntype: x\ncredentials:\n  T:\n    env: BRIG_SECRET_T\n", 0o600)
	v := testVM(t, a, "dev")
	v.OpenShellConfigs = []string{personal, shared}
	if err := a.vms.Save(v); err != nil {
		t.Fatal(err)
	}
	stubSteps(t, nil)
	fakeOpenShellDown(t)
	_, _ = run(t, "agent", "init", "claude", "--vm", "dev", "--secrets-dir", personal)
	if _, err := os.Stat(filepath.Join(shared, "agents/claude/Containerfile")); err != nil {
		t.Errorf("files not in the shared directory: %v", err)
	}
	v.OpenShellConfigs = []string{shared, t.TempDir()}
	if err := a.vms.Save(v); err != nil {
		t.Fatal(err)
	}
	if _, err := run(t, "agent", "init", "claude", "--vm", "dev"); err == nil || !strings.Contains(err.Error(), "not exactly one") {
		t.Errorf("two directories without providers: %v", err)
	}
}

// stubSteps replaces the brig commands of agent init --vm with a recorder
// that fails the command called fail, if any, and returns the commands run.
func stubSteps(t *testing.T, failing map[string]bool) *[]string {
	t.Helper()
	var ran []string
	old := brigStep
	brigStep = func(_ context.Context, _ *cobra.Command, args ...string) error {
		ran = append(ran, strings.Join(args, " "))
		if failing[args[0]] {
			return os.ErrInvalid
		}
		return nil
	}
	t.Cleanup(func() { brigStep = old })
	return &ran
}

// fakeOpenShellDown and fakeOpenShellUp put an openshell in PATH whose
// gateway does not answer, or does; every call is logged to the returned file.
func fakeOpenShellDown(t *testing.T) string { return fakeOpenShellGateway(t, false) }
func fakeOpenShellUp(t *testing.T) string   { return fakeOpenShellGateway(t, true) }

func fakeOpenShellGateway(t *testing.T, up bool) string {
	t.Helper()
	dir := t.TempDir()
	log := filepath.Join(dir, "openshell-log")
	status := "echo 'cannot connect' >&2; exit 1"
	if up {
		status = `echo '{"status": "connected", "version": "0.1.2"}'`
	}
	fakeCommands(t, map[string]string{"openshell": `echo "$*" >>'` + log + `'
case "$*" in *"status -o json") ` + status + ` ;; esac`})
	return log
}

func TestAgentInitWithAVMRunsTheSteps(t *testing.T) {
	a := testApp(t)
	dir, secrets := filepath.Join(t.TempDir(), "shared"), filepath.Join(t.TempDir(), "personal")
	testVM(t, a, "dev")
	fakeOpenShellUp(t)
	// A keyring without the secret.
	tool := filepath.Join(t.TempDir(), "secret-tool")
	writeTestFile(t, tool, "#!/bin/sh\nexit 1\n", 0o755)
	writeTestFile(t, a.dirs.ConfigFile(), "openshell:\n  secret_tool: "+tool+"\n", 0o600)
	ran := stubSteps(t, nil)

	out, err := run(t, "agent", "init", "claude", "--dir", dir, "--secrets-dir", secrets, "--vm", "dev")
	if err != nil {
		t.Fatalf("agent init --vm: %v\n%s", err, out)
	}
	// Standard input is no terminal here: the secret is left for the user.
	want := []string{
		"update dev --add-openshell-config " + dir + " --add-openshell-config " + secrets,
		"sync dev",
		"agent build dev claude",
	}
	if strings.Join(*ran, "\n") != strings.Join(want, "\n") {
		t.Errorf("steps:\n%s\nwant:\n%s", strings.Join(*ran, "\n"), strings.Join(want, "\n"))
	}
	for _, line := range []string{
		"$ brig update dev --add-openshell-config " + dir,
		"$ brig sync dev",
		"$ brig agent build dev claude",
		"Store the missing secrets",
		"brig secret set dev claude-code",
		"brig sandbox create dev --connect -- --name webapp --template claude --provider claude-code",
	} {
		if !strings.Contains(out, line) {
			t.Errorf("output lacks %q:\n%s", line, out)
		}
	}

	// A failing step stops the rest and says how to go on.
	if err := os.WriteFile(tool, []byte("#!/bin/sh\nprintf tok\n"), 0o755); err != nil { //nolint:gosec // G306: the fake must be executable
		t.Fatal(err)
	}
	ran = stubSteps(t, map[string]bool{"sync": true})
	out, err = run(t, "agent", "init", "claude", "--dir", dir, "--secrets-dir", secrets, "--vm", "dev")
	if err == nil || !strings.Contains(err.Error(), "brig sync dev") {
		t.Errorf("failing sync: %v\n%s", err, out)
	}
	if got := strings.Join(*ran, "|"); strings.Contains(got, "agent build") {
		t.Errorf("steps after the failed one ran: %s", got)
	}
	if !strings.Contains(out, "brig agent build dev claude") {
		t.Errorf("no word on how to go on:\n%s", out)
	}
}

func TestAgentInitWithAStoppedVM(t *testing.T) {
	a := testApp(t)
	dir, secrets := filepath.Join(t.TempDir(), "shared"), filepath.Join(t.TempDir(), "personal")
	testVM(t, a, "dev")
	fakeOpenShellDown(t)
	ran := stubSteps(t, nil)
	out, err := run(t, "agent", "init", "claude", "--dir", dir, "--secrets-dir", secrets, "--vm", "dev")
	if err == nil || exitStatus(err) != exitNotReady {
		t.Fatalf("agent init --vm on a stopped VM: %v\n%s", err, out)
	}
	// The directories are attached, and the rest waits for the VM.
	if len(*ran) != 1 || !strings.HasPrefix((*ran)[0], "update dev") {
		t.Errorf("steps: %q", *ran)
	}
	for _, want := range []string{"brig start dev", "brig sync dev", "brig agent build dev claude"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
}

func TestAgentBuild(t *testing.T) {
	a := testApp(t)
	v := testVM(t, a, "dev")
	dir, secrets := t.TempDir(), t.TempDir()
	if out, err := run(t, "agent", "init", "claude", "--dir", dir, "--secrets-dir", secrets); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	v.OpenShellConfigs = []string{dir, secrets}
	if err := a.vms.Save(v); err != nil {
		t.Fatal(err)
	}
	log := filepath.Join(t.TempDir(), "log")
	today := time.Now().Format("2006-01-02")
	fakeCommands(t, map[string]string{
		"podman": `echo "podman $*" >>'` + log + `'
case "$*" in
"image exists localhost/claude-agent:` + today + `") exit 0 ;;
"image exists"*) exit 1 ;;
"image inspect"*) echo sha256:aaa ;;
"save"*) echo image ;;
esac`,
		"ssh": `echo "ssh $*" >>'` + log + `'
case "$*" in
*"image inspect"*) echo sha256:bbb ;;
*) cat >/dev/null ;;
esac`,
		"openshell": `echo "openshell $*" >>'` + log + `'
case "$*" in *"status -o json") echo '{"status": "connected", "version": "0.1.2"}' ;; esac`,
	})

	stubTemplateImage(t, "localhost/claude-agent:old")

	out, err := run(t, "agent", "build", "dev", "claude")
	if err != nil {
		t.Fatalf("agent build: %v\n%s", err, out)
	}
	ref := "localhost/claude-agent:" + today + "-2"
	got, _ := os.ReadFile(log)
	for _, want := range []string{
		"podman build --pull -t " + ref + " " + filepath.Join(dir, "agents/claude"),
		"podman save " + ref,
		"-g brig-dev --workspace default sandbox template delete claude",
		"-g brig-dev --workspace default sandbox template create claude --image " + ref,
	} {
		if !strings.Contains(string(got), want) {
			t.Errorf("calls lack %q:\n%s", want, got)
		}
	}
	if !strings.Contains(out, "Built "+ref+" and pointed template claude at it.") || !strings.Contains(out, "Existing sandboxes keep their image") {
		t.Errorf("output:\n%s", out)
	}

	// --tag and --no-pull; the same image in the VM is not copied again.
	if err := os.WriteFile(log, nil, 0o644); err != nil { //nolint:gosec // G306: a log
		t.Fatal(err)
	}
	fakeCommands(t, map[string]string{
		"podman":    `echo "podman $*" >>'` + log + `'` + "\ncase \"$*\" in \"image inspect\"*) echo sha256:same ;; esac",
		"ssh":       `case "$*" in *"image inspect"*) echo sha256:same ;; *) echo "ssh $*" >>'` + log + `'; cat >/dev/null ;; esac`,
		"openshell": `echo "openshell $*" >>'` + log + `'` + "\ncase \"$*\" in *\"status -o json\") echo '{\"status\": \"connected\", \"version\": \"0.1.2\"}' ;; esac",
	})
	out, err = run(t, "agent", "build", "dev", "claude", "--tag", "test", "--no-pull")
	if err != nil {
		t.Fatalf("agent build --tag: %v\n%s", err, out)
	}
	got, _ = os.ReadFile(log)
	if !strings.Contains(string(got), "podman build -t localhost/claude-agent:test ") || strings.Contains(string(got), "--pull") {
		t.Errorf("calls:\n%s", got)
	}
	if strings.Contains(string(got), "podman save") || strings.Contains(string(got), "ssh ") {
		t.Errorf("the image was copied again:\n%s", got)
	}
	if !strings.Contains(out, "has localhost/claude-agent:test already") {
		t.Errorf("output:\n%s", out)
	}
}

// stubTemplateImage makes image the one that the template of an agent has.
func stubTemplateImage(t *testing.T, image string) {
	t.Helper()
	orig := templateImage
	templateImage = func(context.Context, *vm.VM, string, string) (string, error) { return image, nil }
	t.Cleanup(func() { templateImage = orig })
}

// TestAgentBuildWhenTheTemplateCannotBeCreated fails openshell sandbox
// template create after the delete: the template gets its old image back,
// or the error says that it is gone.
func TestAgentBuildWhenTheTemplateCannotBeCreated(t *testing.T) {
	a := testApp(t)
	v := testVM(t, a, "dev")
	dir := t.TempDir()
	if out, err := run(t, "agent", "init", "claude", "--dir", dir, "--secrets-dir", t.TempDir()); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	v.OpenShellConfigs = []string{dir}
	if err := a.vms.Save(v); err != nil {
		t.Fatal(err)
	}
	for old, want := range map[string]string{
		"localhost/claude-agent:old": "template claude was not updated: ",
		"":                           "template claude was deleted and not created again; create it with openshell -g brig-dev --workspace default sandbox template create claude --image localhost/claude-agent:new",
	} {
		stubTemplateImage(t, old)
		fakeCommands(t, map[string]string{
			"podman": `case "$*" in "image inspect"*) echo sha256:same ;; esac`,
			"ssh":    `case "$*" in *"image inspect"*) echo sha256:same ;; esac`,
			"openshell": `case "$*" in
*"status -o json") echo '{"status": "connected", "version": "0.1.2"}' ;;
*"--image localhost/claude-agent:new") echo "gateway busy" >&2; exit 1 ;;
esac`,
		})
		_, err := run(t, "agent", "build", "dev", "claude", "--tag", "new")
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("old image %q: %v, want %q", old, err, want)
		}
	}
}

func TestAgentBuildRefuses(t *testing.T) {
	a := testApp(t)
	v := testVM(t, a, "dev")
	dir := t.TempDir()
	v.OpenShellConfigs = []string{dir}
	if err := a.vms.Save(v); err != nil {
		t.Fatal(err)
	}
	log := fakeOpenShellDown(t)
	fakeCommands(t, map[string]string{"podman": "echo podman >>'" + log + "'"})
	if _, err := run(t, "agent", "build", "dev", "claude"); err == nil || !strings.Contains(err.Error(), "no agents/claude/agent.yaml") {
		t.Errorf("unknown agent: %v", err)
	}
	if out, err := run(t, "agent", "init", "claude", "--dir", dir, "--secrets-dir", t.TempDir()); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	_, err := run(t, "agent", "build", "dev", "claude")
	if err == nil || exitStatus(err) != exitNotReady {
		t.Errorf("stopped VM: %v", err)
	}
	if got, _ := os.ReadFile(log); strings.Contains(string(got), "podman") {
		t.Errorf("podman ran for a VM that is down:\n%s", got)
	}
}
