// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

// Package agent holds the files that brig agent init writes: an image
// definition for a coding agent (Claude Code, Pi or opencode) that runs in an
// OpenShell sandbox, with the provider profile, provider and default policy
// that go with it. The files are the ones that docs/how-to/agents.md shows;
// a test keeps the two in step.
package agent

import (
	"bytes"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"text/template"

	"go.yaml.in/yaml/v3"
)

//go:embed all:templates
var templates embed.FS

// Versions of the agents that the images install, bumped with brig releases.
// The guide shows the same numbers; the test that compares them fails until
// both are changed.
const (
	ClaudeVersion   = "2.1.295"
	PiVersion       = "1.1.0"
	OpenCodeVersion = "1.19.0"
)

// Kind is a coding agent that brig agent init knows.
type Kind string

// The agents.
const (
	Claude   Kind = "claude"
	Pi       Kind = "pi"
	OpenCode Kind = "opencode"
)

// Kinds are the known agents.
var Kinds = []Kind{Claude, Pi, OpenCode}

// ParseKind returns the agent called s.
func ParseKind(s string) (Kind, error) {
	k := Kind(s)
	if !slices.Contains(Kinds, k) {
		return "", fmt.Errorf("unknown agent %q; brig agent init knows claude, pi and opencode", s)
	}
	return k, nil
}

// Root says in which of the two directories a file belongs: the shared one,
// often in git, or the personal one that only points into the keyring.
type Root int

// The two roots.
const (
	Shared Root = iota
	Secrets
)

// File is one file for brig agent init to write.
type File struct {
	Root Root
	// Path is relative to the root, with forward slashes.
	Path string
	Mode fs.FileMode
	Data []byte
	// Defines says what a config directory file defines, such as
	// "profile:llm", "provider:github" or "policy:default", so that brig
	// agent init can leave it out when another directory defines it already.
	Defines string
}

// Spec says what to write.
type Spec struct {
	Kind Kind
	// Name is the agent's name, by default its kind; it names the directory
	// agents/NAME, the image localhost/NAME-agent and the template.
	Name string
	// Endpoint is the base URL of an OpenAI-compatible API, for Pi and
	// opencode; Model is the model to use there.
	Endpoint string
	Model    string
	// Anthropic makes opencode use Anthropic's API with an API key, the
	// optional Model being one of its model IDs.
	Anthropic bool
	// GitHub, if set, also gives the agent a GitHub provider whose token is
	// in the keyring for this login.
	GitHub string
	// User is the login that names the Claude and Anthropic secrets in the
	// keyring.
	User string
	// LLMProxy, if set, sends the agent's model calls to an llm-proxy
	// service, which holds the model keys, in place of a provider of the
	// agent's own.
	LLMProxy *LLMProxy
}

// LLMProxy is the llm-proxy service that an agent calls.
type LLMProxy struct {
	Service string
	// Port is the service's port, where sandboxes reach it at
	// http://host.openshell.internal:PORT.
	Port int
	// Subscription says that the proxy calls Anthropic with a Claude
	// subscription token, which Anthropic takes only with the OAuth beta
	// that Claude Code then names in its requests.
	Subscription bool
}

// The variables in which an attach of the llm-proxy service puts the
// placeholder of the sandbox's key (see its type's credential.env).
const (
	proxyClaudeEnv = "ANTHROPIC_AUTH_TOKEN"
	proxyKeyEnv    = "LLM_PROXY_KEY"
)

// SubscriptionBeta is the beta that Anthropic wants with a Claude
// subscription token.
const SubscriptionBeta = "oauth-2025-04-20"

// SandboxHost is the name at which a sandbox reaches the host's services;
// service.SandboxHost is the same.
const SandboxHost = "host.openshell.internal"

// Plan is what brig agent init writes for a Spec.
type Plan struct {
	Name  string
	Files []File
	// Definition is the content of agents/NAME/agent.yaml.
	Definition Definition
}

