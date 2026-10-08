// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

package ocsync

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// SecretTool looks secrets up with secret-tool from libsecret, or another
// program that takes the same `lookup ATTRIBUTE VALUE...` arguments and
// prints the secret.
type SecretTool struct {
	// Path is the program: an absolute path, or a name to look up in PATH.
	Path string
	// Timeout bounds each lookup, which may wait for the user to unlock the
	// keyring. Zero means two minutes.
	Timeout time.Duration
}

// ErrSecretNotFound means the Secret Service has no secret with the given
// attributes.
var ErrSecretNotFound = errors.New("no such secret")

// maxSecretSize bounds what brig reads from secret-tool.
const maxSecretSize = 1 << 20

// Lookup returns the secret with the attributes attrs, given as
// attribute/value pairs. A single trailing newline is removed. Neither the
// secret nor secret-tool's standard output ever appears in errors.
func (s SecretTool) Lookup(ctx context.Context, attrs []string) (string, error) {
	path := s.Path
	if path == "" {
		path = "secret-tool"
	}
	if !strings.Contains(path, "/") {
		p, err := exec.LookPath(path)
		if err != nil {
			return "", fmt.Errorf("%s not found; install it (sudo dnf install $(brig print-fedora-deps --with secrets)) or set openshell.secret_tool in config.yaml", path)
		}
		path = p
	}
	timeout := s.Timeout
	if timeout == 0 {
		timeout = 2 * time.Minute
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, path, append([]string{"lookup"}, attrs...)...)
	var stdout, stderr limitedBuffer
	stdout.max, stderr.max = maxSecretSize, 4096
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	// A wrapper script's children may keep the output pipes open after the
	// timeout killed the script itself.
	cmd.WaitDelay = time.Second
	err := cmd.Run()
	if ctx.Err() != nil {
		return "", fmt.Errorf("%s lookup %s: %w", path, strings.Join(attrs, " "), ctx.Err())
	}
	if stdout.overflow {
		return "", fmt.Errorf("%s lookup %s: secret larger than %d bytes", path, strings.Join(attrs, " "), maxSecretSize)
	}
	secret := strings.TrimSuffix(stdout.String(), "\n")
	msg := strings.TrimSpace(stderr.String())
	var exit *exec.ExitError
	switch {
	case err == nil && secret != "":
		return secret, nil
	case err == nil || (errors.As(err, &exit) && exit.ExitCode() == 1 && msg == ""):
		// secret-tool exits 1 without a message when nothing matches.
		return "", fmt.Errorf("%s lookup %s: %w; store it with: secret-tool store --label=LABEL %s",
			path, strings.Join(attrs, " "), ErrSecretNotFound, strings.Join(attrs, " "))
	case msg != "":
		return "", fmt.Errorf("%s lookup %s: %w: %s", path, strings.Join(attrs, " "), err, msg)
	default:
		return "", fmt.Errorf("%s lookup %s: %w", path, strings.Join(attrs, " "), err)
	}
}

// limitedBuffer keeps at most max bytes and notes whether more came.
type limitedBuffer struct {
	bytes.Buffer
	max      int
	overflow bool
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	if room := b.max - b.Len(); len(p) > room {
		b.overflow = true
		if room > 0 {
			b.Buffer.Write(p[:room])
		}
		return len(p), nil
	}
	return b.Buffer.Write(p)
}
