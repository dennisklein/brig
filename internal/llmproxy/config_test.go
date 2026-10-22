// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

package llmproxy

import (
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

var creds = []string{"ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN", "OPENROUTER_API_KEY"}

func TestParseConfig(t *testing.T) {
	c, err := ParseConfig([]byte(`
models:
  - model_name: claude-*
    litellm_params:
      model: anthropic/claude-*
      api_key: os.environ/ANTHROPIC_API_KEY
  - model_name: haiku
    litellm_params:
      model: openrouter/anthropic/claude-haiku-4-5
      api_key: os.environ/OPENROUTER_API_KEY
      extra_headers: {X-Title: brig}
  - model_name: qwen
    litellm_params:
      model: openrouter/qwen/qwen3-coder
      api_base: https://openrouter.ai/api/v1
      api_key: os.environ/OPENROUTER_API_KEY
      timeout: 600
      rpm: 30
    model_info: {input_cost_per_token: 0.0000002}
keys:
  models: [claude-*, haiku, qwen]
  max_budget: 20
  budget_duration: 30d
`), creds)
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Models) != 3 || *c.Keys.MaxBudget != 20 || c.Keys.BudgetDuration != "30d" || c.Subscription() {
		t.Fatalf("got %+v", c)
	}
	out, err := c.LiteLLMConfig()
	if err != nil {
		t.Fatal(err)
	}
	var back struct {
		ModelList []map[string]any `yaml:"model_list"`
		General   map[string]any   `yaml:"general_settings"`
	}
	if err := yaml.Unmarshal(out, &back); err != nil {
		t.Fatal(err)
	}
	if len(back.ModelList) != 3 || back.General["master_key"] != "os.environ/LITELLM_MASTER_KEY" {
		t.Fatalf("LiteLLM config:\n%s", out)
	}
	if strings.Contains(string(out), "keys:") || strings.Contains(string(out), "max_budget") {
		t.Fatalf("brig's key limits leaked into LiteLLM's config:\n%s", out)
	}
}

func TestParseConfigSubscription(t *testing.T) {
	data := []byte(`
models:
  - model_name: claude-*
    litellm_params: {model: anthropic/claude-*}
`)
	c, err := ParseConfig(data, []string{"ANTHROPIC_AUTH_TOKEN"})
	if err != nil || !c.Subscription() {
		t.Fatalf("subscription = %v, %v", c != nil && c.Subscription(), err)
	}
	c, err = ParseConfig(data, []string{"ANTHROPIC_API_KEY"})
	if err != nil || c.Subscription() {
		t.Fatalf("API key: subscription = %v, %v", c != nil && c.Subscription(), err)
	}
}

func TestParseConfigRefuses(t *testing.T) {
	for _, tc := range []struct{ name, yaml, want string }{
		{"literal key", `
models:
  - model_name: x
    litellm_params: {model: anthropic/x, api_key: sk-ant-api03-abc}`, "never the key itself"},
		{"unknown credential", `
models:
  - model_name: x
    litellm_params: {model: anthropic/x, api_key: os.environ/AWS_SECRET_ACCESS_KEY}`, "which no provider has"},
		{"secret param", `
models:
  - model_name: x
    litellm_params: {model: bedrock/x, aws_secret_access_key: abc}`, "aws_secret_access_key is not allowed"},
		{"auth header", `
models:
  - model_name: x
    litellm_params: {model: anthropic/x, extra_headers: {Authorization: Bearer abc}}`, "may carry a credential"},
		{"http base", `
models:
  - model_name: x
    litellm_params: {model: openai/x, api_base: "http://llm.example.com/v1"}`, "https URL"},
		{"bare model", `
models:
  - model_name: x
    litellm_params: {model: gpt-4o}`, "PROVIDER/MODEL"},
		{"no models", `keys: {max_budget: 1}`, "at least one model"},
		{"unknown key model", `
models:
  - model_name: x
    litellm_params: {model: openai/x}
keys: {models: [y]}`, "y is not a model_name"},
		{"duration without budget", `
models:
  - model_name: x
    litellm_params: {model: openai/x}
keys: {budget_duration: 30d}`, "needs max_budget"},
		{"bad duration", `
models:
  - model_name: x
    litellm_params: {model: openai/x}
keys: {max_budget: 1, budget_duration: monthly}`, "not a duration"},
		{"negative budget", `
models:
  - model_name: x
    litellm_params: {model: openai/x}
keys: {max_budget: -1}`, "positive"},
		{"unknown field", `
models:
  - model_name: x
    litellm_params: {model: openai/x}
general_settings: {master_key: sk-1234}`, "general_settings"},
		{"ambiguous anthropic credential", `
models:
  - model_name: x
    litellm_params: {model: anthropic/x}`, "needs api_key"},
		{"subscription as api key", `
models:
  - model_name: x
    litellm_params: {model: anthropic/x, api_key: os.environ/ANTHROPIC_AUTH_TOKEN}`, "leave api_key out"},
		{"two documents", "models: []\n---\nmodels: []\n", "more than one"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseConfig([]byte(tc.yaml), creds)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
			if strings.Contains(err.Error(), "sk-ant-api03-abc") {
				t.Fatalf("error repeats the key: %v", err)
			}
		})
	}
}