// Definition is agents/NAME/agent.yaml: what brig agent build builds and what
// a new sandbox of the agent attaches.
type Definition struct {
	// Image is the repository of the image, without a tag.
	Image string `yaml:"image"`
	// Template is the name of the gateway template that brig agent build
	// keeps pointing at the newest image.
	Template string `yaml:"template"`
	// Providers are what a new sandbox of the agent attaches.
	Providers []string `yaml:"providers"`
	// Services are the brig services that a new sandbox of the agent is
	// attached to, such as an llm-proxy.
	Services []string `yaml:"services,omitempty"`
	// Command is what starts the agent in a sandbox; informational.
	Command string `yaml:"command"`
}

var (
	nameRE  = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,39}$`)
	modelRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/@+-]*$`)
	hostRE  = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9.-]*[A-Za-z0-9])?$`)
	// loginRE matches GitHub logins.
	loginRE = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9-]*[A-Za-z0-9])?$`)
)

// CheckName reports whether s can name an agent.
func CheckName(s string) error {
	if !nameRE.MatchString(s) {
		return fmt.Errorf("invalid agent name %q: use lowercase letters, digits and -, starting with a letter or digit", s)
	}
	return nil
}

// ParseDefinition reads an agent.yaml.
func ParseDefinition(data []byte) (Definition, error) {
	var d Definition
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&d); err != nil {
		return Definition{}, err
	}
	if d.Image == "" {
		return Definition{}, errors.New("missing image")
	}
	if i := strings.LastIndex(d.Image, ":"); i > strings.LastIndex(d.Image, "/") {
		return Definition{}, fmt.Errorf("image %s must not have a tag; brig agent build adds a date", d.Image)
	}
	return d, nil
}

// marshal formats d as an agent.yaml with a comment for each setting.
func (d Definition) marshal() []byte {
	b := fmt.Appendf(nil, `image: %-28s # brig agent build adds a date tag
template: %-25s # kept pointing at the newest build
providers: %-24s # what a new sandbox of this agent attaches
`, d.Image, d.Template, "["+strings.Join(d.Providers, ", ")+"]")
	if len(d.Services) > 0 {
		b = fmt.Appendf(b, "services: %-25s # the brig services it is attached to\n", "["+strings.Join(d.Services, ", ")+"]")
	}
	return fmt.Appendf(b, "command: %-26s # shown by Connect; informational\n", d.Command)
}

// data is what the templates see.
type data struct {
	Version      string
	Model        string
	EndpointURL  string
	EndpointHost string
	EndpointPort string
	// KeyEnv holds the endpoint's key, or its placeholder, for Pi and
	// opencode.
	KeyEnv string
	// ProxyURL is the llm-proxy's URL for Claude Code, and ProxyBetas the
	// betas it adds to its requests.
	ProxyURL   string
	ProxyBetas string
}

// endpoint is a parsed Spec.Endpoint.
type endpoint struct{ url, host, port string }

func parseEndpoint(raw string) (endpoint, error) {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return endpoint{}, fmt.Errorf("invalid endpoint %q: want an http or https URL such as https://llm.example.com/v1", raw)
	}
	host := u.Hostname()
	if !hostRE.MatchString(host) {
		return endpoint{}, fmt.Errorf("invalid endpoint host %q", host)
	}
	port := u.Port()
	if port == "" {
		port = map[string]string{"https": "443", "http": "80"}[u.Scheme]
	} else if n, err := strconv.Atoi(port); err != nil || n < 1 || n > 65535 {
		return endpoint{}, fmt.Errorf("invalid endpoint port %q", port)
	}
	if strings.ContainsAny(raw, `"\ `) {
		return endpoint{}, fmt.Errorf("invalid endpoint %q", raw)
	}
	return endpoint{url: raw, host: host, port: port}, nil
}

