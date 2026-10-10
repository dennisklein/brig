// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

package openshell

import (
	"context"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dennisklein/brig/internal/sshx"
)

func pemFile(typ, content string) string {
	return string(pem.EncodeToMemory(&pem.Block{Type: typ, Bytes: []byte(content)}))
}

// fetchOutput is what fetchScript prints for the given files.
func fetchOutput(files ...string) string {
	var b strings.Builder
	for _, f := range files {
		b.WriteString(base64.StdEncoding.EncodeToString([]byte(f)) + "\n")
	}
	return b.String()
}

// fakeSSH puts an ssh script first in PATH that prints the content of the
// returned output file. It logs its arguments to log.
func fakeSSH(t *testing.T) (output, log string) {
	t.Helper()
	dir := t.TempDir()
	output, log = filepath.Join(dir, "output"), filepath.Join(dir, "log")
	writeScript(t, filepath.Join(dir, "ssh"), log, `cat '`+output+`'; exit $(cat '`+output+`.status' 2>/dev/null || echo 0)`)
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return output, log
}

func vmTarget() sshx.Target {
	return sshx.Target{
		Host: "127.0.0.1", Port: 40022, User: "root",
		IdentityFile: "/vms/dev/id_ed25519", KnownHostsFile: "/vms/dev/known_hosts", HostKeyAlias: "brig-dev",
	}
}

func TestSyncBundle(t *testing.T) {
	output, log := fakeSSH(t)
	configHome := filepath.Join(t.TempDir(), "openshell")
	dir := MTLSDir(configHome, "brig-dev")
	ca, cert, key := pemFile("CERTIFICATE", "ca1"), pemFile("CERTIFICATE", "client1"), pemFile("PRIVATE KEY", "key1")

	sync := func(wantChanged bool) {
		t.Helper()
		changed, err := SyncBundle(context.Background(), vmTarget(), configHome, "brig-dev")
		if err != nil {
			t.Fatal(err)
		}
		if changed != wantChanged {
			t.Errorf("changed = %v, want %v", changed, wantChanged)
		}
		for name, want := range map[string]string{"ca.crt": ca, "tls.crt": cert, "tls.key": key} {
			path := filepath.Join(dir, name)
			if got := readFile(t, path); got != want {
				t.Errorf("%s = %q, want %q", name, got, want)
			}
			if fi, err := os.Stat(path); err != nil || fi.Mode().Perm() != 0o600 {
				t.Errorf("%s: mode %v, %v; want 0600", name, fi.Mode().Perm(), err)
			}
		}
		if fi, err := os.Stat(dir); err != nil || fi.Mode().Perm() != 0o700 {
			t.Errorf("%s: mode %v, %v; want 0700", dir, fi.Mode().Perm(), err)
		}
	}

	writeFile(t, output, fetchOutput(ca, cert, key), 0o600)
	sync(true)
	args := readFile(t, log)
	if !strings.Contains(args, " -- agent@127.0.0.1 "+fetchScript()+"\n") {
		t.Errorf("ssh args = %q, want a login as agent running fetchScript()", args)
	}
	sync(false)

	if err := os.Chmod(filepath.Join(dir, "tls.crt"), 0o644); err != nil { //nolint:gosec // G302: a too-open file that SyncBundle must tighten
		t.Fatal(err)
	}
	sync(true)

	// The gateway regenerated its PKI.
	ca, cert, key = pemFile("CERTIFICATE", "ca2"), pemFile("CERTIFICATE", "client2"), pemFile("PRIVATE KEY", "key2")
	writeFile(t, output, fetchOutput(ca, cert, key), 0o600)
	sync(true)

	// The agent's ~/.bashrc prints before the script runs.
	writeFile(t, output, "Agent pid 4242\n"+fetchOutput(ca, cert, key), 0o600)
	sync(false)
}

