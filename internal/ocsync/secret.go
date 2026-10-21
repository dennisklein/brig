// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

package ocsync

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// SecretTool looks secrets up and stores them with secret-tool from
// libsecret, or another program that takes the same arguments: `lookup
// ATTRIBUTE VALUE...`, which prints the secret, and `store --label=LABEL
// ATTRIBUTE VALUE...`, which reads it from standard input. It puts brig's
// namespace before the attributes it is given; see KeyringAttributes.
type SecretTool struct {
	// Path is the program: an absolute path, or a name to look up in PATH.
	Path string
	// Timeout bounds each lookup, which may wait for the user to unlock the
	// keyring. Zero means two minutes.
	Timeout time.Duration
}

// The attribute/value pair that brig puts before the attributes of every
// secret it looks up or stores. A provider file, which a shared config
// directory may hold, can then name only secrets stored for brig, and not,
// say, a password that another program keeps in the same keyring.
const (
	NamespaceAttribute = "application"
	NamespaceValue     = "brig"
)

// KeyringAttributes returns the attributes under which the secret of a
// secret_tool reference is in the keyring: brig's namespace, then lookup.
func KeyringAttributes(lookup []string) []string {
	return append([]string{NamespaceAttribute, NamespaceValue}, lookup...)
}

// ErrSecretNotFound means the Secret Service has no secret with the given
// attributes.
var ErrSecretNotFound = errors.New("no such secret")

// maxSecretSize bounds what brig reads from secret-tool and from the
// environment.
const maxSecretSize = 1 << 20

// LookupEnv returns the value of the environment variable name, which must
// start with EnvSecretPrefix. Like the output of secret-tool, it must not be
// empty or larger than a mebibyte.
func LookupEnv(name string) (string, error) {
	if !envSecretRE.MatchString(name) {
		return "", fmt.Errorf("environment variable %s: the name must start with %s", name, EnvSecretPrefix)
	}
	v := os.Getenv(name)
	switch {
	case v == "":
		return "", fmt.Errorf("environment variable %s: %w", name, ErrSecretNotFound)
	case len(v) > maxSecretSize:
		return "", fmt.Errorf("environment variable %s: secret larger than %d bytes", name, maxSecretSize)
	}
	return v, nil
}

// Lookup returns the secret with the attributes attrs, given as
// attribute/value pairs, in brig's namespace. A single trailing newline is
// removed. Neither the secret nor secret-tool's standard output ever appears
// in errors.
func (s SecretTool) Lookup(ctx context.Context, attrs []string) (string, error) {
	path, err := s.program()
	if err != nil {
		return "", err
	}
	attrs = KeyringAttributes(attrs)
	ctx, cancel := s.withTimeout(ctx)
	defer cancel()
	cmd := exec.CommandContext(ctx, path, append([]string{"lookup"}, attrs...)...)
	var stdout, stderr limitedBuffer
	stdout.max, stderr.max = maxSecretSize, 4096
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	// A wrapper script's children may keep the output pipes open after the
	// timeout killed the script itself.
	cmd.WaitDelay = time.Second
	err = cmd.Run()
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
		return "", fmt.Errorf("%s lookup %s: %w; %s",
			path, strings.Join(attrs, " "), ErrSecretNotFound, storeHint(path, attrs))
	case msg != "":
		return "", fmt.Errorf("%s lookup %s: %w: %s", path, strings.Join(attrs, " "), err, msg)
	default:
		return "", fmt.Errorf("%s lookup %s: %w", path, strings.Join(attrs, " "), err)
	}
}

// Lookup returns the value of the service's secret name, from where ss
// binds it: the keyring, through secrets, or brig's environment.
func (ss ServiceSecrets) Lookup(ctx context.Context, secrets Secrets, name string) (string, error) {
	c, ok := ss.Secrets[name]
	if !ok {
		return "", fmt.Errorf("secret %s of service %s is not bound in %s", name, ss.Name, ss.Path)
	}
	var v string
	var err error
	if c.Env != "" {
		v, err = LookupEnv(c.Env)
	} else {
		v, err = secrets.Lookup(ctx, c.SecretTool.Lookup)
	}
	if err != nil {
		return "", fmt.Errorf("secret %s of service %s: %w", name, ss.Name, err)
	}
	return v, nil
}

// program returns the path of the program to run.
func (s SecretTool) program() (string, error) {
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
	return path, nil
}

// withTimeout bounds ctx by the timeout.
func (s SecretTool) withTimeout(ctx context.Context) (context.Context, context.CancelFunc) {
	timeout := s.Timeout
	if timeout == 0 {
		timeout = 2 * time.Minute
	}
	return context.WithTimeout(ctx, timeout)
}

// Store saves secret under the attributes attrs, given as attribute/value
// pairs, in brig's namespace, with the label label. The secret goes to the program on standard
// input, never on its command line, and is exactly the bytes given, so the
// caller removes a trailing newline. A program that cannot store, such as a
// wrapper that only looks up, exits non-zero; the error then names the
// attributes, so the secret can be stored by hand. Neither the secret nor
// the program's standard output ever appears in errors.
func (s SecretTool) Store(ctx context.Context, label string, attrs []string, secret string) error {
	path, err := s.program()
	if err != nil {
		return err
	}
	attrs = KeyringAttributes(attrs)
	ctx, cancel := s.withTimeout(ctx)
	defer cancel()
	cmd := exec.CommandContext(ctx, path, append([]string{"store", "--label=" + label}, attrs...)...)
	var stderr limitedBuffer
	stderr.max = 4096
	cmd.Stdin = strings.NewReader(secret)
	cmd.Stderr = &stderr
	cmd.WaitDelay = time.Second
	err = cmd.Run()
	if ctx.Err() != nil {
		return fmt.Errorf("%s store %s: %w", path, strings.Join(attrs, " "), ctx.Err())
	}
	if err == nil {
		return nil
	}
	msg := strings.TrimSpace(stderr.String())
	if secret != "" {
		msg = strings.ReplaceAll(msg, secret, "")
	}
	if msg != "" {
		msg = ": " + msg
	}
	return fmt.Errorf("%s store %s: %w%s; if %s cannot store secrets, store it yourself under these attributes: %s",
		path, strings.Join(attrs, " "), err, msg, filepath.Base(path), attributeWords(attrs))
}

// attributeWords formats attrs for pasting into a shell.
func attributeWords(attrs []string) string {
	words := make([]string, len(attrs))
	for i, a := range attrs {
		words[i] = shellWord(a)
	}
	return strings.Join(words, " ")
}

// storeHint says how to store a secret that lookup did not find. Only
// secret-tool stores into libsecret, so another program gets no such hint.
func storeHint(path string, attrs []string) string {
	if filepath.Base(path) != "secret-tool" {
		return "store it where " + path + " looks"
	}
	return "store it with: secret-tool store --label=LABEL " + attributeWords(attrs)
}

// shellWord quotes s for a POSIX shell. Words of plain characters stay as
// they are.
func shellWord(s string) string {
	plain := "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789-._/:@%+=,"
	if s != "" && strings.IndexFunc(s, func(r rune) bool { return !strings.ContainsRune(plain, r) }) < 0 {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
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