// New returns the plan for spec.
func New(spec Spec) (*Plan, error) {
	if spec.Name == "" {
		spec.Name = string(spec.Kind)
	}
	if err := CheckName(spec.Name); err != nil {
		return nil, err
	}
	if spec.Model != "" && !modelRE.MatchString(spec.Model) {
		return nil, fmt.Errorf("invalid model ID %q", spec.Model)
	}
	if spec.GitHub != "" && !loginRE.MatchString(spec.GitHub) {
		return nil, fmt.Errorf("invalid GitHub login %q", spec.GitHub)
	}
	if px := spec.LLMProxy; px != nil {
		if spec.Endpoint != "" || spec.Anthropic {
			return nil, errors.New("an agent of the llm-proxy calls no --endpoint and no --anthropic: the proxy holds the model keys")
		}
		if px.Port < 1 || px.Port > 65535 || !nameRE.MatchString(px.Service) {
			return nil, fmt.Errorf("invalid llm-proxy service %q at port %d", px.Service, px.Port)
		}
		spec.Endpoint = fmt.Sprintf("http://%s:%d/v1", SandboxHost, px.Port)
	}
	var ep endpoint
	if spec.Endpoint != "" {
		var err error
		if ep, err = parseEndpoint(spec.Endpoint); err != nil {
			return nil, err
		}
	}
	p := &Plan{Name: spec.Name}
	d := data{Model: spec.Model, EndpointURL: ep.url, EndpointHost: ep.host, EndpointPort: ep.port, KeyEnv: "LLM_API_KEY"}
	px := spec.LLMProxy
	if px != nil {
		d.KeyEnv = proxyKeyEnv
		p.Definition.Services = []string{px.Service}
	}
	var providers []string
	dir := "agents/" + spec.Name + "/"
	switch spec.Kind {
	case Claude:
		if px == nil && (spec.Endpoint != "" || spec.Model != "" || spec.Anthropic) {
			return nil, errors.New("claude takes a subscription token; --endpoint, --model and --anthropic are for pi and opencode")
		}
		if px != nil {
			if spec.Model != "" {
				return nil, errors.New("claude picks its model itself; --model is for pi and opencode")
			}
			// Claude Code adds /v1/messages to the base URL itself.
			d.ProxyURL = strings.TrimSuffix(ep.url, "/v1")
			if px.Subscription {
				d.ProxyBetas = SubscriptionBeta
			}
		}
		d.Version = ClaudeVersion
		p.add(Shared, dir+"Containerfile", 0o644, "", render(d, "claude/Containerfile"))
		p.add(Shared, dir+"claude-sandbox", 0o755, "", file("claude/claude-sandbox"))
		p.add(Shared, dir+"managed-settings.json", 0o644, "", render(d, "claude/managed-settings.json"))
		p.add(Shared, dir+"etc/CLAUDE.md", 0o644, "", file("claude/etc/CLAUDE.md"))
		p.add(Shared, dir+"skills/.gitkeep", 0o644, "", nil)
		if px == nil {
			p.add(Shared, "profiles/claude-code.yaml", 0o644, "profile:claude-code", file("shared/claude-code.yaml"))
			p.add(Secrets, "providers/claude-code.yaml", 0o644, "provider:claude-code",
				providerFile("claude-code", "claude-code", "CLAUDE_CODE_OAUTH_TOKEN", "service", "claude.ai", "user", spec.User))
			providers = append(providers, "claude-code")
		}
		p.Definition.Command = "claude-sandbox"
	case Pi:
		if spec.Anthropic {
			return nil, errors.New("--anthropic is for opencode; Pi talks to an --endpoint")
		}
		if px != nil && spec.Model == "" {
			return nil, errors.New("pi needs --model ID: a model_name of the llm-proxy's llm-proxy.yaml")
		}
		if spec.Endpoint == "" || spec.Model == "" {
			return nil, errors.New("pi needs --endpoint URL and --model ID: the OpenAI-compatible API it talks to")
		}
		d.Version = PiVersion
		p.add(Shared, dir+"Containerfile", 0o644, "", render(d, "pi/Containerfile"))
		p.add(Shared, dir+"pi-sandbox", 0o755, "", file("pi/pi-sandbox"))
		p.add(Shared, dir+"etc/settings.json", 0o644, "", render(d, "pi/etc/settings.json"))
		p.add(Shared, dir+"etc/models.json", 0o644, "", render(d, "pi/etc/models.json"))
		p.add(Shared, dir+"etc/AGENTS.md", 0o644, "", file("pi/etc/AGENTS.md"))
		if px == nil {
			p.addLLM(d, ep)
			providers = append(providers, "llm")
		}
		p.Definition.Command = "pi-sandbox"
	case OpenCode:
		d.Version = OpenCodeVersion
		p.add(Shared, dir+"Containerfile", 0o644, "", render(d, "opencode/Containerfile"))
		p.add(Shared, dir+"opencode-sandbox", 0o755, "", file("opencode/opencode-sandbox"))
		p.add(Shared, dir+"etc/AGENTS.md", 0o644, "", file("opencode/etc/AGENTS.md"))
		switch {
		case spec.Anthropic && spec.Endpoint != "":
			return nil, errors.New("opencode uses either --anthropic or an --endpoint, not both")
		case spec.Anthropic:
			p.add(Shared, dir+"etc/opencode.json", 0o644, "", render(d, "opencode/etc/opencode-anthropic.json"))
			p.add(Shared, "profiles/anthropic.yaml", 0o644, "profile:anthropic", file("shared/anthropic.yaml"))
			p.add(Secrets, "providers/anthropic.yaml", 0o644, "provider:anthropic",
				providerFile("anthropic", "anthropic", "ANTHROPIC_API_KEY", "service", "anthropic.com", "user", spec.User))
			providers = append(providers, "anthropic")
		case spec.Endpoint != "" && spec.Model != "":
			p.add(Shared, dir+"etc/opencode.json", 0o644, "", render(d, "opencode/etc/opencode-endpoint.json"))
			if px == nil {
				p.addLLM(d, ep)
				providers = append(providers, "llm")
			}
		case px != nil:
			return nil, errors.New("opencode needs --model ID: a model_name of the llm-proxy's llm-proxy.yaml")
		default:
			return nil, errors.New("opencode needs --endpoint URL and --model ID, or --anthropic for Anthropic's API with an API key")
		}
		p.Definition.Command = "opencode-sandbox"
	default:
		return nil, fmt.Errorf("unknown agent %q", spec.Kind)
	}
	if spec.GitHub != "" {
		p.add(Secrets, "providers/github.yaml", 0o644, "provider:github",
			providerFile("github", "github", "GH_TOKEN", "service", "github.com", "user", spec.GitHub))
		providers = append(providers, "github")
	}
	p.add(Shared, "policies/default.yaml", 0o644, "policy:default", file("shared/default-policy.yaml"))
	p.Definition.Image = "localhost/" + spec.Name + "-agent"
	p.Definition.Template = spec.Name
	p.Definition.Providers = providers
	p.add(Shared, dir+"agent.yaml", 0o644, "", p.Definition.marshal())
	slices.SortFunc(p.Files, func(a, b File) int {
		if a.Root != b.Root {
			return int(a.Root - b.Root)
		}
		return strings.Compare(a.Path, b.Path)
	})
	return p, nil
}

