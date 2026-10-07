// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

package sshx

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"
)

// The tests in this file run the host's OpenSSH client against an
// in-process SSH server, so that the options brig passes are checked by ssh
// itself. They are skipped where ssh is not installed.

// sshServer accepts one client key, answers each exec request by echoing
// the command it received, and records the type of every request and the
// fingerprint of every key the client offers.
type sshServer struct {
	port      int
	mu        sync.Mutex
	requests  []string
	offered   []string
	rejectAll bool
}

func (s *sshServer) record(typ string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.requests = append(s.requests, typ)
}

// seen returns the types of the requests received so far.
func (s *sshServer) seen() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.requests)
}

func startSSHServer(t *testing.T, hostKey ssh.Signer, clientKey ssh.PublicKey) *sshServer {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.Close() })
	s := &sshServer{port: l.Addr().(*net.TCPAddr).Port}
	cfg := &ssh.ServerConfig{
		//nolint:gosec // G408: the recorded keys are test output; the decision depends on key and rejectAll only
		PublicKeyCallback: func(_ ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
			s.mu.Lock()
			defer s.mu.Unlock()
			if fp := ssh.FingerprintSHA256(key); !slices.Contains(s.offered, fp) {
				s.offered = append(s.offered, fp)
			}
			if !s.rejectAll && bytes.Equal(key.Marshal(), clientKey.Marshal()) {
				return nil, nil
			}
			return nil, errors.New("unknown key")
		},
	}
	cfg.AddHostKey(hostKey)
	go func() {
		for {
			conn, err := l.Accept()
			if err != nil {
				return
			}
			go s.serve(conn, cfg)
		}
	}()
	return s
}

func (s *sshServer) serve(conn net.Conn, cfg *ssh.ServerConfig) {
	defer func() { _ = conn.Close() }()
	sconn, chans, reqs, err := ssh.NewServerConn(conn, cfg)
	if err != nil {
		return
	}
	defer func() { _ = sconn.Close() }()
	go func() {
		for req := range reqs {
			s.record(req.Type)
			if req.WantReply {
				_ = req.Reply(false, nil)
			}
		}
	}()
	for nc := range chans {
		if nc.ChannelType() != "session" {
			_ = nc.Reject(ssh.UnknownChannelType, "")
			continue
		}
		ch, creqs, err := nc.Accept()
		if err != nil {
			return
		}
		go s.session(ch, creqs)
	}
}

func (s *sshServer) session(ch ssh.Channel, reqs <-chan *ssh.Request) {
	defer func() { _ = ch.Close() }()
	for req := range reqs {
		s.record(req.Type)
		if req.Type != "exec" {
			if req.WantReply {
				_ = req.Reply(false, nil)
			}
			continue
		}
		var payload struct{ Command string }
		ok := ssh.Unmarshal(req.Payload, &payload) == nil
		_ = req.Reply(ok, nil)
		if ok {
			_, _ = io.WriteString(ch, payload.Command)
			_, _ = ch.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{0}))
		}
		return
	}
}

