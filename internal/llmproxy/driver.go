// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

package llmproxy

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/dennisklein/brig/internal/litellm"
	"github.com/dennisklein/brig/internal/openshell"
	"github.com/dennisklein/brig/internal/osclient"
	"github.com/dennisklein/brig/internal/service"
)

// TypeName is the name of the service type, as types/llm-proxy.yaml in
// package service defines it.
const TypeName = "llm-proxy"

// UIPath is where LiteLLM serves its web UI, at the service's port on the
// host; the master key logs in.
const UIPath = "/ui/"

// What the llm-proxy runs in its service VM's OpenShell gateway: one
// sandbox, in the default workspace, whose LiteLLM listens on the
// sandbox's loopback interface.
const (
	Sandbox   = "litellm"
	Workspace = openshell.DefaultWorkspace
	Port      = 4000
)

// The secrets that the llm-proxy type declares, which the config
// directory's services/NAME.yaml binds to the keyring.
const (
	// MasterKeySecret is LiteLLM's master key, with which brig creates and
	// deletes the keys of sandboxes, and which logs in to LiteLLM's UI.
	MasterKeySecret = "MASTER_KEY"
	// SaltKeySecret encrypts what LiteLLM stores in its database.
	SaltKeySecret = "SALT_KEY"
)

// secretRE is what a master or salt key may hold, so that it is safe in
// the shell file that hands it to LiteLLM.
var secretRE = regexp.MustCompile(`^[A-Za-z0-9_.+/=-]{16,512}$`)

//go:embed policy.yaml
var policyFile []byte

// Gateway is what the driver needs of the service VM's OpenShell gateway.
// *osclient.Client has all but the provider methods, which the openshell
// CLI does.
type Gateway interface {
	Sandbox(ctx context.Context, workspace, name string) (osclient.SandboxRecord, error)
	CreateSandbox(ctx context.Context, r osclient.CreateRequest) error
	StartSandbox(ctx context.Context, workspace, name string) error
	StopSandbox(ctx context.Context, workspace, name string) error
	Exec(ctx context.Context, r osclient.ExecRequest, stdout, stderr io.Writer) (int, error)
	AttachProvider(ctx context.Context, workspace, sandbox, provider string) error
	DetachProvider(ctx context.Context, workspace, sandbox, provider string) error
	ForwardTCP(ctx context.Context, workspace, sandbox string, port uint32) (io.ReadWriteCloser, error)
	Close() error
}

// Setup is what the driver reads from the service's config directories.
type Setup struct {
	Config *Config
	// Providers are the model keys' providers, which the LiteLLM sandbox
	// attaches.
	Providers []string
}

// Driver is the service.Driver of the llm-proxy type. Its keys are
// LiteLLM's virtual keys, which LiteLLM mints and checks, so brig keeps no
// hash of them.
type Driver struct {
	// Open connects to the service VM's gateway.
	Open func(ctx context.Context, env service.Env) (Gateway, error)
	// Setup reads the service's config directories.
	Setup func(env service.Env) (Setup, error)
	// Image makes sure that the service VM's Podman has the image ref,
	// building it on the host and copying it in if need be.
	Image func(ctx context.Context, env service.Env, ref string) error
	// Attachments returns the service's attachments, whose keys Started
	// keeps; it deletes the other keys that brig made.
	Attachments func(env service.Env) ([]service.Attachment, error)
	// Out gets notes for the user, such as a sandbox that runs an older
	// image.
	Out io.Writer
	// Timeout bounds the wait for the sandbox and for LiteLLM; zero means
	// five minutes.
	Timeout time.Duration
	// Poll is the pause between checks while waiting; zero means a second.
	Poll time.Duration
}

var _ service.Driver = (*Driver)(nil)

// admin returns a client of LiteLLM's admin API, which reaches the
// sandbox's loopback interface through gw. brig thus needs neither the
// forward of the service's host port nor a way into the sandbox from the
// VM's network.
func (d *Driver) admin(ctx context.Context, env service.Env, gw Gateway) (*litellm.Client, error) {
	master, err := secret(ctx, env, MasterKeySecret)
	if err != nil {
		return nil, err
	}
	tr := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			rwc, err := gw.ForwardTCP(ctx, Workspace, Sandbox, Port)
			if err != nil {
				return nil, fmt.Errorf("connect to sandbox %s: %w", Sandbox, err)
			}
			return pipeConn(rwc), nil
		},
		MaxIdleConns:    1,
		IdleConnTimeout: 10 * time.Second,
	}
	return &litellm.Client{BaseURL: "http://" + Sandbox, MasterKey: master, HTTP: &http.Client{Transport: tr, Timeout: time.Minute}}, nil
}

