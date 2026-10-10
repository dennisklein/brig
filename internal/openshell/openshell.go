// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

// Package openshell drives the host's openshell CLI for brig VMs: it
// registers each VM's OpenShell gateway, keeps the gateway's mTLS client
// bundle in the CLI's configuration directory up to date, and manages the
// gateway's provider profiles and providers.
//
// The layout of that directory follows OpenShell v0.1.2:
// <ConfigHome>/active_gateway names the selected gateway and
// <ConfigHome>/gateways/<name>/ holds a gateway's metadata.json and its
// client bundle in mtls/.
package openshell

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/dennisklein/brig/internal/vm"
)

// InstallHint tells users how to install the openshell CLI from the brig dnf
// repository. Weak dependencies are skipped because the CLI package
// recommends podman, which the host does not need.
const InstallHint = "sudo rpm --import https://dennisklein.github.io/brig/RPM-GPG-KEY-brig && " +
	"sudo dnf install https://dennisklein.github.io/brig/brig-release.noarch.rpm && " +
	"sudo dnf install --setopt=install_weak_deps=False openshell"

// ErrNotInstalled is returned by Find when openshell is not in PATH.
var ErrNotInstalled = errors.New("openshell CLI not found; install it from the brig repository: " + InstallHint)

// CLI runs the host's openshell CLI.
type CLI struct {
	// Path is the openshell executable.
	Path string
	// ConfigHome is the CLI's configuration directory,
	// $XDG_CONFIG_HOME/openshell. brig runs the CLI with XDG_CONFIG_HOME set
	// to its parent, so that both agree on where gateways are registered.
	ConfigHome string
}

// Find locates openshell in PATH and its configuration directory as the CLI
// resolves it: $XDG_CONFIG_HOME/openshell, by default ~/.config/openshell.
func Find() (*CLI, error) {
	path, err := exec.LookPath("openshell")
	if err != nil {
		return nil, ErrNotInstalled
	}
	base := os.Getenv("XDG_CONFIG_HOME")
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, err
		}
		base = filepath.Join(home, ".config")
	} else if !filepath.IsAbs(base) {
		return nil, errors.New("XDG_CONFIG_HOME must be an absolute path")
	}
	return &CLI{Path: path, ConfigHome: filepath.Join(base, "openshell")}, nil
}

// Version returns the CLI's version, e.g. "0.1.2".
func (c *CLI) Version(ctx context.Context) (string, error) {
	out, err := c.run(ctx, "--version")
	if err != nil {
		return "", err
	}
	f := strings.Fields(string(out))
	if len(f) != 2 || f[0] != "openshell" {
		return "", fmt.Errorf("unexpected output of openshell --version: %q", out)
	}
	return f[1], nil
}

// GatewayVersion asks the registered gateway for its version. It fails
// unless the CLI reaches the gateway over mTLS, so it doubles as a readiness
// check.
func (c *CLI) GatewayVersion(ctx context.Context, gateway string) (string, error) {
	if _, err := gatewayDir(c.ConfigHome, gateway); err != nil {
		return "", err
	}
	// status exits 0 for an unknown gateway and for one that answers HTTP
	// but not gRPC, and tells them apart in its output. It fails when it
	// cannot read the client bundle or connect at all.
	out, err := c.run(ctx, "-g", gateway, "status", "-o", "json")
	if err != nil {
		return "", err
	}
	var st struct {
		Status  string `json:"status"`
		Version string `json:"version"`
		Error   string `json:"error"`
	}
	if err := json.NewDecoder(bytes.NewReader(jsonObject(out))).Decode(&st); err != nil {
		return "", fmt.Errorf("parse openshell status: %w", err)
	}
	switch {
	case st.Status == "connected" && st.Version != "":
		return st.Version, nil
	case st.Status == "not_configured":
		return "", fmt.Errorf("gateway %s is not registered", gateway)
	case st.Error != "":
		return "", fmt.Errorf("gateway %s is %s: %s", gateway, st.Status, st.Error)
	default:
		return "", fmt.Errorf("gateway %s is %s", gateway, st.Status)
	}
}