// openSSHTarget starts an SSH server and returns a Target for it whose files
// live in a directory with characters that ssh_config treats specially.
func openSSHTarget(t *testing.T) (Target, *sshServer) {
	t.Helper()
	if _, err := exec.LookPath("ssh"); err != nil {
		t.Skip("OpenSSH client not installed")
	}
	hostPEM, hostPub, err := GenerateKey("")
	if err != nil {
		t.Fatal(err)
	}
	hostKey, err := ssh.ParsePrivateKey(hostPEM)
	if err != nil {
		t.Fatal(err)
	}
	clientPEM, clientPub, err := GenerateKey("")
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(t.TempDir(), `my "vms" #1`)
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	tg := Target{
		Host: "127.0.0.1", User: "agent",
		IdentityFile:   filepath.Join(dir, "id_ed25519"),
		KnownHostsFile: filepath.Join(dir, "known_hosts"),
		HostKeyAlias:   "brig-dev",
	}
	if err := os.WriteFile(tg.IdentityFile, clientPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := WriteKnownHosts(tg.KnownHostsFile, tg.HostKeyAlias, hostPub); err != nil {
		t.Fatal(err)
	}
	srv := startSSHServer(t, hostKey, clientPub)
	tg.Port = srv.port
	return tg, srv
}

func TestOpenSSH(t *testing.T) {
	tg, _ := openSSHTarget(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := tg.WaitReady(ctx); err != nil {
		t.Fatal(err)
	}
	// A remote command that looks like an ssh option reaches the server.
	out, err := tg.Output(ctx, "-V podman load")
	if err != nil || string(out) != "-V podman load" {
		t.Errorf("Output() = %q, %v", out, err)
	}
	out, err = tg.Command(ctx, "printf", "%s|", "a b").Output()
	if err != nil || string(out) != "printf %s| a b" {
		t.Errorf("Command().Output() = %q, %v", out, err)
	}
}

func TestOpenSSHHostKeyMismatch(t *testing.T) {
	tg, srv := openSSHTarget(t)
	_, other, err := GenerateKey("")
	if err != nil {
		t.Fatal(err)
	}
	if err := WriteKnownHosts(tg.KnownHostsFile, tg.HostKeyAlias, other); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	err = tg.WaitReady(ctx)
	if err == nil || ctx.Err() != nil || !strings.Contains(err.Error(), "Host key verification failed") {
		t.Fatalf("WaitReady() = %v, want a host key verification failure", err)
	}
	if got := srv.seen(); len(got) != 0 {
		t.Errorf("server got requests %q from a client that should not trust it", got)
	}
}

// TestOpenSSHNoForwarding checks that an ssh_config that forwards the agent,
// X11 and ports to every host does not apply to brig's connections.
func TestOpenSSHNoForwarding(t *testing.T) {
	tg, srv := openSSHTarget(t)
	dir := t.TempDir()
	config := filepath.Join(dir, "ssh_config")
	if err := os.WriteFile(config, []byte(`Host *
	ForwardAgent yes
	ForwardX11 yes
	ForwardX11Trusted yes
	RemoteForward 127.0.0.1:0 127.0.0.1:1
`), 0o600); err != nil {
		t.Fatal(err)
	}
	// ssh forwards only an agent it can reach, and asks it for keys.
	sock, err := net.Listen("unix", filepath.Join(dir, "agent"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sock.Close() }()
	go func() {
		keyring := agent.NewKeyring()
		for {
			conn, err := sock.Accept()
			if err != nil {
				return
			}
			go func() {
				defer func() { _ = conn.Close() }()
				_ = agent.ServeAgent(keyring, conn)
			}()
		}
	}()
	t.Setenv("SSH_AUTH_SOCK", sock.Addr().String())
	t.Setenv("DISPLAY", ":99")
	forwarding := []string{"auth-agent-req@openssh.com", "x11-req", "tcpip-forward"}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	run := func(args ...string) []string {
		t.Helper()
		cmd := exec.CommandContext(ctx, "ssh", args...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("ssh: %v\n%s", err, out)
		}
		return srv.seen()
	}

	// The control run reads the ssh_config after all, as ssh takes the last
	// -F, and undoes brig's forwarding options, as options that come first
	// win. The server handles global requests such as tcpip-forward
	// concurrently with the session, so wait for all of them before
	// starting the next run.
	control := slices.Clone(tg.Args("true"))
	if control[0] != "-F" {
		t.Fatalf("Args() = %q, want -F first", control)
	}
	control[1] = config
	seen := run(append([]string{"-o", "ForwardAgent=yes", "-o", "ForwardX11=yes", "-o", "ClearAllForwardings=no"}, control...)...)
	for _, req := range forwarding {
		for !slices.Contains(seen, req) {
			if ctx.Err() != nil {
				t.Fatalf("control run: server did not get %s, only %q", req, seen)
			}
			time.Sleep(10 * time.Millisecond)
			seen = srv.seen()
		}
	}
	srv.mu.Lock()
	srv.requests = nil
	srv.mu.Unlock()

	seen = run(append([]string{"-F", config}, tg.Args("true")...)...)
	if !slices.Contains(seen, "exec") {
		t.Fatalf("server got %q, want an exec request", seen)
	}
	for _, req := range forwarding {
		if slices.Contains(seen, req) {
			t.Errorf("server got %s", req)
		}
	}
}

// TestOpenSSHOffersOnlyTheVMKey checks that a VM that rejects brig's key
// never gets to see the user's other keys, which an ssh_config would offer
// next despite IdentitiesOnly.
func TestOpenSSHOffersOnlyTheVMKey(t *testing.T) {
	tg, srv := openSSHTarget(t)
	srv.mu.Lock()
	srv.rejectAll = true
	srv.mu.Unlock()
	dir := t.TempDir()
	personalPEM, personal, err := GenerateKey("")
	if err != nil {
		t.Fatal(err)
	}
	personalFile := filepath.Join(dir, "id_personal")
	if err := os.WriteFile(personalFile, personalPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	config := filepath.Join(dir, "ssh_config")
	if err := os.WriteFile(config, []byte("Host *\n\tIdentityFile "+personalFile+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	offered := func(args []string) []string {
		t.Helper()
		srv.mu.Lock()
		srv.offered = nil
		srv.mu.Unlock()
		if err := exec.CommandContext(ctx, "ssh", args...).Run(); err == nil {
			t.Fatal("ssh logged in although the server rejects every key")
		}
		srv.mu.Lock()
		defer srv.mu.Unlock()
		return slices.Clone(srv.offered)
	}

	// ssh takes the last -F, so this reads the ssh_config after all.
	control := slices.Clone(tg.Args("true"))
	if control[0] != "-F" {
		t.Fatalf("Args() = %q, want -F first", control)
	}
	control[1] = config
	if got := offered(control); !slices.Contains(got, ssh.FingerprintSHA256(personal)) {
		t.Fatalf("control run: ssh offered %q, not the key from ssh_config", got)
	}

	got := offered(append([]string{"-F", config}, tg.Args("true")...))
	if slices.Contains(got, ssh.FingerprintSHA256(personal)) || len(got) != 1 {
		t.Errorf("ssh offered %q, want only the VM key", got)
	}
}