// pipeConn makes a net.Conn of a forwarded connection, for net/http.
func pipeConn(rwc io.ReadWriteCloser) net.Conn {
	local, remote := net.Pipe()
	go func() {
		_, _ = io.Copy(rwc, remote)
		_ = rwc.Close()
	}()
	go func() {
		_, _ = io.Copy(remote, rwc)
		_ = remote.Close()
	}()
	return local
}

// withAdmin opens the service VM's gateway and runs f with a client of
// LiteLLM's admin API.
func (d *Driver) withAdmin(ctx context.Context, env service.Env, f func(*litellm.Client) error) error {
	gw, err := d.Open(ctx, env)
	if err != nil {
		return err
	}
	defer func() { _ = gw.Close() }()
	c, err := d.admin(ctx, env, gw)
	if err != nil {
		return err
	}
	defer c.HTTP.CloseIdleConnections()
	return f(c)
}

func secret(ctx context.Context, env service.Env, name string) (string, error) {
	s, err := env.Secret(ctx, name)
	if err != nil {
		return "", fmt.Errorf("secret %s of service %s: %w", name, env.Service.Name, err)
	}
	if !secretRE.MatchString(s) {
		return "", fmt.Errorf("secret %s of service %s: want 16 to 512 letters, digits or _.+/=-, such as the output of openssl rand -hex 32", name, env.Service.Name)
	}
	return s, nil
}

// Issue creates the sandbox's LiteLLM key, with the limits of the service's
// llm-proxy.yaml. A key that the sandbox had already is replaced, since brig
// keeps no copy of it.
func (d *Driver) Issue(ctx context.Context, env service.Env, a *service.Attachment) (string, error) {
	setup, err := d.Setup(env)
	if err != nil {
		return "", err
	}
	o := litellm.Owner{Gateway: a.Gateway, Workspace: a.Workspace, Sandbox: a.Sandbox}
	alias, err := o.Alias()
	if err != nil {
		return "", err
	}
	k := setup.Config.Keys
	var key string
	err = d.withAdmin(ctx, env, func(c *litellm.Client) error {
		key, err = c.ReplaceKey(ctx, litellm.KeyRequest{
			Alias: alias, Models: k.Models, MaxBudget: k.MaxBudget, BudgetDuration: k.BudgetDuration, Metadata: o.Metadata(),
		})
		return err
	})
	return key, err
}

// Revoke deletes the sandbox's LiteLLM key; a key that is gone already is
// no error.
func (d *Driver) Revoke(ctx context.Context, env service.Env, a service.Attachment) error {
	alias, err := litellm.Owner{Gateway: a.Gateway, Workspace: a.Workspace, Sandbox: a.Sandbox}.Alias()
	if err != nil {
		return err
	}
	return d.withAdmin(ctx, env, func(c *litellm.Client) error {
		if err := c.DeleteKeys(ctx, alias); err != nil && !errors.Is(err, litellm.ErrNotFound) {
			return err
		}
		return nil
	})
}

// prune deletes the LiteLLM keys that brig made for sandboxes that are no
// longer attached, such as those of an attach that failed half way or of a
// gateway deleted while the service was down, and returns their aliases.
// Keys made in LiteLLM's UI have no brig/ alias and stay.
func prune(ctx context.Context, c *litellm.Client, atts []service.Attachment) ([]string, error) {
	have, err := c.Aliases(ctx)
	if err != nil {
		return nil, err
	}
	var stale []string
	for _, alias := range have {
		o, _ := litellm.ParseAlias(alias)
		if !slices.ContainsFunc(atts, func(a service.Attachment) bool { return a.Is(o.Gateway, o.Workspace, o.Sandbox) }) {
			stale = append(stale, alias)
		}
	}
	if err := c.DeleteKeys(ctx, stale...); err != nil && !errors.Is(err, litellm.ErrNotFound) {
		return nil, err
	}
	return stale, nil
}

