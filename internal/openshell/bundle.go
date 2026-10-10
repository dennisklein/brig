// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

package openshell

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/dennisklein/brig/internal/guest"
	"github.com/dennisklein/brig/internal/paths"
	"github.com/dennisklein/brig/internal/sshx"
	"github.com/dennisklein/brig/internal/vm"
)

// ErrNoBundle is returned by SyncBundle while the gateway has not generated
// its PKI yet, which it does when it first starts.
var ErrNoBundle = errors.New("the gateway has not created its mTLS bundle yet")

// remoteTLSDir is where the gateway's systemd user unit keeps its PKI
// (OPENSHELL_LOCAL_TLS_DIR=%h/.local/state/openshell/tls).
const remoteTLSDir = "/home/" + guest.User + "/.local/state/openshell/tls"

// bundle lists the files the CLI needs to reach a gateway over mTLS: their
// paths in remoteTLSDir, their names in MTLSDir and their PEM block types.
// The CA's private key stays in the VM.
var bundle = []struct{ remote, local, pemType string }{
	{"ca.crt", "ca.crt", "CERTIFICATE"},
	{"client/tls.crt", "tls.crt", "CERTIFICATE"},
	{"client/tls.key", "tls.key", "PRIVATE KEY"},
}

// noBundleStatus is the exit status of fetchScript when a file is missing.
const noBundleStatus = 3

// fetchScript prints each bundle file base64-encoded on a line of its own.
func fetchScript() string {
	tests := make([]string, len(bundle))
	prints := make([]string, len(bundle))
	for i, f := range bundle {
		tests[i] = "test -s " + f.remote
		prints[i] = "base64 -w0 " + f.remote + " && echo"
	}
	return "cd " + remoteTLSDir + " 2>/dev/null && " + strings.Join(tests, " && ") +
		" || exit " + strconv.Itoa(noBundleStatus) + "; " + strings.Join(prints, " && ")
}

// MTLSDir is where the CLI looks for the client bundle of gateway.
func MTLSDir(configHome, gateway string) string {
	return filepath.Join(configHome, "gateways", gateway, "mtls")
}

// SyncBundle copies ca.crt, client/tls.crt and client/tls.key from the VM's
// ~/.local/state/openshell/tls into MTLSDir as ca.crt, tls.crt and tls.key
// (directory 0700, files 0600, atomic writes), never copying the CA key. It
// logs in as guest.User, whatever t.User says. It reports whether anything
// changed, e.g. because the gateway regenerated its PKI.
//
// The gateway service (re)generates its PKI before it starts, including a
// new CA when its server certificate lacks a name that its OpenShell version
// requires, e.g. after an upgrade. It replaces the files one by one, so a
// sync that runs while the service is still starting can copy an outdated or
// mixed bundle. Callers sync again while the gateway rejects the bundle.
func SyncBundle(ctx context.Context, t sshx.Target, configHome, gateway string) (changed bool, err error) {
	dir, err := gatewayDir(configHome, gateway)
	if err != nil {
		return false, err
	}
	t.User = guest.User
	out, err := t.Output(ctx, fetchScript())
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && exitErr.ExitCode() == noBundleStatus {
		return false, fmt.Errorf("gateway %s: %w", gateway, ErrNoBundle)
	}
	if err != nil {
		return false, fmt.Errorf("fetch the mTLS bundle of gateway %s: %w", gateway, err)
	}
	files, err := decodeBundle(out)
	if err != nil {
		return false, fmt.Errorf("fetch the mTLS bundle of gateway %s: %w", gateway, err)
	}

	dir = filepath.Join(dir, "mtls")
	if err := paths.EnsurePrivate(dir); err != nil {
		return false, err
	}
	for i, f := range bundle {
		path := filepath.Join(dir, f.local)
		if unchanged(path, files[i]) {
			continue
		}
		if err := vm.WriteFileAtomic(path, files[i], 0o600); err != nil {
			return changed, err
		}
		changed = true
	}
	return changed, nil
}

// decodeBundle decodes fetchScript's output into the bundle's files. It
// ignores lines before the last len(bundle) ones: sshd runs the script with
// the agent's login shell, which may print to stdout from ~/.bashrc first.
func decodeBundle(out []byte) ([][]byte, error) {
	lines := strings.Split(strings.TrimSuffix(string(out), "\n"), "\n")
	if len(lines) < len(bundle) {
		return nil, fmt.Errorf("got %d files, want %d", len(lines), len(bundle))
	}
	lines = lines[len(lines)-len(bundle):]
	files := make([][]byte, len(bundle))
	for i, f := range bundle {
		data, err := base64.StdEncoding.DecodeString(lines[i])
		if err != nil {
			return nil, fmt.Errorf("%s: %w", f.remote, err)
		}
		if block, _ := pem.Decode(data); block == nil || !strings.HasSuffix(block.Type, f.pemType) {
			return nil, fmt.Errorf("%s: no %s PEM block", f.remote, f.pemType)
		}
		files[i] = data
	}
	return files, nil
}

// unchanged reports whether path holds data with mode 0600.
func unchanged(path string, data []byte) bool {
	fi, err := os.Stat(path)
	if err != nil || fi.Mode().Perm() != 0o600 {
		return false
	}
	old, err := os.ReadFile(path)
	return err == nil && bytes.Equal(old, data)
}

// RemoveBundle deletes the gateway's client bundle from MTLSDir.
func RemoveBundle(configHome, gateway string) error {
	dir, err := gatewayDir(configHome, gateway)
	if err != nil {
		return err
	}
	return os.RemoveAll(filepath.Join(dir, "mtls"))
}