func TestSyncBundleErrors(t *testing.T) {
	output, _ := fakeSSH(t)
	configHome := filepath.Join(t.TempDir(), "openshell")
	sync := func() error {
		_, err := SyncBundle(context.Background(), vmTarget(), configHome, "brig-dev")
		return err
	}

	writeFile(t, output+".status", "3", 0o600)
	if err := sync(); !errors.Is(err, ErrNoBundle) {
		t.Errorf("SyncBundle() = %v, want ErrNoBundle", err)
	}
	writeFile(t, output, "Connection refused\n", 0o600)
	writeFile(t, output+".status", "255", 0o600)
	if err := sync(); err == nil || errors.Is(err, ErrNoBundle) {
		t.Errorf("SyncBundle() = %v, want an ssh error", err)
	}
	if err := os.Remove(output + ".status"); err != nil {
		t.Fatal(err)
	}

	cert := pemFile("CERTIFICATE", "x")
	for name, out := range map[string]string{
		"too few files":  fetchOutput(cert, cert),
		"not base64":     fetchOutput(cert, cert) + "!!!\n",
		"not PEM":        fetchOutput(cert, cert, "key"),
		"wrong PEM type": fetchOutput(cert, cert, cert),
		"empty file":     fetchOutput(cert, "", pemFile("PRIVATE KEY", "k")),
		"key for the CA": fetchOutput(pemFile("PRIVATE KEY", "k"), cert, pemFile("PRIVATE KEY", "k")),
	} {
		writeFile(t, output, out, 0o600)
		if err := sync(); err == nil {
			t.Errorf("%s: SyncBundle() succeeded", name)
		}
	}
	if _, err := os.Stat(MTLSDir(configHome, "brig-dev")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("failed syncs created the bundle directory: %v", err)
	}

	for _, gateway := range []string{"", ".", "..", "a/b"} {
		if _, err := SyncBundle(context.Background(), vmTarget(), configHome, gateway); err == nil {
			t.Errorf("SyncBundle() accepted gateway %q", gateway)
		}
	}
}

// TestFetchScript runs the remote script against a local copy of the
// gateway's PKI directory.
func TestFetchScript(t *testing.T) {
	if _, err := exec.LookPath("base64"); err != nil {
		t.Skip("base64 not installed")
	}
	tls := t.TempDir()
	script := strings.Replace(fetchScript(), remoteTLSDir, tls, 1)
	run := func() ([]byte, error) { return exec.Command("sh", "-c", script).Output() }

	var exitErr *exec.ExitError
	if _, err := run(); !errors.As(err, &exitErr) || exitErr.ExitCode() != noBundleStatus {
		t.Fatalf("script on an empty directory: %v, want exit status %d", err, noBundleStatus)
	}
	files := map[string]string{
		"ca.crt":         pemFile("CERTIFICATE", "ca"),
		"ca.key":         pemFile("PRIVATE KEY", "ca key"),
		"client/tls.crt": pemFile("CERTIFICATE", "client"),
		"client/tls.key": pemFile("PRIVATE KEY", "client key"),
	}
	for name, data := range files {
		if name != "client/tls.key" {
			writeFile(t, filepath.Join(tls, name), data, 0o600)
		}
	}
	if _, err := run(); !errors.As(err, &exitErr) || exitErr.ExitCode() != noBundleStatus {
		t.Fatalf("script without client/tls.key: %v, want exit status %d", err, noBundleStatus)
	}
	writeFile(t, filepath.Join(tls, "client/tls.key"), files["client/tls.key"], 0o600)
	out, err := run()
	if err != nil {
		t.Fatal(err)
	}
	got, err := decodeBundle(out)
	if err != nil {
		t.Fatal(err)
	}
	for i, name := range []string{"ca.crt", "client/tls.crt", "client/tls.key"} {
		if string(got[i]) != files[name] {
			t.Errorf("file %d = %q, want %s", i, got[i], name)
		}
	}
	if strings.Contains(fetchScript(), "ca.key") {
		t.Error("fetchScript() reads the CA key")
	}
}

func TestRemoveBundle(t *testing.T) {
	configHome := filepath.Join(t.TempDir(), "openshell")
	dir := MTLSDir(configHome, "brig-dev")
	if dir != filepath.Join(configHome, "gateways", "brig-dev", "mtls") {
		t.Errorf("MTLSDir() = %s", dir)
	}
	metadata := filepath.Join(configHome, "gateways", "brig-dev", "metadata.json")
	writeFile(t, metadata, "{}", 0o600)
	writeFile(t, filepath.Join(dir, "tls.key"), "key", 0o600)
	if err := RemoveBundle(configHome, "brig-dev"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("%s still exists: %v", dir, err)
	}
	if _, err := os.Stat(metadata); err != nil {
		t.Errorf("RemoveBundle() removed the registration: %v", err)
	}
	if err := RemoveBundle(configHome, "brig-dev"); err != nil {
		t.Errorf("RemoveBundle() of a missing bundle: %v", err)
	}
	if err := RemoveBundle(configHome, ".."); err == nil {
		t.Error("RemoveBundle() accepted gateway \"..\"")
	}
}
