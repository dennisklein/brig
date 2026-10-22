// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

package llmproxy

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"maps"
	"math"
	"net/url"
	"regexp"
	"slices"
	"strings"

	"go.yaml.in/yaml/v3"
)

// ConfigFile is the name of the llm-proxy's file in the service's config
// directory, next to the profiles and providers of its model keys.
const ConfigFile = "llm-proxy.yaml"

// Config is the content of llm-proxy.yaml:
//
//	models:                     # LiteLLM's model_list
//	  - model_name: claude-*
//	    litellm_params:
//	      model: anthropic/claude-*
//	      api_key: os.environ/ANTHROPIC_API_KEY
//	keys:                       # what each sandbox's key may do
//	  models: [claude-*]
//	  max_budget: 20
//	  budget_duration: 30d
//
// The models are LiteLLM's, with fewer settings: an api_key can only name a
// credential of a provider in the same config directory, whose value in the
// LiteLLM sandbox is OpenShell's placeholder. So the file holds no secret,
// and LiteLLM never sees a model key.
type Config struct {
	Models []Model `yaml:"models"`
	Keys   Keys    `yaml:"keys"`

	subscription bool
}

// Subscription reports whether LiteLLM calls Anthropic with a Claude
// subscription token: an anthropic model has no api_key, and the only
// Anthropic credential is ANTHROPIC_AUTH_TOKEN. Anthropic then wants the
// OAuth beta, which Claude Code must name in its requests, since LiteLLM
// passes the client's betas on in place of its own.
func (c *Config) Subscription() bool { return c.subscription }

// Model is one entry of LiteLLM's model_list.
type Model struct {
	Name   string         `yaml:"model_name"`
	Params map[string]any `yaml:"litellm_params"`
	// Info is LiteLLM's model_info, such as costs; brig passes it on.
	Info map[string]any `yaml:"model_info,omitempty"`
}

// Keys are the limits of each sandbox's virtual key.
type Keys struct {
	// Models are the model names a key may use; empty means all.
	Models []string `yaml:"models"`
	// MaxBudget is the most a key may spend per BudgetDuration, in US
	// dollars; nil means no limit.
	MaxBudget *float64 `yaml:"max_budget"`
	// BudgetDuration resets the spend, such as 30d or 24h.
	BudgetDuration string `yaml:"budget_duration"`
}

// allowedParams are the litellm_params that llm-proxy.yaml may set. Others,
// such as aws_secret_access_key or vertex_credentials, would hold secrets,
// or change what LiteLLM runs.
var allowedParams = []string{
	"api_base", "api_key", "api_version", "extra_headers", "max_retries", "model",
	"rpm", "stream_timeout", "timeout", "tpm",
}

