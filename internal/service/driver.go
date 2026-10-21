// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

package service

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"

	"github.com/dennisklein/brig/internal/sshx"
)

// Env is what a Driver gets to work with a service.
type Env struct {
	Service Service
	Type    Type
	// Secret looks up a secret that the type declares, as the config
	// directories bind it (see ocsync.ServiceSecrets).
	Secret func(ctx context.Context, name string) (string, error)
	// Root logs in to the service VM as root; it is the zero Target for a
	// service on the host.
	Root sshx.Target
}

// Driver is what a service type does beyond what brig does for every type.
// The default, HashDriver, suits a service that checks the tokens of its
// callers against the hashes in tokens.json.
type Driver interface {
	// Issue mints the token of a new attachment, registers it with the
	// service and returns it; it may fill in a.SHA256. brig records a in
	// tokens.json and stores the token on the attachment's gateway.
	Issue(ctx context.Context, env Env, a *Attachment) (token string, err error)
	// Revoke makes the service refuse the attachment's token from now on.
	// brig removes a from tokens.json too, whatever Revoke returns.
	Revoke(ctx context.Context, env Env, a Attachment) error
	// Started runs after each start of the service, once brig has put the
	// service's secrets and tokens in place and, for a type with OpenShell,
	// the service's gateway answers and its config directories synced.
	Started(ctx context.Context, env Env) error
}

// HashDriver mints random tokens and records their hashes in tokens.json,
// which brig hands to the service; revoking a token removes its hash.
type HashDriver struct{}

// TokenPrefix starts the tokens that HashDriver mints, so that secret
// scanners and people can tell them.
const TokenPrefix = "brigsvc_"

// Issue mints a token of 256 random bits.
func (HashDriver) Issue(_ context.Context, _ Env, a *Attachment) (string, error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	token := TokenPrefix + base64.RawURLEncoding.EncodeToString(b[:])
	a.SHA256 = TokenHash(token)
	return token, nil
}

// Revoke does nothing: the token's hash goes with the attachment.
func (HashDriver) Revoke(context.Context, Env, Attachment) error { return nil }

// Started does nothing.
func (HashDriver) Started(context.Context, Env) error { return nil }

// TokenHash returns the hex-encoded SHA-256 of token, as Attachment.SHA256
// holds it.
func TokenHash(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// HostRunner runs what a service needs on the host, as a user unit: the
// controller of a service of placement host, such as the vm runner's, or,
// for a service of placement vm, what runs next to its VM, such as the
// forwarder to the service's sandbox (see Type.Sandbox).
type HostRunner interface {
	// Start starts it unless it runs. brig calls it when it creates a
	// service on the host, at brig service start, and at every start of a
	// gateway that has a sandbox attached to the service; for a service of
	// placement vm, after every start of its VM, once Driver.Started
	// returned.
	Start(ctx context.Context, svc Service) error
	// Stop stops it, keeping what it made. brig calls it at brig service
	// stop and, for a service of placement vm, whenever its VM stops.
	Stop(ctx context.Context, svc Service) error
	// Delete removes what it made and stops it. brig calls it once the
	// service has no attachments and, for a service of placement vm, its
	// VM is gone, before it removes the service's directory.
	Delete(ctx context.Context, svc Service) error
}
