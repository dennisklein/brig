// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

// Package sshx connects to brig VMs with the host's OpenSSH client and
// manages the keys and known_hosts files it uses.
//
// brig never trusts host keys on first use: it generates each VM's host key
// itself, pins it in a per-VM known_hosts file under a host key alias, and
// runs ssh with strict host key checking against that file only. It also
// turns off agent, X11 and port forwarding, so that a session never gives a
// VM a way back into the host.
package sshx

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"errors"
	"fmt"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/dennisklein/brig/internal/vm"
)

// GenerateKey returns a new ed25519 key as OpenSSH private key PEM and its
// public key.
func GenerateKey(comment string) (privatePEM []byte, pub ssh.PublicKey, err error) {
	pubKey, privKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, nil, fmt.Errorf("generate ed25519 key: %w", err)
	}
	block, err := ssh.MarshalPrivateKey(privKey, comment)
	if err != nil {
		return nil, nil, fmt.Errorf("encode private key: %w", err)
	}
	pub, err = ssh.NewPublicKey(pubKey)
	if err != nil {
		return nil, nil, fmt.Errorf("encode public key: %w", err)
	}
	return pem.EncodeToMemory(block), pub, nil
}

// AuthorizedKey formats pub as an authorized_keys line without a trailing
// newline. Whitespace in comment, including newlines, collapses to single
// spaces.
func AuthorizedKey(pub ssh.PublicKey, comment string) string {
	line := strings.TrimSuffix(string(ssh.MarshalAuthorizedKey(pub)), "\n")
	if comment = strings.Join(strings.Fields(comment), " "); comment != "" {
		line += " " + comment
	}
	return line
}

var aliasRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

// WriteKnownHosts atomically writes a known_hosts file with mode 0600 that
// trusts only pub, for the host key alias alias.
func WriteKnownHosts(path, alias string, pub ssh.PublicKey) error {
	// Host patterns in known_hosts treat characters such as '*', '!' and
	// ',' specially; brig's aliases never need them.
	if !aliasRE.MatchString(alias) {
		return fmt.Errorf("invalid host key alias %q", alias)
	}
	line := alias + " " + AuthorizedKey(pub, "") + "\n"
	return vm.WriteFileAtomic(path, []byte(line), 0o600)
}

// Target is an SSH login on a VM. All fields are required.
type Target struct {
	// Host is the address ssh connects to, e.g. 127.0.0.1.
	Host string
	Port int
	User string
	// IdentityFile is the private key to log in with.
	IdentityFile string
	// KnownHostsFile pins the VM's host key under HostKeyAlias (see
	// WriteKnownHosts).
	KnownHostsFile string
	// HostKeyAlias names the VM in KnownHostsFile. With an alias, ssh looks
	// up the host key without the port, so the pin survives port changes.
	HostKeyAlias string
}

// Args returns the arguments for a non-interactive ssh (BatchMode=yes) that
// runs remote on t: everything after "ssh" on the command line. The remote
// shell joins remote with spaces and interprets the result.
func (t Target) Args(remote ...string) []string { return t.args(true, remote) }

func (t Target) args(batch bool, remote []string) []string {
	args := []string{
		// Read no ssh_config at all: the user's or the system's could
		// offer the VM other identities (IdentityFile entries add to -i
		// even with IdentitiesOnly), share connections or turn on
		// forwarding. Everything brig needs is on the command line.
		"-F", "none",
		"-p", strconv.Itoa(t.Port),
		"-i", t.IdentityFile,
		"-o", "IdentitiesOnly=yes",
		"-o", "UserKnownHostsFile=" + quote(t.KnownHostsFile),
		"-o", "GlobalKnownHostsFile=/dev/null",
		"-o", "HostKeyAlias=" + t.HostKeyAlias,
		"-o", "StrictHostKeyChecking=yes",
		"-o", "HostKeyAlgorithms=ssh-ed25519",
		// The VM is a sandbox: never hand it the user's SSH agent, X
		// display or host ports. These are ssh's defaults without an
		// ssh_config; state them anyway, as command-line options take
		// precedence over any configuration.
		"-o", "ForwardAgent=no",
		"-o", "ForwardX11=no",
		"-o", "ClearAllForwardings=yes",
	}
	if batch {
		args = append(args, "-o", "BatchMode=yes")
	}
	args = append(args,
		"-o", "ConnectTimeout=5",
		"-o", "LogLevel=ERROR",
		// ssh parses options that follow the destination unless "--"
		// ends them, so a remote command could otherwise inject options.
		"--", t.User+"@"+t.Host)
	return append(args, remote...)
}

// quote protects a path in an ssh -o option value, which ssh splits like a
// shell would: UserKnownHostsFile takes a list of files.
func quote(s string) string {
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(s) + `"`
}

// Command returns a non-interactive ssh command (BatchMode=yes) that runs
// remote on t. Callers may attach stdin and stdout before starting it.
func (t Target) Command(ctx context.Context, remote ...string) *exec.Cmd {
	return exec.CommandContext(ctx, "ssh", t.Args(remote...)...)
}

// Interactive returns the full argv, starting with "ssh", for exec'ing an
// interactive ssh on t. Without BatchMode, ssh may prompt, e.g. for the
// identity's passphrase. Like plain ssh, it allocates a terminal only when
// remote is empty.
func (t Target) Interactive(remote ...string) []string {
	return append([]string{"ssh"}, t.args(false, remote)...)
}

// Output runs remote on t and returns its standard output. A failure's error
// includes what ssh and the remote command wrote to standard error and wraps
// the *exec.ExitError carrying the exit status (255 for ssh's own errors).
func (t Target) Output(ctx context.Context, remote string) ([]byte, error) {
	cmd := t.Command(ctx, remote)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			err = fmt.Errorf("%w: %s", err, msg)
		}
		return out, fmt.Errorf("ssh %s@%s:%d: %w", t.User, t.Host, t.Port, err)
	}
	return out, nil
}

// WaitReady polls t with `true`, backing off between attempts, until a login
// succeeds or ctx is done. It gives up at once when ssh cannot be run or the
// host key does not match, which waiting cannot fix.
func (t Target) WaitReady(ctx context.Context) error {
	delay := 250 * time.Millisecond
	for {
		_, err := t.Output(ctx, "true")
		if err == nil {
			return nil
		}
		var exitErr *exec.ExitError
		if ctx.Err() == nil && (!errors.As(err, &exitErr) ||
			strings.Contains(err.Error(), "Host key verification failed")) {
			return err
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("wait for ssh: %w: %w", ctx.Err(), err)
		case <-time.After(delay):
		}
		delay = min(2*delay, 2*time.Second)
	}
}
