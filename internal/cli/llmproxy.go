// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"

	"github.com/dennisklein/brig/internal/llmproxy"
	"github.com/dennisklein/brig/internal/ocsync"
	"github.com/dennisklein/brig/internal/openshell"
	"github.com/dennisklein/brig/internal/osclient"
	"github.com/dennisklein/brig/internal/service"
	"github.com/dennisklein/brig/internal/svcforward"
	"github.com/dennisklein/brig/internal/vm"
)

func init() {
	appDrivers[llmproxy.TypeName] = func(a *app) service.Driver {
		return &llmproxy.Driver{
			Open:  a.llmProxyGateway,
			Setup: a.llmProxySetup,
			Image: a.llmProxyImage,
			Attachments: func(env service.Env) ([]service.Attachment, error) {
				return a.services.Attachments(env.Service.Name)
			},
			Out: os.Stderr,
		}
	}
	hostRunners[llmproxy.TypeName] = forwardRunner{}
}

// forwardRunner serves the host port of a service whose workload listens
// in a sandbox of its VM (see service.Type.Sandbox), with brig service
// forward in a user unit.
type forwardRunner struct{}

func (forwardRunner) Start(ctx context.Context, svc service.Service) error {
	return svcforward.StartUnit(ctx, svc.Name)
}

func (forwardRunner) Stop(ctx context.Context, svc service.Service) error {
	svcforward.StopUnit(ctx, svc.Name)
	return nil
}

func (forwardRunner) Delete(ctx context.Context, svc service.Service) error {
	svcforward.StopUnit(ctx, svc.Name)
	return nil
}

// llmProxyGateway is the gateway of an llm-proxy's VM: its API, and the
// openshell CLI for the providers of the LiteLLM sandbox.
type llmProxyGateway struct {
	*osclient.Client
	cli *openshell.Gateway
}

func (g llmProxyGateway) AttachProvider(ctx context.Context, ws, sandbox, provider string) error {
	in, err := g.cli.In(ws)
	if err != nil {
		return err
	}
	return in.AttachProvider(ctx, sandbox, provider)
}

func (g llmProxyGateway) DetachProvider(ctx context.Context, ws, sandbox, provider string) error {
	in, err := g.cli.In(ws)
	if err != nil {
		return err
	}
	return in.DetachProvider(ctx, sandbox, provider)
}

func (a *app) llmProxyGateway(_ context.Context, env service.Env) (llmproxy.Gateway, error) {
	v, err := a.vms.Load(env.Service.Name)
	if err != nil {
		return nil, err
	}
	cli, err := openshell.Find()
	if err != nil {
		return nil, err
	}
	gw, err := cli.Gateway(v.GatewayName())
	if err != nil {
		return nil, err
	}
	c, err := gatewayClient(v)
	if err != nil {
		return nil, fmt.Errorf("gateway of service %s: %w", env.Service.Name, err)
	}
	return llmProxyGateway{Client: c, cli: gw}, nil
}

// llmProxySetup reads the config directories of the llm-proxy's VM: the
// providers of the default workspace hold the model keys, and
// llm-proxy.yaml in one of the directories says which models LiteLLM
// offers with which of their credentials.
func (a *app) llmProxySetup(env service.Env) (llmproxy.Setup, error) {
	v, err := a.vms.Load(env.Service.Name)
	if err != nil {
		return llmproxy.Setup{}, err
	}
	set, err := ocsync.Load(v.OpenShellConfigs)
	if err != nil {
		return llmproxy.Setup{}, err
	}
	var providers, creds []string
	for _, p := range set.Workspaces[openshell.DefaultWorkspace].Providers {
		providers = append(providers, p.Name)
		for key := range p.Credentials {
			creds = append(creds, key)
		}
	}
	slices.Sort(creds)
	var found []string
	for _, dir := range set.Dirs {
		path := filepath.Join(dir, llmproxy.ConfigFile)
		if _, err := os.Lstat(path); err == nil {
			found = append(found, path)
		} else if !errors.Is(err, fs.ErrNotExist) {
			return llmproxy.Setup{}, err
		}
	}
	switch len(found) {
	case 0:
		return llmproxy.Setup{}, fmt.Errorf("no config directory of service %s has %s, which lists the models that LiteLLM offers; brig service create --help shows one",
			env.Service.Name, llmproxy.ConfigFile)
	case 1:
	default:
		return llmproxy.Setup{}, fmt.Errorf("%s is in more than one config directory of service %s: %s", llmproxy.ConfigFile, env.Service.Name, strings.Join(found, ", "))
	}
	data, err := ocsync.ReadConfigFile(found[0])
	if err != nil {
		return llmproxy.Setup{}, err
	}
	cfg, err := llmproxy.ParseConfig(data, slices.Compact(creds))
	if err != nil {
		return llmproxy.Setup{}, fmt.Errorf("%s: %w", found[0], err)
	}
	return llmproxy.Setup{Config: cfg, Providers: providers}, nil
}

