// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

// Package service defines brig's services: workloads that agents in a
// gateway's sandboxes may use but never control, such as a model proxy.
//
// A service has a type, which decides what runs and where. Types are built
// into brig as manifests in types/*.yaml (see Type); they are not loaded from
// the user's configuration, so that the format can change while the first
// types shape it.
//
// Attaching a service to a sandbox gives that sandbox a token of its own:
// brig mints it, registers it with the service through the type's Driver,
// stores it as an OpenShell provider on the sandbox's gateway with a profile
// that brig generates (see Profile), and attaches the provider to the
// sandbox. The agent sees only OpenShell's placeholder; OpenShell's proxy
// adds the token to the requests that the profile's binaries send to the
// service, which then knows which gateway, workspace and sandbox calls it.
package service

import (
	"bytes"
	"embed"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"path"
	"regexp"
	"slices"
	"sort"
	"strings"

	"go.yaml.in/yaml/v3"

	"github.com/dennisklein/brig/internal/bytesize"
	"github.com/dennisklein/brig/internal/ocsync"
	"github.com/dennisklein/brig/internal/vm"
)

// Kind says which way trust points between a service and its callers.
type Kind string

const (
	// Broker holds a secret or a power and offers agents a narrow API. It
	// is trusted, one serves every gateway, and it tells its callers apart
	// by their tokens.
	Broker Kind = "broker"
	// Runner runs the agents' workloads, such as VMs or a browser. Agents
	// control what runs there, so it holds no secrets.
	Runner Kind = "runner"
)

// Placement says where a service runs.
type Placement string

const (
	// PlacementVM runs the service in a VM of its own, made from brig's base
	// image like a gateway, with its own network profile.
	PlacementVM Placement = "vm"
	// PlacementHost runs the service on the host, as a user unit; only what
	// must create VMs, such as the vm runner's controller, runs there.
	PlacementHost Placement = "host"
)

// Protocols that a service speaks to its callers, as OpenShell names them in
// a profile's endpoints. Both are HTTP, so OpenShell can add the token.
const (
	ProtocolREST = "rest"
	ProtocolMCP  = "mcp"
)

// Auth styles: how OpenShell adds the token to a request.
const (
	// AuthBearer sends "Authorization: Bearer TOKEN", or the token after
	// Bearer in Credential.Header.
	AuthBearer = "bearer"
	// AuthHeader sends the token as the value of Credential.Header.
	AuthHeader = "header"
)

// Type is a built-in service type, as its manifest types/NAME.yaml defines
// it:
//
//	name: llm-proxy
//	description: Model proxy with a key and a budget per sandbox
//	kind: broker
//	placement: vm
//	openshell: true
//	sandbox: {name: litellm, port: 4000}
//	protocol: rest
//	credential:
//	  env: [ANTHROPIC_AUTH_TOKEN, LLM_PROXY_KEY]
//	  auth: bearer
//	binaries: [/usr/local/bin/claude, /usr/bin/node]
//	network_profile: default
//	resources: {cpus: 2, memory: 4GiB}
//	secrets: [MASTER_KEY]
type Type struct {
	Name        string    `yaml:"name"`
	Description string    `yaml:"description"`
	Kind        Kind      `yaml:"kind"`
	Placement   Placement `yaml:"placement"`
	// OpenShell boots the service VM's OpenShell gateway, so that the
	// workload runs in a sandbox there and uses keys without reading them.
	// Third-party code that holds keys runs that way.
	OpenShell bool `yaml:"openshell"`
	// Port is where the workload listens: in the service VM for placement
	// vm, where brig forwards the service's host port to; unused for
	// placement host, whose controller listens on the host port, and with
	// Sandbox.
	Port int `yaml:"port"`
	// Sandbox, for a type with OpenShell, names the sandbox in the service
	// VM where the workload listens, on the sandbox's loopback interface,
	// which nothing in the VM's network can connect to. brig then forwards
	// nothing into the VM: the type's HostRunner (see servicedriver.go)
	// listens on the service's host port and passes connections on through
	// the service VM's gateway.
	Sandbox *SandboxPort `yaml:"sandbox"`
	// Protocol is ProtocolREST or ProtocolMCP.
	Protocol   string     `yaml:"protocol"`
	Credential Credential `yaml:"credential"`
	// Binaries are the absolute paths of the programs in a sandbox that may
	// call the service with the sandbox's token. OpenShell matches them by
	// path, so they are the real gate.
	Binaries []string `yaml:"binaries"`
	// NetworkProfile is the network profile of a new service VM (see
	// config.yaml), which limits its own egress.
	NetworkProfile string `yaml:"network_profile"`
	// Resources are a new service VM's; zero values come from config.yaml.
	Resources Resources `yaml:"resources"`
	// Secrets are the names of the secrets the workload needs, which a
	// config directory's services/NAME.yaml binds to the keyring.
	Secrets []string `yaml:"secrets"`
}