// jsonObject skips log lines that the CLI writes to standard output before
// a pretty-printed JSON object.
func jsonObject(out []byte) []byte {
	if i := bytes.Index(out, []byte("\n{")); i >= 0 && !bytes.HasPrefix(out, []byte("{")) {
		return out[i+1:]
	}
	return out
}

// Register registers the gateway listening on 127.0.0.1:port under the name
// gateway, replacing an existing registration of that name. The gateway's
// client bundle must already be in MTLSDir (see SyncBundle). If adding the
// gateway fails after an old registration was removed, the gateway is left
// unregistered. The CLI's active gateway stays as it was, although
// registering selects the new gateway. Register waits while another brig
// process registers or unregisters a gateway.
func (c *CLI) Register(ctx context.Context, gateway string, port int) (err error) {
	dir, err := gatewayDir(c.ConfigHome, gateway)
	if err != nil {
		return err
	}
	if port < 1 || port > 65535 {
		return fmt.Errorf("invalid gateway port %d", port)
	}
	unlock, err := c.lock(ctx)
	if err != nil {
		return err
	}
	defer unlock()
	restore, err := c.keepActive(gateway)
	if err != nil {
		return err
	}
	defer func() {
		if rerr := restore(); rerr != nil {
			err = errors.Join(err, fmt.Errorf("restore the active openshell gateway: %w", rerr))
		}
	}()
	// gateway add refuses names that are already registered.
	if registered(dir) {
		if _, err := c.run(ctx, "gateway", "remove", gateway); err != nil {
			return err
		}
	}
	// --remote registers an mTLS gateway whose client bundle is on disk;
	// the SSH destination it names is only shown in listings. gateway add
	// checks that the gateway answers, which a stalled VM could drag out
	// while every other brig process waits for the lock.
	ctx, cancel := context.WithTimeout(ctx, registerTimeout)
	defer cancel()
	_, err = c.run(ctx, "gateway", "add", "https://127.0.0.1:"+strconv.Itoa(port),
		"--remote", gateway, "--name", gateway)
	return err
}

// registerTimeout bounds `openshell gateway add`.
const registerTimeout = 2 * time.Minute

// Unregister removes the gateway's registration and everything the CLI keeps
// about it, including its client bundle. A gateway that is not registered is
// not an error. Like Register, it waits for other brig processes.
func (c *CLI) Unregister(ctx context.Context, gateway string) error {
	dir, err := gatewayDir(c.ConfigHome, gateway)
	if err != nil {
		return err
	}
	unlock, err := c.lock(ctx)
	if err != nil {
		return err
	}
	defer unlock()
	if registered(dir) {
		if _, err := c.run(ctx, "gateway", "remove", gateway); err != nil {
			return err
		}
	}
	// gateway remove deletes only metadata.json and auth tokens.
	return os.RemoveAll(dir)
}

// lock serializes Register and Unregister across brig processes. Register
// restores the active gateway it found before adding its own, which a
// concurrent registration would otherwise overwrite or leave pointing at a
// removed gateway. The lock is an flock on ConfigHome, held until unlock is
// called; lock waits for it until ctx is done.
func (c *CLI) lock(ctx context.Context) (unlock func(), err error) {
	if err := os.MkdirAll(c.ConfigHome, 0o700); err != nil {
		return nil, err
	}
	f, err := os.Open(c.ConfigHome)
	if err != nil {
		return nil, err
	}
	for {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return func() { _ = f.Close() }, nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EINTR) {
			_ = f.Close()
			return nil, fmt.Errorf("lock %s: %w", c.ConfigHome, err)
		}
		select {
		case <-ctx.Done():
			_ = f.Close()
			return nil, fmt.Errorf("lock %s: %w", c.ConfigHome, ctx.Err())
		case <-time.After(50 * time.Millisecond):
		}
	}
}

// registered reports whether the gateway in dir has a per-user registration.
// Like the CLI, it assumes one exists when that cannot be determined.
func registered(dir string) bool {
	_, err := os.Stat(filepath.Join(dir, "metadata.json"))
	return !errors.Is(err, fs.ErrNotExist)
}

