// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/dennisklein/brig/internal/llmproxy"
	"github.com/dennisklein/brig/internal/ocsync"
	"github.com/dennisklein/brig/internal/openshell"
	"github.com/dennisklein/brig/internal/service"
	"github.com/dennisklein/brig/internal/vm"
)

func writeFiles(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	for name, data := range files {
		path := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

const claudeProvider = `name: claude
type: claude-sub
credentials:
  ANTHROPIC_AUTH_TOKEN:
    secret_tool:
      lookup: [service, claude.ai, user, alice]
`

// testLLMProxy saves an llm-proxy service and its VM, whose config
// directory holds a provider of the Claude subscription token.
func testLLMProxy(t *testing.T, a *app, config string) (*service.Service, string) {
	t.Helper()
	dir := t.TempDir()
	files := map[string]string{"providers/claude.yaml": claudeProvider}
	if config != "" {
		files[llmproxy.ConfigFile] = config
	}
	writeFiles(t, dir, files)
	svc := &service.Service{Name: "models", Type: llmproxy.TypeName, Placement: service.PlacementVM, Port: 41234, CreatedAt: time.Now().UTC()}
	if err := a.services.Save(svc); err != nil {
		t.Fatal(err)
	}
	v := testVM(t, a, "models")
	v.Role, v.Type, v.OpenShellConfigs = vm.RoleService, llmproxy.TypeName, []string{dir}
	if err := a.vms.Save(v); err != nil {
		t.Fatal(err)
	}
	return svc, dir
}

const subscriptionConfig = `models:
  - model_name: claude-*
    litellm_params: {model: anthropic/claude-*}
keys: {max_budget: 10, budget_duration: 30d}
`

func TestLLMProxySetup(t *testing.T) {
	a := testApp(t)
	svc, dir := testLLMProxy(t, a, subscriptionConfig)
	env := service.Env{Service: *svc}
	setup, err := a.llmProxySetup(env)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(setup.Providers, []string{"claude"}) || !setup.Config.Subscription() || *setup.Config.Keys.MaxBudget != 10 {
		t.Errorf("setup = %+v", setup)
	}

	writeFiles(t, dir, map[string]string{llmproxy.ConfigFile: "models:\n  - model_name: x\n    litellm_params: {model: openai/x, api_key: os.environ/OPENAI_API_KEY}\n"})
	if _, err := a.llmProxySetup(env); err == nil || !strings.Contains(err.Error(), "OPENAI_API_KEY") {
		t.Errorf("a key that no provider has: %v", err)
	}
	if err := os.Remove(filepath.Join(dir, llmproxy.ConfigFile)); err != nil {
		t.Fatal(err)
	}
	if _, err := a.llmProxySetup(env); err == nil || !strings.Contains(err.Error(), "has "+llmproxy.ConfigFile) {
		t.Errorf("no %s: %v", llmproxy.ConfigFile, err)
	}
}

func TestKeepModelKeysOff(t *testing.T) {
	a := testApp(t)
	gwDir := t.TempDir()
	writeFiles(t, gwDir, map[string]string{
		"providers/claude-code.yaml": strings.Replace(claudeProvider, "name: claude", "name: claude-code", 1),
		"providers/github.yaml":      "name: github\ntype: github\ncredentials:\n  GH_TOKEN:\n    secret_tool:\n      lookup: [service, github.com, user, alice]\n",
	})
	gw := testVM(t, a, "dev")
	load := func() *ocsync.Set {
		set, err := ocsync.Load([]string{gwDir})
		if err != nil {
			t.Fatal(err)
		}
		return set
	}
	names := func(set *ocsync.Set) []string {
		var out []string
		for _, p := range set.Workspaces[openshell.DefaultWorkspace].Providers {
			out = append(out, p.Name)
		}
		return out
	}

	// Without an llm-proxy, nothing is kept off.
	set := load()
	if err := a.keepModelKeysOff(gw, set, ocsync.Options{}); err != nil || len(names(set)) != 2 {
		t.Fatalf("no proxy: %q, %v", names(set), err)
	}

	testLLMProxy(t, a, subscriptionConfig)
	set = load()
	var out bytes.Buffer
	var changes []ocsync.Change
	err := a.keepModelKeysOff(gw, set, ocsync.Options{Out: &out, Record: func(c ocsync.Change) { changes = append(changes, c) }})
	if err != nil || !slices.Equal(names(set), []string{"github"}) {
		t.Fatalf("with proxy: %q, %v", names(set), err)
	}
	if !strings.Contains(out.String(), "provider claude-code: kept off the gateway: service models holds its key") || len(changes) != 1 {
		t.Errorf("out = %q, changes = %+v", out.String(), changes)
	}

	// The service VM itself keeps its keys.
	sv, err := a.vms.Load("models")
	if err != nil {
		t.Fatal(err)
	}
	svcSet, err := ocsync.Load(sv.OpenShellConfigs)
	if err != nil {
		t.Fatal(err)
	}
	if err := a.keepModelKeysOff(sv, svcSet, ocsync.Options{}); err != nil || len(names(svcSet)) != 1 {
		t.Errorf("service VM: %q, %v", names(svcSet), err)
	}
}