// llmProxyImage makes sure that the llm-proxy's VM has the LiteLLM image
// ref: it builds it with Podman on the host from the context that brig
// embeds, unless the host has it, and copies it into the VM.
func (a *app) llmProxyImage(ctx context.Context, env service.Env, ref string) error {
	v, err := a.vms.Load(env.Service.Name)
	if err != nil {
		return err
	}
	if a.sshTarget(v, false).Command(ctx, "podman", "image", "exists", ref).Run() == nil {
		return nil
	}
	if !podmanImageExists(ctx, ref) {
		dir, err := os.MkdirTemp("", "brig-llm-proxy-")
		if err != nil {
			return err
		}
		defer func() { _ = os.RemoveAll(dir) }()
		if err := llmproxy.WriteImageContext(dir); err != nil {
			return err
		}
		fmt.Fprintf(os.Stderr, "Building %s with Podman; the first build takes some minutes.\n", ref)
		out := newTailWriter(20)
		build := exec.CommandContext(ctx, "podman", "build", "-t", ref, dir)
		build.Stdout, build.Stderr = out, out
		if err := build.Run(); err != nil {
			return fmt.Errorf("podman build %s: %w\n%s", ref, err, strings.TrimSpace(out.String()))
		}
	}
	return a.pushImage(ctx, v, ref, os.Stderr, io.Discard)
}

// keepModelKeysOff takes out of set, the config directories of the gateway
// VM v, each provider with a secret that an llm-proxy service holds too, so
// that the model keys that the proxy holds reach no gateway. With --prune,
// brig sync then deletes such providers that it created before. A service
// whose config directories cannot be read stops the sync, since brig could
// not tell its keys.
func (a *app) keepModelKeysOff(v *vm.VM, set *ocsync.Set, opts ocsync.Options) error {
	if v.IsService() {
		return nil
	}
	svcs, err := a.services.List()
	if err != nil {
		return err
	}
	held := map[string]string{}
	for _, svc := range svcs {
		if svc.Type != llmproxy.TypeName {
			continue
		}
		sv, err := a.vms.Load(svc.Name)
		if err != nil {
			return err
		}
		ss, err := ocsync.Load(sv.OpenShellConfigs)
		if err != nil {
			return fmt.Errorf("service %s, whose model keys stay off gateways: %w", svc.Name, err)
		}
		for _, p := range ss.Workspaces[openshell.DefaultWorkspace].Providers {
			for _, c := range p.Credentials {
				held[credentialRef(c)] = svc.Name
			}
		}
	}
	if len(held) == 0 {
		return nil
	}
	for name, ws := range set.Workspaces {
		ws.Providers = slices.DeleteFunc(ws.Providers, func(p ocsync.Provider) bool {
			for _, c := range p.Credentials {
				if svc, ok := held[credentialRef(c)]; ok {
					msg := fmt.Sprintf("kept off the gateway: service %s holds its key", svc)
					if opts.Out != nil {
						fmt.Fprintf(opts.Out, "provider %s: %s\n", p.Name, msg)
					}
					if opts.Record != nil {
						opts.Record(ocsync.Change{Workspace: name, Kind: "provider", Name: p.Name, Result: msg})
					}
					return true
				}
			}
			return false
		})
		set.Workspaces[name] = ws
	}
	return nil
}

// credentialRef names where a credential's value comes from.
func credentialRef(c ocsync.Credential) string {
	if c.SecretTool != nil {
		return "secret_tool\x00" + strings.Join(c.SecretTool.Lookup, "\x00")
	}
	return "env\x00" + c.Env
}