// Started makes sure the LiteLLM sandbox runs with the model keys'
// providers attached, hands LiteLLM its keys and config, which it loads
// again if they changed, and deletes the keys of sandboxes that are no
// longer attached. It runs after each start of the service VM and after
// each sync of its config directories.
func (d *Driver) Started(ctx context.Context, env service.Env) error {
	setup, err := d.Setup(env)
	if err != nil {
		return err
	}
	config, err := setup.Config.LiteLLMConfig()
	if err != nil {
		return err
	}
	master, err := secret(ctx, env, MasterKeySecret)
	if err != nil {
		return err
	}
	salt, err := secret(ctx, env, SaltKeySecret)
	if err != nil {
		return err
	}
	ref := ImageRef()
	if err := d.Image(ctx, env, ref); err != nil {
		return err
	}
	gw, err := d.Open(ctx, env)
	if err != nil {
		return err
	}
	defer func() { _ = gw.Close() }()
	if err := d.ensureSandbox(ctx, env, gw, ref, setup.Providers); err != nil {
		return err
	}
	var stdin bytes.Buffer
	fmt.Fprintf(&stdin, "%s=%s\n%s=%s\n---\n", MasterKeyEnv, master, SaltKeyEnv, salt)
	stdin.Write(config)
	var out, errOut bytes.Buffer
	code, err := gw.Exec(ctx, osclient.ExecRequest{
		Workspace: Workspace, Sandbox: Sandbox, Command: []string{"llm-proxy-load"}, Stdin: stdin.Bytes(), Timeout: time.Minute,
	}, &out, &errOut)
	if err == nil && code != 0 {
		err = fmt.Errorf("exit status %d: %s", code, strings.TrimSpace(errOut.String()))
	}
	if err != nil {
		return fmt.Errorf("hand LiteLLM its config in sandbox %s: %w", Sandbox, err)
	}
	if strings.Contains(out.String(), "reloaded") {
		fmt.Fprintf(d.out(), "LiteLLM of service %s loads its new config.\n", env.Service.Name)
	}
	if err := d.waitLiteLLM(ctx, gw); err != nil {
		return err
	}
	atts, err := d.Attachments(env)
	if err != nil {
		return err
	}
	c, err := d.admin(ctx, env, gw)
	if err != nil {
		return err
	}
	defer c.HTTP.CloseIdleConnections()
	stale, err := prune(ctx, c, atts)
	if err != nil {
		return fmt.Errorf("delete the keys of detached sandboxes: %w", err)
	}
	for _, alias := range stale {
		fmt.Fprintf(d.out(), "Deleted LiteLLM key %s of service %s: its sandbox is not attached.\n", alias, env.Service.Name)
	}
	return nil
}

func (d *Driver) out() io.Writer {
	if d.Out == nil {
		return io.Discard
	}
	return d.Out
}

func (d *Driver) timeout() time.Duration {
	if d.Timeout == 0 {
		return 5 * time.Minute
	}
	return d.Timeout
}

func (d *Driver) poll() time.Duration {
	if d.Poll == 0 {
		return time.Second
	}
	return d.Poll
}