var (
	envRefRE   = regexp.MustCompile(`^os\.environ/([A-Za-z_][A-Za-z0-9_]*)$`)
	modelRE    = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/@*+-]*$`)
	durationRE = regexp.MustCompile(`^[1-9][0-9]*[smhd]$|^[1-9][0-9]*mo$`)
	headerRE   = regexp.MustCompile(`^[A-Za-z0-9-]+$`)
	// secretHeaderRE matches header names that carry credentials, which
	// belong in a provider.
	secretHeaderRE = regexp.MustCompile(`(?i)auth|key|token|secret|cookie|session|password`)
)

// ParseConfig reads llm-proxy.yaml. credentials are the credential names of
// the providers in the service's config directories: an api_key must name
// one of them.
func ParseConfig(data []byte, credentials []string) (*Config, error) {
	var c Config
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&c); err != nil {
		return nil, err
	}
	var extra yaml.Node
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		return nil, errors.New("more than one YAML document")
	}
	return &c, c.validate(credentials)
}

func (c *Config) validate(credentials []string) error {
	if len(c.Models) == 0 {
		return errors.New("models: name at least one model")
	}
	names := map[string]bool{}
	for i, m := range c.Models {
		if err := m.validate(credentials); err != nil {
			return fmt.Errorf("models[%d] %s: %w", i, m.Name, err)
		}
		// Without api_key, LiteLLM sends ANTHROPIC_API_KEY if it is set,
		// and ANTHROPIC_AUTH_TOKEN, a subscription token, only if not: a
		// model meant for the subscription would silently use the API key.
		model, _ := m.Params["model"].(string)
		key, _ := m.Params["api_key"].(string)
		if key == "os.environ/ANTHROPIC_AUTH_TOKEN" && strings.HasPrefix(model, "anthropic/") {
			return fmt.Errorf("models[%d] %s: ANTHROPIC_AUTH_TOKEN is a subscription token, which LiteLLM sends as Bearer only on its own: leave api_key out", i, m.Name)
		}
		if _, ok := m.Params["api_key"]; !ok && strings.HasPrefix(model, "anthropic/") &&
			slices.Contains(credentials, "ANTHROPIC_API_KEY") && slices.Contains(credentials, "ANTHROPIC_AUTH_TOKEN") {
			return fmt.Errorf("models[%d] %s: with both ANTHROPIC_API_KEY and ANTHROPIC_AUTH_TOKEN, an anthropic model needs api_key: os.environ/ANTHROPIC_API_KEY, or the API key's provider another credential name", i, m.Name)
		}
		if _, ok := m.Params["api_key"]; !ok && strings.HasPrefix(model, "anthropic/") &&
			slices.Contains(credentials, "ANTHROPIC_AUTH_TOKEN") && !slices.Contains(credentials, "ANTHROPIC_API_KEY") {
			c.subscription = true
		}
		names[m.Name] = true
	}
	for _, name := range c.Keys.Models {
		if !names[name] {
			return fmt.Errorf("keys: models: %s is not a model_name of models", name)
		}
	}
	if b := c.Keys.MaxBudget; b != nil && (*b <= 0 || math.IsInf(*b, 0) || math.IsNaN(*b)) {
		return errors.New("keys: max_budget must be a positive number of US dollars")
	}
	if d := c.Keys.BudgetDuration; d != "" {
		if !durationRE.MatchString(d) {
			return fmt.Errorf("keys: budget_duration %q is not a duration such as 30d, 24h or 1mo", d)
		}
		if c.Keys.MaxBudget == nil {
			return errors.New("keys: budget_duration needs max_budget")
		}
	}
	return nil
}

func (m Model) validate(credentials []string) error {
	if !modelRE.MatchString(m.Name) {
		return fmt.Errorf("invalid model_name %q", m.Name)
	}
	if m.Params == nil {
		return errors.New("missing litellm_params")
	}
	for _, k := range slices.Sorted(maps.Keys(m.Params)) {
		if !slices.Contains(allowedParams, k) {
			return fmt.Errorf("litellm_params: %s is not allowed here; brig allows %s, and keys belong in providers", k, strings.Join(allowedParams, ", "))
		}
	}
	model, _ := m.Params["model"].(string)
	if !modelRE.MatchString(model) || !strings.Contains(model, "/") {
		return fmt.Errorf("litellm_params: model %v must be PROVIDER/MODEL, such as anthropic/claude-sonnet-4-5", m.Params["model"])
	}
	if v, ok := m.Params["api_key"]; ok {
		s, _ := v.(string)
		sub := envRefRE.FindStringSubmatch(s)
		switch {
		case sub == nil:
			return errors.New("litellm_params: api_key must be os.environ/NAME, where NAME is a credential of a provider; never the key itself")
		case !slices.Contains(credentials, sub[1]):
			if len(credentials) == 0 {
				return fmt.Errorf("litellm_params: api_key names %s, but the config directory has no providers", sub[1])
			}
			return fmt.Errorf("litellm_params: api_key names %s, which no provider has (have %s)", sub[1], strings.Join(credentials, ", "))
		}
	}
	if v, ok := m.Params["api_base"]; ok {
		s, _ := v.(string)
		u, err := url.Parse(s)
		if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
			return fmt.Errorf("litellm_params: api_base %v must be an https URL without user or query", v)
		}
	}
	if v, ok := m.Params["extra_headers"]; ok {
		h, ok := v.(map[string]any)
		if !ok {
			return errors.New("litellm_params: extra_headers must map header names to values")
		}
		for _, name := range slices.Sorted(maps.Keys(h)) {
			if _, ok := h[name].(string); !ok || !headerRE.MatchString(name) {
				return fmt.Errorf("litellm_params: extra_headers: %s must be a header name with a string value", name)
			}
			if secretHeaderRE.MatchString(name) {
				return fmt.Errorf("litellm_params: extra_headers: %s may carry a credential, which belongs in a provider", name)
			}
		}
	}
	for _, k := range []string{"max_retries", "rpm", "tpm", "timeout", "stream_timeout"} {
		if v, ok := m.Params[k]; ok {
			if n, ok := number(v); !ok || n < 0 {
				return fmt.Errorf("litellm_params: %s must be a number of at least 0", k)
			}
		}
	}
	if v, ok := m.Params["api_version"]; ok {
		if _, ok := v.(string); !ok {
			return errors.New("litellm_params: api_version must be a string")
		}
	}
	return nil
}

func number(v any) (float64, bool) {
	switch n := v.(type) {
	case int:
		return float64(n), true
	case float64:
		return n, true
	}
	return 0, false
}

// MasterKeyEnv and SaltKeyEnv are the variables in which the LiteLLM
// sandbox gets the service's secrets.
const (
	MasterKeyEnv = "LITELLM_MASTER_KEY"
	SaltKeyEnv   = "LITELLM_SALT_KEY"
)

// LiteLLMConfig returns LiteLLM's config.yaml for c.
func (c *Config) LiteLLMConfig() ([]byte, error) {
	type out struct {
		Models  []Model        `yaml:"model_list"`
		General map[string]any `yaml:"general_settings"`
	}
	return yaml.Marshal(out{
		Models: c.Models,
		General: map[string]any{
			"master_key": "os.environ/" + MasterKeyEnv,
			// Keys and models come from brig, not from LiteLLM's UI.
			"store_model_in_db": false,
		},
	})
}
