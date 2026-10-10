// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

package sshx

import (
	"bytes"
	"context"
	"errors"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
)

func TestGenerateKey(t *testing.T) {
	privPEM, pub, err := GenerateKey("brig dev")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix(privPEM, []byte("-----BEGIN OPENSSH PRIVATE KEY-----\n")) {
		t.Fatalf("private key is not OpenSSH PEM:\n%s", privPEM)
	}
	signer, err := ssh.ParsePrivateKey(privPEM)
	if err != nil {
		t.Fatal(err)
	}
	if pub.Type() != ssh.KeyAlgoED25519 {
		t.Errorf("key type = %s", pub.Type())
	}
	if !bytes.Equal(signer.PublicKey().Marshal(), pub.Marshal()) {
		t.Error("public key does not belong to the private key")
	}
	_, pub2, err := GenerateKey("")
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(pub.Marshal(), pub2.Marshal()) {
		t.Error("two keys are identical")
	}
}

func TestAuthorizedKey(t *testing.T) {
	_, pub, err := GenerateKey("")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ comment, want string }{
		{"", ""},
		{"brig dev", "brig dev"},
		{" brig\ndev\r\n", "brig dev"},
	} {
		line := AuthorizedKey(pub, tc.comment)
		if strings.ContainsAny(line, "\r\n") || strings.HasSuffix(line, " ") {
			t.Errorf("AuthorizedKey(%q) = %q", tc.comment, line)
		}
		got, comment, _, rest, err := ssh.ParseAuthorizedKey([]byte(line))
		if err != nil || len(rest) != 0 {
			t.Fatalf("ParseAuthorizedKey(%q): %v, rest %q", line, err, rest)
		}
		if !bytes.Equal(got.Marshal(), pub.Marshal()) || comment != tc.want {
			t.Errorf("AuthorizedKey(%q) = %q, comment %q, want %q", tc.comment, line, comment, tc.want)
		}
	}
}

func TestWriteKnownHosts(t *testing.T) {
	_, pub, err := GenerateKey("")
	if err != nil {
		t.Fatal(err)
	}
	_, other, err := GenerateKey("")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "known_hosts")
	if err := os.WriteFile(path, []byte("stale\n"), 0o644); err != nil { //nolint:gosec // G306: a too-open file that WriteKnownHosts must tighten
		t.Fatal(err)
	}
	if err := WriteKnownHosts(path, "brig-dev", pub); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if want := "brig-dev " + AuthorizedKey(pub, "") + "\n"; string(data) != want {
		t.Fatalf("known_hosts = %q, want %q", data, want)
	}
	if fi, err := os.Stat(path); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %v, %v; want 0600", fi.Mode().Perm(), err)
	}

	// ssh looks up "brig-dev" when HostKeyAlias=brig-dev; knownhosts
	// normalizes "brig-dev:22" to that.
	check, err := knownhosts.New(path)
	if err != nil {
		t.Fatal(err)
	}
	addr := &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 40022}
	if err := check("brig-dev:22", addr, pub); err != nil {
		t.Errorf("pinned key rejected: %v", err)
	}
	if err := check("brig-dev:22", addr, other); err == nil {
		t.Error("other key accepted")
	}
	if err := check("brig-prod:22", addr, pub); err == nil {
		t.Error("key accepted for another alias")
	}

	for _, bad := range []string{"", "*", "a,b", "!a", "[a]:22", "a b", "-a", "|1|x"} {
		if err := WriteKnownHosts(path, bad, pub); err == nil {
			t.Errorf("WriteKnownHosts accepted alias %q", bad)
		}
	}
}

func target() Target {
	return Target{
		Host: "127.0.0.1", Port: 40022, User: "agent",
		IdentityFile: "/data/vms/dev/id_ed25519", KnownHostsFile: "/data/my vms/dev/known_hosts",
		HostKeyAlias: "brig-dev",
	}
}

func TestArgs(t *testing.T) {
	want := []string{
		"-F", "none",
		"-p", "40022",
		"-i", "/data/vms/dev/id_ed25519",
		"-o", "IdentitiesOnly=yes",
		"-o", `UserKnownHostsFile="/data/my vms/dev/known_hosts"`,
		"-o", "GlobalKnownHostsFile=/dev/null",
		"-o", "HostKeyAlias=brig-dev",
		"-o", "StrictHostKeyChecking=yes",
		"-o", "HostKeyAlgorithms=ssh-ed25519",
		"-o", "ForwardAgent=no",
		"-o", "ForwardX11=no",
		"-o", "ClearAllForwardings=yes",
		"-o", "BatchMode=yes",
		"-o", "ConnectTimeout=5",
		"-o", "LogLevel=ERROR",
		"--", "agent@127.0.0.1",
		"podman", "load",
	}
	if got := target().Args("podman", "load"); !reflect.DeepEqual(got, want) {
		t.Errorf("Args() =\n%q\nwant\n%q", got, want)
	}

	interactive := target().Interactive()
	if interactive[0] != "ssh" || interactive[len(interactive)-1] != "agent@127.0.0.1" {
		t.Errorf("Interactive() = %q", interactive)
	}
	if strings.Contains(strings.Join(interactive, " "), "BatchMode") {
		t.Errorf("Interactive() uses BatchMode: %q", interactive)
	}
	if !reflect.DeepEqual(target().Command(context.Background(), "true").Args[1:], target().Args("true")) {
		t.Error("Command() does not use Args()")
	}
}