// ensureSandbox creates the LiteLLM sandbox, or makes the one there run
// with providers attached.
func (d *Driver) ensureSandbox(ctx context.Context, env service.Env, gw Gateway, ref string, providers []string) error {
	rec, err := gw.Sandbox(ctx, Workspace, Sandbox)
	if errors.Is(err, osclient.ErrNoSandbox) {
		spec, err := sandboxSpec(ref, providers)
		if err != nil {
			return err
		}
		policy, err := osclient.PolicyJSON(policyFile)
		if err != nil {
			return err
		}
		fmt.Fprintf(d.out(), "Creating sandbox %s of service %s from %s.\n", Sandbox, env.Service.Name, ref)
		if err := gw.CreateSandbox(ctx, osclient.CreateRequest{
			Workspace: Workspace, Name: Sandbox, Spec: spec, Policy: policy,
			Labels: map[string]string{"brig.service": env.Service.Name},
		}); err != nil {
			return fmt.Errorf("create sandbox %s: %w", Sandbox, err)
		}
		return d.waitPhase(ctx, gw, "Ready")
	}
	if err != nil {
		return err
	}
	if rec.Image != ref {
		fmt.Fprintf(d.out(), "Sandbox %s of service %s runs %s; this brig would create it from %s. Its database is in the sandbox, so brig leaves it as it is.\n",
			Sandbox, env.Service.Name, rec.Image, ref)
	}
	have := rec.Providers()
	changed := false
	for _, p := range providers {
		if !slices.Contains(have, p) {
			if err := gw.AttachProvider(ctx, Workspace, Sandbox, p); err != nil {
				return fmt.Errorf("attach provider %s to sandbox %s: %w", p, Sandbox, err)
			}
			changed = true
		}
	}
	for _, p := range have {
		if !slices.Contains(providers, p) {
			if err := gw.DetachProvider(ctx, Workspace, Sandbox, p); err != nil {
				return fmt.Errorf("detach provider %s from sandbox %s: %w", p, Sandbox, err)
			}
			changed = true
		}
	}
	// A process gets its providers' variables when it starts.
	if changed && rec.Phase != "Stopped" {
		fmt.Fprintf(d.out(), "Restarting sandbox %s of service %s for its new providers.\n", Sandbox, env.Service.Name)
		if err := gw.StopSandbox(ctx, Workspace, Sandbox); err != nil {
			return fmt.Errorf("stop sandbox %s: %w", Sandbox, err)
		}
		if err := d.waitPhase(ctx, gw, "Stopped"); err != nil {
			return err
		}
		rec.Phase = "Stopped"
	}
	if rec.Phase == "Stopped" {
		if err := gw.StartSandbox(ctx, Workspace, Sandbox); err != nil {
			return fmt.Errorf("start sandbox %s: %w", Sandbox, err)
		}
	}
	return d.waitPhase(ctx, gw, "Ready")
}

// sandboxSpec is the spec of a new LiteLLM sandbox, as protobuf JSON.
func sandboxSpec(ref string, providers []string) (json.RawMessage, error) {
	if providers == nil {
		providers = []string{}
	}
	return json.Marshal(map[string]any{
		"template":      map[string]any{"image": ref},
		"providers":     providers,
		"environment":   map[string]string{"LLM_PROXY_PORT": strconv.Itoa(Port)},
		"restartPolicy": "SANDBOX_RESTART_POLICY_ALWAYS",
	})
}

func (d *Driver) waitPhase(ctx context.Context, gw Gateway, phase string) error {
	ctx, cancel := context.WithTimeout(ctx, d.timeout())
	defer cancel()
	for {
		rec, err := gw.Sandbox(ctx, Workspace, Sandbox)
		switch {
		case err == nil && rec.Phase == phase:
			return nil
		case err == nil && rec.Phase == "Error":
			return fmt.Errorf("sandbox %s is in error: %s", Sandbox, rec.Problem)
		case err == nil:
			err = fmt.Errorf("sandbox %s is %s", Sandbox, strings.ToLower(rec.Phase))
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("waiting for sandbox %s to be %s: %w", Sandbox, strings.ToLower(phase), err)
		case <-time.After(d.poll()):
		}
	}
}

// healthCheck asks LiteLLM in the sandbox whether it serves.
var healthCheck = []string{
	"python3", "-c",
	`import sys, urllib.request; urllib.request.urlopen("http://127.0.0.1:" + sys.argv[1] + "/health/liveliness", timeout=5)`,
	strconv.Itoa(Port),
}

func (d *Driver) waitLiteLLM(ctx context.Context, gw Gateway) error {
	ctx, cancel := context.WithTimeout(ctx, d.timeout())
	defer cancel()
	for {
		var errOut bytes.Buffer
		code, err := gw.Exec(ctx, osclient.ExecRequest{Workspace: Workspace, Sandbox: Sandbox, Command: healthCheck, Timeout: 20 * time.Second}, io.Discard, &errOut)
		if err == nil && code == 0 {
			return nil
		}
		if err == nil {
			err = fmt.Errorf("exit status %d", code)
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("waiting for LiteLLM in sandbox %s: %w", Sandbox, err)
		case <-time.After(d.poll()):
		}
	}
}