// SandboxPort is a port on the loopback interface of a sandbox.
type SandboxPort struct {
	Name string `yaml:"name"`
	Port int    `yaml:"port"`
}

// Credential says how a sandbox's token travels.
type Credential struct {
	// Env are the environment variables that hold the token's placeholder
	// in a sandbox, all with the same token, for agents that read it under
	// different names. Empty means one variable named after the service:
	// NAME_TOKEN, upper case, with underscores for dashes.
	Env []string `yaml:"env"`
	// Auth is AuthBearer (the default) or AuthHeader.
	Auth string `yaml:"auth"`
	// Header is the header that carries the token, by default
	// authorization.
	Header string `yaml:"header"`
}

// Resources are the CPUs and memory of a service VM.
type Resources struct {
	CPUs   int           `yaml:"cpus"`
	Memory bytesize.Size `yaml:"memory"`
}

var (
	// envRE matches environment variable names, as credential keys are.
	envRE = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	// headerRE matches HTTP header names.
	headerRE = regexp.MustCompile(`^[A-Za-z0-9-]+$`)
)

func (t Type) validate() error {
	if err := vm.ValidateName(t.Name); err != nil {
		return fmt.Errorf("type name: %w", err)
	}
	switch t.Kind {
	case Broker, Runner:
	default:
		return fmt.Errorf("kind must be %s or %s, not %q", Broker, Runner, t.Kind)
	}
	switch t.Placement {
	case PlacementVM:
		if t.Sandbox != nil {
			if err := t.Sandbox.validate(t.OpenShell, t.Port); err != nil {
				return err
			}
		} else if t.Port < 1 || t.Port > 65535 {
			return fmt.Errorf("port %d is not a TCP port", t.Port)
		}
	case PlacementHost:
		if t.OpenShell {
			return errors.New("a service on the host cannot run OpenShell")
		}
		if len(t.Secrets) > 0 {
			return errors.New("a service on the host has no VM to hold its secrets")
		}
		if t.Sandbox != nil {
			return errors.New("a service on the host has no sandbox")
		}
	default:
		return fmt.Errorf("placement must be %s or %s, not %q", PlacementVM, PlacementHost, t.Placement)
	}
	switch t.Protocol {
	case ProtocolREST, ProtocolMCP:
	default:
		return fmt.Errorf("protocol must be %s or %s, not %q", ProtocolREST, ProtocolMCP, t.Protocol)
	}
	for _, env := range t.Credential.Env {
		if err := checkEnv(env); err != nil {
			return fmt.Errorf("credential: %w", err)
		}
	}
	switch t.Credential.Auth {
	case "", AuthBearer:
	case AuthHeader:
		if t.Credential.Header == "" {
			return errors.New("credential: auth header needs a header")
		}
	default:
		return fmt.Errorf("credential: auth must be %s or %s, not %q", AuthBearer, AuthHeader, t.Credential.Auth)
	}
	if h := t.Credential.Header; h != "" && !headerRE.MatchString(h) {
		return fmt.Errorf("credential: invalid header %q", h)
	}
	if err := checkBinaries(t.Binaries); err != nil {
		return err
	}
	if len(t.Binaries) == 0 {
		return errors.New("binaries: name at least one program that may call the service")
	}
	if t.Resources.CPUs < 0 || t.Resources.Memory%bytesize.MiB != 0 {
		return errors.New("resources: cpus must not be negative, and memory must be a whole number of MiB")
	}
	for _, s := range t.Secrets {
		if !envRE.MatchString(s) {
			return fmt.Errorf("secrets: invalid name %q", s)
		}
	}
	return nil
}