// keepActive saves the active gateway selection and returns a function that
// restores it if registering gateway selected it. A selection that the
// user's own openshell commands made meanwhile, which take no lock, stays.
func (c *CLI) keepActive(gateway string) (restore func() error, err error) {
	path := filepath.Join(c.ConfigHome, "active_gateway")
	selectedByUs := func() bool {
		cur, err := os.ReadFile(path)
		return err == nil && strings.TrimSpace(string(cur)) == gateway
	}
	fi, err := os.Stat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return func() error {
			if !selectedByUs() {
				return nil
			}
			if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
				return err
			}
			return nil
		}, nil
	}
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return func() error {
		if !selectedByUs() {
			return nil
		}
		return vm.WriteFileAtomic(path, data, fi.Mode().Perm())
	}, nil
}

// gatewayEnv lists variables that could point the CLI at another gateway than
// the one brig names, make it skip verifying the gateway's certificate, or
// select another workspace than the gateway's default one.
var gatewayEnv = []string{"OPENSHELL_GATEWAY", "OPENSHELL_GATEWAY_ENDPOINT", "OPENSHELL_GATEWAY_INSECURE", "OPENSHELL_WORKSPACE"}

// run runs the CLI and returns its standard output. Errors include what the
// CLI reported.
func (c *CLI) run(ctx context.Context, args ...string) ([]byte, error) {
	return c.runEnv(ctx, nil, args...)
}

// runEnv is run with the additional environment variables env, each
// KEY=VALUE, which replace inherited variables of the same name.
func (c *CLI) runEnv(ctx context.Context, env []string, args ...string) ([]byte, error) {
	if !filepath.IsAbs(c.ConfigHome) || filepath.Base(c.ConfigHome) != "openshell" {
		return nil, fmt.Errorf("openshell config home %q is not an absolute path ending in /openshell", c.ConfigHome)
	}
	extra := map[string]bool{}
	for _, kv := range env {
		name, _, _ := strings.Cut(kv, "=")
		extra[name] = true
	}
	cmd := exec.CommandContext(ctx, c.Path, args...)
	cmd.Env = []string{"XDG_CONFIG_HOME=" + filepath.Dir(c.ConfigHome)}
	for _, kv := range os.Environ() {
		name, _, _ := strings.Cut(kv, "=")
		if name != "XDG_CONFIG_HOME" && !slices.Contains(gatewayEnv, name) && !extra[name] {
			cmd.Env = append(cmd.Env, kv)
		}
	}
	cmd.Env = append(cmd.Env, env...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = strings.TrimSpace(string(out))
		}
		if msg != "" {
			err = fmt.Errorf("%w: %s", err, msg)
		}
		return nil, fmt.Errorf("openshell %s: %w", strings.Join(args, " "), err)
	}
	return out, nil
}

var nameRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

// gatewayDir returns where the CLI keeps what it knows about gateway.
func gatewayDir(configHome, gateway string) (string, error) {
	// The CLI stores a gateway under its name as a single path component
	// and maps other characters to '_' when it looks for the bundle.
	if !nameRE.MatchString(gateway) {
		return "", fmt.Errorf("invalid gateway name %q", gateway)
	}
	if !filepath.IsAbs(configHome) {
		return "", fmt.Errorf("openshell config home %q is not absolute", configHome)
	}
	return filepath.Join(configHome, "gateways", gateway), nil
}

// CompatibleVersions reports whether the host CLI's and the gateway's
// versions share major and minor version. OpenShell releases with another
// minor version may change the protocol.
func CompatibleVersions(host, gateway string) bool {
	h, ok := majorMinor(host)
	g, ok2 := majorMinor(gateway)
	return ok && ok2 && h == g
}

func majorMinor(version string) ([2]int, bool) {
	var mm [2]int
	parts := strings.SplitN(strings.TrimPrefix(version, "v"), ".", 3)
	if len(parts) < 2 {
		return mm, false
	}
	for i := range mm {
		n, err := strconv.Atoi(parts[i])
		if err != nil || n < 0 {
			return mm, false
		}
		mm[i] = n
	}
	return mm, true
}