func (p *Plan) add(root Root, path string, mode fs.FileMode, defines string, data []byte) {
	p.Files = append(p.Files, File{Root: root, Path: path, Mode: mode, Data: data, Defines: defines})
}

// addLLM adds the profile and provider that Pi and opencode share.
func (p *Plan) addLLM(d data, ep endpoint) {
	p.add(Shared, "profiles/llm.yaml", 0o644, "profile:llm", render(d, "shared/llm.yaml"))
	p.add(Secrets, "providers/llm.yaml", 0o644, "provider:llm",
		providerFile("llm", "llm", "LLM_API_KEY", "service", ep.host))
}

// file returns an embedded file, which must exist.
func file(name string) []byte {
	b, err := templates.ReadFile("templates/" + name)
	if err != nil {
		panic(err)
	}
	return b
}

// render fills in the embedded template name.
func render(d data, name string) []byte {
	t, err := template.New(path.Base(name)).Option("missingkey=error").Parse(string(file(name)))
	if err != nil {
		panic(fmt.Sprintf("%s: %v", name, err))
	}
	var out bytes.Buffer
	if err := t.Execute(&out, d); err != nil {
		panic(fmt.Sprintf("%s: %v", name, err))
	}
	return out.Bytes()
}

// providerFile writes a provider file whose one credential is looked up by
// the attribute/value pairs attrs.
func providerFile(name, typ, credential string, attrs ...string) []byte {
	words := make([]string, len(attrs))
	for i, a := range attrs {
		words[i] = yamlScalar(a)
	}
	return fmt.Appendf(nil, `name: %s
type: %s
credentials:
  %s:
    secret_tool:                  # the arguments of secret-tool lookup
      lookup: [%s]
`, name, typ, credential, strings.Join(words, ", "))
}

var plainRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._@/-]*$`)

// yamlScalar writes s as a YAML scalar in a flow sequence.
func yamlScalar(s string) string {
	if plainRE.MatchString(s) {
		return s
	}
	return strconv.Quote(s)
}

// Result says what Write did with one file.
type Result struct {
	File File
	// Path is where the file is, or would be.
	Path string
	// State is "created", "unchanged", "overwritten" or "skipped".
	State string
	// Note says why a file was skipped.
	Note string
}

// Roots are the directories the two roots stand for.
type Roots struct{ Shared, Secrets string }

func (r Roots) dir(root Root) string {
	if root == Secrets {
		return r.Secrets
	}
	return r.Shared
}

// ConflictError lists the files that exist already with other content.
type ConflictError struct{ Paths []string }

func (e *ConflictError) Error() string {
	return "these files exist with other content; compare them, or overwrite them with --force:\n  " + strings.Join(e.Paths, "\n  ")
}

// Write writes the files below roots. A file that exists with the same content
// is left alone; one with other content stops the write with a ConflictError
// before anything is written, unless force is set. skip names files not to
// write, with the reason, by path under their root.
func Write(files []File, roots Roots, force bool, skip func(File) string) ([]Result, error) {
	results := make([]Result, len(files))
	var conflicts []string
	for i, f := range files {
		target := filepath.Join(roots.dir(f.Root), filepath.FromSlash(f.Path))
		results[i] = Result{File: f, Path: target}
		if note := skip(f); note != "" {
			results[i].State, results[i].Note = "skipped", note
			continue
		}
		existing, err := os.ReadFile(target)
		switch {
		case errors.Is(err, fs.ErrNotExist):
			results[i].State = "created"
		case err != nil:
			return nil, err
		case bytes.Equal(existing, f.Data):
			results[i].State = "unchanged"
		case force:
			results[i].State = "overwritten"
		default:
			conflicts = append(conflicts, target)
		}
	}
	if len(conflicts) > 0 {
		return nil, &ConflictError{Paths: conflicts}
	}
	for _, r := range results {
		if r.State != "created" && r.State != "overwritten" {
			continue
		}
		dirMode := fs.FileMode(0o755)
		if r.File.Root == Secrets {
			dirMode = 0o700
		}
		if err := os.MkdirAll(filepath.Dir(r.Path), dirMode); err != nil {
			return nil, err
		}
		if err := writeFile(r.Path, r.File.Data, r.File.Mode); err != nil {
			return nil, err
		}
	}
	return results, nil
}

// writeFile writes data to a temporary file next to path and renames it, so
// that a reader never sees half a file.
func writeFile(path string, data []byte, mode fs.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".brig-agent-")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Chmod(mode); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
