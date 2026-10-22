// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/dennisklein/brig/internal/llmproxy"
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
