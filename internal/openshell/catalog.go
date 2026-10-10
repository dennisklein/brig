// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

package openshell

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
)

// Profile is a provider profile in a gateway's catalog, as `openshell
// profile list -o json` reports it.
type Profile struct {
	ID              string     `json:"id"`
	ResourceVersion uint64     `json:"resource_version"`
	Scope           string     `json:"scope"`
	Source          string     `json:"source"`
	Endpoints       []Endpoint `json:"endpoints"`
}

// Profile scopes. A workspace profile overrides a platform profile with the
// same ID inside its workspace.
const (
	ScopeWorkspace = "workspace"
	ScopePlatform  = "platform"
)

// Endpoint is a destination of a provider profile's credentials.
type Endpoint struct {
	Host  string   `json:"host" yaml:"host"`
	Port  uint32   `json:"port" yaml:"port"`
	Ports []uint32 `json:"ports" yaml:"ports"`
}

// String formats the endpoint as host:port or host:{port,...}.
func (e Endpoint) String() string {
	ports := e.Ports
	if len(ports) == 0 && e.Port != 0 {
		ports = []uint32{e.Port}
	}
	switch len(ports) {
	case 0:
		return e.Host
	case 1:
		return fmt.Sprintf("%s:%d", e.Host, ports[0])
	}
	s := make([]string, len(ports))
	for i, p := range ports {
		s[i] = fmt.Sprint(p)
	}
	return e.Host + ":{" + strings.Join(s, ",") + "}"
}

// Provider is a provider instance in a gateway, without its credential
// values, which the CLI never prints.
type Provider struct {
	Name           string   `json:"name"`
	Type           string   `json:"type"`
	CredentialKeys []string `json:"credential_keys"`
}

// Gateway runs the CLI against one registered gateway, in its default
// workspace.
type Gateway struct {
	cli  *CLI
	name string
}

// Gateway returns a client for the registered gateway name.
func (c *CLI) Gateway(name string) (*Gateway, error) {
	if _, err := gatewayDir(c.ConfigHome, name); err != nil {
		return nil, err
	}
	return &Gateway{cli: c, name: name}, nil
}

func (g *Gateway) run(ctx context.Context, env []string, args ...string) ([]byte, error) {
	return g.cli.runEnv(ctx, env, append([]string{"-g", g.name}, args...)...)
}

// Profiles lists the provider profiles visible in the default workspace,
// platform profiles included.
func (g *Gateway) Profiles(ctx context.Context) ([]Profile, error) {
	out, err := g.run(ctx, nil, "profile", "list", "-o", "json")
	if err != nil {
		return nil, err
	}
	var profiles []Profile
	if err := json.Unmarshal(jsonValue(out), &profiles); err != nil {
		return nil, fmt.Errorf("parse openshell profile list: %w", err)
	}
	return profiles, nil
}

// LintProfile validates a profile file without storing it.
func (g *Gateway) LintProfile(ctx context.Context, file string) error {
	_, err := g.run(ctx, nil, "profile", "lint", "-f", file)
	return err
}

// ImportProfile adds the profile in file to the default workspace. It fails
// if the workspace has a profile with its ID already.
func (g *Gateway) ImportProfile(ctx context.Context, file string) error {
	_, err := g.run(ctx, nil, "profile", "import", "-f", file)
	return err
}

// UpdateProfile replaces the workspace profile id with the one in file,
// which must carry the profile's current resource_version.
func (g *Gateway) UpdateProfile(ctx context.Context, id, file string) error {
	_, err := g.run(ctx, nil, "profile", "update", id, "-f", file)
	return err
}

// DeleteProfile deletes the workspace profile id.
func (g *Gateway) DeleteProfile(ctx context.Context, id string) error {
	_, err := g.run(ctx, nil, "profile", "delete", id)
	return err
}

