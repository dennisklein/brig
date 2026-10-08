// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

package openshell

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// writeScript writes an executable shell script that appends its arguments
// as one line to log before running body.
func writeScript(t *testing.T, path, log, body string) {
	t.Helper()
	script := "#!/bin/sh\necho \"$*\" >>'" + log + "'\n" + body + "\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil { //nolint:gosec // G306: the fake must be executable
		t.Fatal(err)
	}
}

// fakeCLI returns a CLI whose executable is a script running body, and the
// file the script logs its arguments to.
func fakeCLI(t *testing.T, body string) (c *CLI, log string) {
	t.Helper()
	dir := t.TempDir()
	log = filepath.Join(dir, "log")
	c = &CLI{Path: filepath.Join(dir, "openshell"), ConfigHome: filepath.Join(dir, "config", "openshell")}
	writeScript(t, c.Path, log, body)
	return c, log
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	return string(data)
}

func writeFile(t *testing.T, path, data string, perm os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(data), perm); err != nil {
		t.Fatal(err)
	}
}

func TestFind(t *testing.T) {
	bin := t.TempDir()
	t.Setenv("PATH", bin)
	if _, err := Find(); !errors.Is(err, ErrNotInstalled) {
		t.Fatalf("Find() without openshell = %v, want ErrNotInstalled", err)
	}
	if !strings.Contains(ErrNotInstalled.Error(), "dnf install") {
		t.Errorf("ErrNotInstalled lacks an install hint: %v", ErrNotInstalled)
	}

	writeScript(t, filepath.Join(bin, "openshell"), os.DevNull, "")
	t.Setenv("HOME", "/home/u")
	for _, tc := range []struct{ xdg, want string }{
		{"", "/home/u/.config/openshell"},
		{"/cfg", "/cfg/openshell"},
	} {
		t.Setenv("XDG_CONFIG_HOME", tc.xdg)
		c, err := Find()
		if err != nil {
			t.Fatal(err)
		}
		if c.Path != filepath.Join(bin, "openshell") || c.ConfigHome != tc.want {
			t.Errorf("XDG_CONFIG_HOME=%q: Find() = %+v, want ConfigHome %s", tc.xdg, c, tc.want)
		}
	}
	t.Setenv("XDG_CONFIG_HOME", "cfg")
	if _, err := Find(); err == nil {
		t.Error("Find() accepted a relative XDG_CONFIG_HOME")
	}
}

func TestVersion(t *testing.T) {
	c, log := fakeCLI(t, `echo "openshell 0.1.2"`)
	v, err := c.Version(context.Background())
	if err != nil || v != "0.1.2" {
		t.Fatalf("Version() = %q, %v", v, err)
	}
	if got := readFile(t, log); got != "--version\n" {
		t.Errorf("args = %q", got)
	}

	c, _ = fakeCLI(t, `echo "something else"`)
	if v, err := c.Version(context.Background()); err == nil {
		t.Errorf("Version() = %q for unexpected output", v)
	}
}

func TestRunEnvironment(t *testing.T) {
	c, _ := fakeCLI(t, `env | grep -E '^(XDG_CONFIG_HOME|OPENSHELL_)' | sort`)
	t.Setenv("XDG_CONFIG_HOME", "/elsewhere")
	t.Setenv("OPENSHELL_GATEWAY", "other")
	t.Setenv("OPENSHELL_GATEWAY_ENDPOINT", "https://example.com")
	t.Setenv("OPENSHELL_GATEWAY_INSECURE", "1")
	t.Setenv("OPENSHELL_WORKSPACE", "team")
	t.Setenv("OPENSHELL_COLOR", "never")
	out, err := c.run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := "OPENSHELL_COLOR=never\nXDG_CONFIG_HOME=" + filepath.Dir(c.ConfigHome) + "\n"
	if string(out) != want {
		t.Errorf("environment:\n%s\nwant:\n%s", out, want)
	}

	for _, home := range []string{"", "relative/openshell", "/cfg/other"} {
		c.ConfigHome = home
		if _, err := c.run(context.Background()); err == nil {
			t.Errorf("run() accepted ConfigHome %q", home)
		}
	}
}

func TestRunError(t *testing.T) {
	c, _ := fakeCLI(t, `echo "  × No gateway metadata found for 'x'." >&2; exit 1`)
	_, err := c.run(context.Background(), "gateway", "remove", "x")
	want := "openshell gateway remove x: exit status 1: × No gateway metadata found for 'x'."
	if err == nil || err.Error() != want {
		t.Errorf("run() = %v, want %s", err, want)
	}
}