func TestQuote(t *testing.T) {
	if got, want := quote(`/a "b"\c`), `"/a \"b\"\\c"`; got != want {
		t.Errorf("quote() = %s, want %s", got, want)
	}
}

// fakeSSH puts an ssh script with the given body first in PATH. The script
// logs its arguments, one per line, to the returned file.
func fakeSSH(t *testing.T, body string) (log string) {
	t.Helper()
	dir := t.TempDir()
	log = filepath.Join(dir, "log")
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" >>'" + log + "'\n" + body + "\n"
	if err := os.WriteFile(filepath.Join(dir, "ssh"), []byte(script), 0o755); err != nil { //nolint:gosec // G306: the fake must be executable
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return log
}

func TestOutput(t *testing.T) {
	log := fakeSSH(t, `echo hello; echo noise >&2`)
	out, err := target().Output(context.Background(), "cat /etc/hostname")
	if err != nil || string(out) != "hello\n" {
		t.Fatalf("Output() = %q, %v", out, err)
	}
	args, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(string(args), "\nBatchMode=yes\n-o\nConnectTimeout=5\n-o\nLogLevel=ERROR\n--\nagent@127.0.0.1\ncat /etc/hostname\n") {
		t.Errorf("ssh args:\n%s", args)
	}
}

func TestOutputError(t *testing.T) {
	fakeSSH(t, `echo 'Permission denied (publickey).' >&2; exit 255`)
	_, err := target().Output(context.Background(), "true")
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 255 {
		t.Fatalf("Output() error = %v, want exit status 255", err)
	}
	if want := "ssh agent@127.0.0.1:40022: exit status 255: Permission denied (publickey)."; err.Error() != want {
		t.Errorf("error = %q, want %q", err, want)
	}
}

func TestOutputLimit(t *testing.T) {
	fakeSSH(t, `exec head -c 20000000 /dev/zero`)
	_, err := target().Output(context.Background(), "cat big")
	if err == nil || !strings.Contains(err.Error(), "wrote more than") {
		t.Fatalf("Output() error = %v, want the limit", err)
	}
}

func TestWaitReady(t *testing.T) {
	dir := t.TempDir()
	// Fail twice, then succeed.
	fakeSSH(t, `n=$(cat '`+dir+`/n' 2>/dev/null || echo 0)
echo $((n + 1)) >'`+dir+`/n'
[ "$n" -ge 2 ] || { echo 'Connection refused' >&2; exit 255; }`)
	if err := target().WaitReady(context.Background()); err != nil {
		t.Fatal(err)
	}
	n, err := os.ReadFile(filepath.Join(dir, "n"))
	if err != nil || string(n) != "3\n" {
		t.Errorf("ssh ran %q times, want 3", n)
	}
}

func TestWaitReadyTimeout(t *testing.T) {
	fakeSSH(t, `echo 'Connection refused' >&2; exit 255`)
	ctx, cancel := context.WithTimeout(context.Background(), 600*time.Millisecond)
	defer cancel()
	err := target().WaitReady(ctx)
	if !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), "Connection refused") {
		t.Fatalf("WaitReady() = %v, want deadline exceeded with the last error", err)
	}
}

func TestWaitReadyNoSSH(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	err := target().WaitReady(ctx)
	if !errors.Is(err, exec.ErrNotFound) || ctx.Err() != nil {
		t.Fatalf("WaitReady() = %v, want exec.ErrNotFound at once", err)
	}
}

func TestWaitReadyHostKeyMismatch(t *testing.T) {
	log := fakeSSH(t, `echo 'Host key verification failed.' >&2; exit 255`)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	err := target().WaitReady(ctx)
	if err == nil || errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), "Host key verification failed") {
		t.Fatalf("WaitReady() = %v", err)
	}
	if args, _ := os.ReadFile(log); strings.Count(string(args), "agent@127.0.0.1\n") != 1 {
		t.Errorf("WaitReady() retried a host key mismatch:\n%s", args)
	}
}