// Providers lists the providers of the default workspace.
func (g *Gateway) Providers(ctx context.Context) ([]Provider, error) {
	var all []Provider
	token := ""
	// A gateway answers from inside the VM, so its page tokens must not
	// lead in circles.
	seen := map[string]bool{}
	for {
		args := []string{"provider", "list", "-o", "json"}
		if token != "" {
			args = append(args, "--page-token", token)
		}
		out, err := g.run(ctx, nil, args...)
		if err != nil {
			return nil, err
		}
		var page struct {
			Providers     []Provider `json:"providers"`
			NextPageToken string     `json:"next_page_token"`
		}
		if err := json.Unmarshal(jsonValue(out), &page); err != nil {
			return nil, fmt.Errorf("parse openshell provider list: %w", err)
		}
		all = append(all, page.Providers...)
		if page.NextPageToken == "" {
			return all, nil
		}
		if seen[page.NextPageToken] {
			return nil, fmt.Errorf("openshell provider list: page token %q repeats", page.NextPageToken)
		}
		seen[page.NextPageToken] = true
		token = page.NextPageToken
	}
}

// CreateProvider creates a provider of type typ with the credentials creds.
// The values reach the CLI only through its environment, never its command
// line, which other users' processes can read.
func (g *Gateway) CreateProvider(ctx context.Context, name, typ string, creds map[string]string) error {
	args := []string{"provider", "create", "--name", name, "--type", typ}
	env := credentialArgs(&args, creds)
	_, err := g.run(ctx, env, args...)
	return redact(err, creds)
}

// UpdateProvider sets the credentials creds of the provider name, keeping
// its other credentials, and deletes the credentials named in remove.
func (g *Gateway) UpdateProvider(ctx context.Context, name string, creds map[string]string, remove []string) error {
	args := []string{"provider", "update", name}
	env := credentialArgs(&args, creds)
	for _, key := range remove {
		// An empty value deletes the credential.
		args = append(args, "--credential", key+"=")
	}
	_, err := g.run(ctx, env, args...)
	return redact(err, creds)
}

// DeleteProvider deletes the provider name.
func (g *Gateway) DeleteProvider(ctx context.Context, name string) error {
	_, err := g.run(ctx, nil, "provider", "delete", name)
	return err
}

// redact removes the credential values from an error of the CLI, which
// brig prints and records, in case the CLI or the gateway ever echoes one.
func redact(err error, creds map[string]string) error {
	if err == nil {
		return nil
	}
	msg := err.Error()
	// The longest values go first, so that a value which contains another
	// is not left half visible once the shorter one is replaced.
	vals := slices.SortedFunc(maps.Values(creds), func(a, b string) int {
		return cmp.Or(cmp.Compare(len(b), len(a)), cmp.Compare(a, b))
	})
	for _, v := range vals {
		if v != "" {
			msg = strings.ReplaceAll(msg, v, "[redacted]")
		}
	}
	if msg == err.Error() {
		return err
	}
	return errors.New(msg)
}

// credentialArgs appends one `--credential KEY` per credential to args, which
// makes the CLI read the value from the environment variable KEY, and returns
// those variables.
func credentialArgs(args *[]string, creds map[string]string) []string {
	var env []string
	for _, key := range slices.Sorted(maps.Keys(creds)) {
		*args = append(*args, "--credential", key)
		env = append(env, key+"="+creds[key])
	}
	return env
}

// jsonValue skips log lines that the CLI may write to standard output before
// a JSON value: it returns the output from the first line on which valid
// JSON starts, so that a log line like "[WARN] ..." does not count.
func jsonValue(out []byte) []byte {
	for rest := out; len(rest) > 0; {
		trimmed := bytes.TrimLeft(rest, " \t\r")
		if len(trimmed) > 0 && (trimmed[0] == '{' || trimmed[0] == '[') && json.Valid(trimmed) {
			return trimmed
		}
		i := bytes.IndexByte(rest, '\n')
		if i < 0 {
			break
		}
		rest = rest[i+1:]
	}
	return out
}