func TestGatewayVersion(t *testing.T) {
	for _, tc := range []struct {
		name, out, want, wantErr string
	}{
		{
			name: "connected",
			out: `2026-10-07T12:00:00Z  WARN openshell: something
{
  "authentication": {"provider": "mTLS transport", "status": "authenticated"},
  "gateway": "brig-dev",
  "server": "https://127.0.0.1:40670",
  "status": "connected",
  "version": "0.1.2"
}`,
			want: "0.1.2",
		},
		{
			name: "disconnected",
			out: `{
  "gateway": "brig-dev",
  "status": "disconnected",
  "error": "transport error"
}`,
			wantErr: "gateway brig-dev is disconnected: transport error",
		},
		{
			name:    "unknown",
			out:     `{"status": "not_configured"}`,
			wantErr: "gateway brig-dev is not registered",
		},
		{name: "garbage", out: "Server Status", wantErr: "parse openshell status"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, log := fakeCLI(t, "cat <<'EOF'\n"+tc.out+"\nEOF")
			v, err := c.GatewayVersion(context.Background(), "brig-dev")
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("GatewayVersion() = %q, %v; want error %q", v, err, tc.wantErr)
				}
				return
			}
			if err != nil || v != tc.want {
				t.Fatalf("GatewayVersion() = %q, %v; want %q", v, err, tc.want)
			}
			if got := readFile(t, log); got != "-g brig-dev status -o json\n" {
				t.Errorf("args = %q", got)
			}
		})
	}

	c, _ := fakeCLI(t, `echo "failed to read TLS CA" >&2; exit 1`)
	if _, err := c.GatewayVersion(context.Background(), "brig-dev"); err == nil || !strings.Contains(err.Error(), "TLS CA") {
		t.Errorf("GatewayVersion() = %v, want the CLI's error", err)
	}
	if _, err := c.GatewayVersion(context.Background(), "../x"); err == nil {
		t.Error("GatewayVersion() accepted an invalid name")
	}
}

// fakeGateways mimics how openshell v0.1.2 adds and removes gateways.
const fakeGateways = `cfg="$XDG_CONFIG_HOME/openshell"
case "$1 $2" in
"gateway add")
	[ ! -e "$cfg/gateways/$7/metadata.json" ] || { echo "Gateway '$7' already exists." >&2; exit 1; }
	mkdir -p "$cfg/gateways/$7"
	echo "$3" >"$cfg/gateways/$7/metadata.json"
	printf %s "$7" >"$cfg/active_gateway"
	;;
"gateway remove")
	[ -e "$cfg/gateways/$3/metadata.json" ] || { echo "No gateway metadata found for '$3'." >&2; exit 1; }
	rm "$cfg/gateways/$3/metadata.json"
	[ "$(cat "$cfg/active_gateway" 2>/dev/null)" != "$3" ] || rm "$cfg/active_gateway"
	;;
esac`

func TestRegister(t *testing.T) {
	const (
		add    = "gateway add https://127.0.0.1:40670 --remote brig-dev --name brig-dev\n"
		remove = "gateway remove brig-dev\n"
	)
	for _, tc := range []struct {
		name       string
		active     string // selected gateway before and after, "" for none
		registered bool
		wantCalls  string
	}{
		{name: "fresh", wantCalls: add},
		{name: "another gateway selected", active: "other", wantCalls: add},
		{name: "re-register", active: "other", registered: true, wantCalls: remove + add},
		{name: "re-register selected", active: "brig-dev", registered: true, wantCalls: remove + add},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, log := fakeCLI(t, fakeGateways)
			active := filepath.Join(c.ConfigHome, "active_gateway")
			metadata := filepath.Join(c.ConfigHome, "gateways", "brig-dev", "metadata.json")
			if tc.active != "" {
				writeFile(t, active, tc.active, 0o644)
			}
			if tc.registered {
				writeFile(t, metadata, "https://127.0.0.1:1234\n", 0o600)
			}
			if err := c.Register(context.Background(), "brig-dev", 40670); err != nil {
				t.Fatal(err)
			}
			if got := readFile(t, log); got != tc.wantCalls {
				t.Errorf("calls:\n%s\nwant:\n%s", got, tc.wantCalls)
			}
			if got := readFile(t, metadata); got != "https://127.0.0.1:40670\n" {
				t.Errorf("registered endpoint = %q", got)
			}
			fi, err := os.Stat(active)
			switch {
			case tc.active == "" && !errors.Is(err, os.ErrNotExist):
				t.Errorf("active gateway is %q, want none", readFile(t, active))
			case tc.active != "" && (err != nil || fi.Mode().Perm() != 0o644 || readFile(t, active) != tc.active):
				t.Errorf("active gateway is %q (%v), want %s with mode 0644", readFile(t, active), err, tc.active)
			}
		})
	}
}