// sandboxNameRE matches sandbox names, as action.ValidSandboxName does.
var sandboxNameRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,62}$`)

func (p SandboxPort) validate(openShell bool, port int) error {
	switch {
	case !openShell:
		return errors.New("sandbox: a type without OpenShell has no sandboxes")
	case port != 0:
		return errors.New("sandbox: the workload listens in the sandbox; leave port out")
	case !sandboxNameRE.MatchString(p.Name):
		return fmt.Errorf("sandbox: invalid name %q", p.Name)
	case p.Port < 1 || p.Port > 65535:
		return fmt.Errorf("sandbox: port %d is not a TCP port", p.Port)
	}
	return nil
}

// ForwardsPort reports whether brig forwards the service's host port to
// Port in the service VM; with Sandbox, it does not.
func (t Type) ForwardsPort() bool { return t.Placement == PlacementVM && t.Sandbox == nil }

// checkEnv checks a credential's environment variable, which brig hands to
// the openshell CLI in its environment.
func checkEnv(name string) error {
	return ocsync.CheckCredentialKey(name)
}

// checkBinaries checks programs that may call a service: clean absolute
// paths, without characters that YAML or OpenShell would read differently.
func checkBinaries(binaries []string) error {
	for _, b := range binaries {
		if !path.IsAbs(b) || path.Clean(b) != b || strings.ContainsFunc(b, func(r rune) bool {
			return r < 0x21 || r == 0x7f || strings.ContainsRune(`"'\`, r)
		}) {
			return fmt.Errorf("binaries: %q is not a clean absolute path", b)
		}
	}
	return nil
}

// CredentialEnv returns the environment variables that hold the token of a
// service called name of this type in a sandbox.
func (t Type) CredentialEnv(name string) []string {
	if len(t.Credential.Env) > 0 {
		return t.Credential.Env
	}
	return []string{strings.ToUpper(strings.ReplaceAll(name, "-", "_")) + "_TOKEN"}
}

// Catalog holds the service types brig knows.
type Catalog struct {
	types map[string]Type
}

//go:embed types
var builtinFS embed.FS

// Builtin returns the types built into brig. It panics if a manifest is
// invalid, which the package's tests rule out.
func Builtin() *Catalog {
	sub, err := fs.Sub(builtinFS, "types")
	if err != nil {
		panic(err)
	}
	c, err := LoadCatalog(sub)
	if err != nil {
		panic(err)
	}
	return c
}

// LoadCatalog reads the manifests *.yaml in fsys, whose base names must be
// their types' names.
func LoadCatalog(fsys fs.FS) (*Catalog, error) {
	files, err := fs.Glob(fsys, "*.yaml")
	if err != nil {
		return nil, err
	}
	c := &Catalog{types: map[string]Type{}}
	for _, file := range files {
		data, err := fs.ReadFile(fsys, file)
		if err != nil {
			return nil, err
		}
		t, err := ParseType(data)
		if err != nil {
			return nil, fmt.Errorf("service type %s: %w", file, err)
		}
		if t.Name+".yaml" != file {
			return nil, fmt.Errorf("service type %s: named %q", file, t.Name)
		}
		c.types[t.Name] = t
	}
	return c, nil
}

// NewCatalog returns a catalog of the given types, for tests.
func NewCatalog(types ...Type) (*Catalog, error) {
	c := &Catalog{types: map[string]Type{}}
	for _, t := range types {
		if err := t.validate(); err != nil {
			return nil, fmt.Errorf("service type %s: %w", t.Name, err)
		}
		c.types[t.Name] = t
	}
	return c, nil
}

// ParseType reads and checks a manifest. Unknown keys and further YAML
// documents are errors.
func ParseType(data []byte) (Type, error) {
	var t Type
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&t); err != nil {
		return Type{}, err
	}
	var extra yaml.Node
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		return Type{}, errors.New("more than one YAML document")
	}
	return t, t.validate()
}

// ErrUnknownType is returned for a type that brig does not have.
var ErrUnknownType = errors.New("unknown service type")

// Get returns the type called name.
func (c *Catalog) Get(name string) (Type, error) {
	t, ok := c.types[name]
	if !ok {
		have := c.Names()
		if len(have) == 0 {
			return Type{}, fmt.Errorf("%w %q: this brig has no service types yet", ErrUnknownType, name)
		}
		return Type{}, fmt.Errorf("%w %q (have %s)", ErrUnknownType, name, strings.Join(have, ", "))
	}
	return t, nil
}

// Names returns the names of the types, sorted.
func (c *Catalog) Names() []string {
	names := make([]string, 0, len(c.types))
	for name := range c.types {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// Types returns the types, sorted by name.
func (c *Catalog) Types() []Type {
	var out []Type
	for _, name := range c.Names() {
		out = append(out, c.types[name])
	}
	return slices.Clip(out)
}