func TestRegisterFailure(t *testing.T) {
	c, _ := fakeCLI(t, `echo "mTLS certificates for gateway 'brig-dev' were not found." >&2; exit 1`)
	active := filepath.Join(c.ConfigHome, "active_gateway")
	writeFile(t, active, "other", 0o600)
	err := c.Register(context.Background(), "brig-dev", 40670)
	if err == nil || !strings.Contains(err.Error(), "were not found") {
		t.Fatalf("Register() = %v, want the CLI's error", err)
	}
	if got := readFile(t, active); got != "other" {
		t.Errorf("active gateway = %q, want other", got)
	}

	c, log := fakeCLI(t, "")
	for _, tc := range []struct {
		name string
		port int
	}{{"brig-dev", 0}, {"brig-dev", 65536}, {"", 40670}, {"a/b", 40670}, {"..", 40670}} {
		if err := c.Register(context.Background(), tc.name, tc.port); err == nil {
			t.Errorf("Register(%q, %d) succeeded", tc.name, tc.port)
		}
	}
	if got := readFile(t, log); got != "" {
		t.Errorf("invalid registrations ran openshell: %q", got)
	}
}

func TestRegisterLock(t *testing.T) {
	c, log := fakeCLI(t, fakeGateways)
	unlock, err := c.lock(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	if err := c.Register(ctx, "brig-dev", 40670); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("Register() while locked = %v, want deadline exceeded", err)
	}
	if err := c.Unregister(ctx, "brig-dev"); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("Unregister() while locked = %v, want deadline exceeded", err)
	}
	if got := readFile(t, log); got != "" {
		t.Errorf("ran openshell while locked: %q", got)
	}
	unlock()
	if err := c.Register(context.Background(), "brig-dev", 40670); err != nil {
		t.Fatal(err)
	}
}

// TestRegisterConcurrently overlaps registrations the way concurrent brig
// processes would: each one starts while the previous one has selected its
// gateway but not yet restored the selection.
func TestRegisterConcurrently(t *testing.T) {
	c, _ := fakeCLI(t, fakeGateways+"\nsleep 0.1")
	active := filepath.Join(c.ConfigHome, "active_gateway")
	writeFile(t, active, "other", 0o600)
	var wg sync.WaitGroup
	for i := range 4 {
		wg.Go(func() {
			time.Sleep(time.Duration(i) * 30 * time.Millisecond)
			if err := c.Register(context.Background(), fmt.Sprintf("brig-%d", i), 40670+i); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	if got := readFile(t, active); got != "other" {
		t.Errorf("active gateway = %q, want other", got)
	}
}

func TestUnregister(t *testing.T) {
	c, log := fakeCLI(t, fakeGateways)
	dir := filepath.Join(c.ConfigHome, "gateways", "brig-dev")
	writeFile(t, filepath.Join(dir, "metadata.json"), "https://127.0.0.1:40670\n", 0o600)
	writeFile(t, filepath.Join(dir, "mtls", "tls.key"), "key", 0o600)
	writeFile(t, filepath.Join(c.ConfigHome, "active_gateway"), "brig-dev", 0o600)
	if err := c.Unregister(context.Background(), "brig-dev"); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, log); got != "gateway remove brig-dev\n" {
		t.Errorf("calls = %q", got)
	}
	if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("%s still exists: %v", dir, err)
	}

	// Unregistered, but a stale bundle is left behind.
	writeFile(t, filepath.Join(dir, "mtls", "tls.key"), "key", 0o600)
	if err := c.Unregister(context.Background(), "brig-dev"); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, log); got != "gateway remove brig-dev\n" {
		t.Errorf("calls = %q, want no further calls", got)
	}
	if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("%s still exists: %v", dir, err)
	}

	c, _ = fakeCLI(t, `echo boom >&2; exit 1`)
	dir = filepath.Join(c.ConfigHome, "gateways", "brig-dev")
	writeFile(t, filepath.Join(dir, "metadata.json"), "https://127.0.0.1:40670\n", 0o600)
	if err := c.Unregister(context.Background(), "brig-dev"); err == nil || !strings.Contains(err.Error(), "boom") {
		t.Errorf("Unregister() = %v, want the CLI's error", err)
	}
	if _, err := os.Stat(dir); err != nil {
		t.Errorf("failed Unregister() removed %s: %v", dir, err)
	}
}

func TestCompatibleVersions(t *testing.T) {
	for _, tc := range []struct {
		host, gateway string
		want          bool
	}{
		{"0.1.2", "0.1.2", true},
		{"0.1.2", "0.1.5", true},
		{"v0.1.2", "0.1.0-dev.6+g2bf9969ab", true},
		{"0.1", "0.1.3", true},
		{"0.1.2", "0.2.0", false},
		{"1.1.0", "0.1.0", false},
		{"0.1.2", "", false},
		{"0", "0", false},
		{"x.y.z", "x.y.z", false},
		{"0.-1.0", "0.-1.0", false},
	} {
		if got := CompatibleVersions(tc.host, tc.gateway); got != tc.want {
			t.Errorf("CompatibleVersions(%q, %q) = %v", tc.host, tc.gateway, got)
		}
	}
}
