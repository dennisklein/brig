// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

// Package ocsync applies OpenShell config directories to a VM's gateway.
//
// A config directory holds OpenShell provider profiles, providers and a
// default sandbox policy:
//
//	profiles/*.yaml          OpenShell provider profile files, as they are
//	providers/*.yaml         providers, whose credentials reference secrets
//	                         in the user's keyring (see Provider)
//	workspaces/NAME/profiles, workspaces/NAME/providers
//	                         the same for the workspace NAME; the top-level
//	                         directories are for the default workspace
//	policies/default.yaml    the default policy for new sandboxes
//
// Profiles and policies contain no secrets and can be shared. Provider files
// name each credential's secret by its secret-tool lookup attributes, so no
// secret is ever written to a config directory.
package ocsync

import (
	"bytes"
	"cmp"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"go.yaml.in/yaml/v3"

	"github.com/dennisklein/brig/internal/openshell"
)

// Set is the merged content of a VM's config directories.
type Set struct {
	// Dirs are the config directories, in the order they were given.
	Dirs []string
	// Workspaces holds the profiles and providers of each workspace. A
	// directory's top-level profiles/ and providers/ fill the default
	// workspace, which is always present; workspaces/NAME/profiles and
	// workspaces/NAME/providers fill NAME.
	Workspaces map[string]WorkspaceSet
	// DefaultPolicy is the path of the default sandbox policy, if any.
	DefaultPolicy string
	// Policies are all the policy files, default.yaml included, sorted by
	// name and then path. Unlike the default policy, a name may occur in
	// several directories: brig only reads these files to compare them with
	// sandboxes (see PolicyFile).
	Policies []PolicyFile
}

// PolicyFile is a sandbox policy file in a config directory's policies
// directory, such as policies/webapp.yaml, which is a convention for one
// project's policy: the default's content plus the project's network rules.
type PolicyFile struct {
	// Name is the file name without its extension.
	Name string
	Path string
}

// ProfileFile is an OpenShell provider profile file.
type ProfileFile struct {
	ID        string
	Path      string
	Data      []byte
	Endpoints []openshell.Endpoint
}

// SHA256 is the hex-encoded SHA-256 of the file's content.
func (p ProfileFile) SHA256() string {
	sum := sha256.Sum256(p.Data)
	return hex.EncodeToString(sum[:])
}

// Provider is an OpenShell provider instance. Its file looks like this:
//
//	name: github
//	type: github
//	credentials:
//	  GH_TOKEN:
//	    secret_tool:
//	      lookup: [service, github.com, user, alice]
//
// lookup holds the attribute/value pairs of `secret-tool lookup`. There is
// deliberately no way to give a credential's value in the file.
type Provider struct {
	Name        string                `yaml:"name"`
	Type        string                `yaml:"type"`
	Credentials map[string]Credential `yaml:"credentials"`
	// Path is the file the provider was read from.
	Path string `yaml:"-"`
}

// Credential says where a provider credential's value comes from.
type Credential struct {
	SecretTool *SecretToolRef `yaml:"secret_tool"`
}

// SecretToolRef names a secret in the Secret Service by the attributes that
// `secret-tool lookup` takes.
type SecretToolRef struct {
	Lookup []string `yaml:"lookup"`
}

// String formats the reference as the arguments of `secret-tool lookup`.
func (r SecretToolRef) String() string {
	return strings.Join(r.Lookup, " ")
}

var (
	// nameRE matches OpenShell resource names and profile IDs.
	nameRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)
	// envRE matches environment variable names, which credential keys are.
	envRE = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	// reservedEnvRE matches credential keys brig cannot hand to the openshell
	// CLI in its environment without changing how the CLI itself runs, and
	// OpenShell's own v<digits>_ prefix.
	reservedEnvRE = regexp.MustCompile(`^(?i:(OPENSHELL|XDG|LD|RUST|SSL|LC)_.*|PATH|HOME|USER|LOGNAME|SHELL|TMPDIR|LANG|TERM|HTTPS?_PROXY|NO_PROXY|ALL_PROXY)$|^v[0-9]+_`)
)

// Load reads the config directories dirs. A profile ID, provider name or
// default policy defined in more than one place is an error, so that one
// directory never silently overrides another.
func Load(dirs []string) (*Set, error) {
	s := &Set{Dirs: dirs, Workspaces: map[string]WorkspaceSet{openshell.DefaultWorkspace: {}}}
	profileAt := map[string]string{}
	providerAt := map[string]string{}
	for _, dir := range dirs {
		if !filepath.IsAbs(dir) {
			return nil, fmt.Errorf("OpenShell config directory %q is not an absolute path", dir)
		}
		fi, err := os.Stat(dir)
		if err != nil {
			return nil, fmt.Errorf("OpenShell config directory: %w", err)
		}
		if !fi.IsDir() {
			return nil, fmt.Errorf("OpenShell config directory %s is not a directory", dir)
		}
		if err := s.loadWorkspace(openshell.DefaultWorkspace, dir, profileAt, providerAt); err != nil {
			return nil, err
		}
		names, err := workspaceDirs(filepath.Join(dir, "workspaces"))
		if err != nil {
			return nil, err
		}
		for _, name := range names {
			if err := s.loadWorkspace(name, filepath.Join(dir, "workspaces", name), profileAt, providerAt); err != nil {
				return nil, err
			}
		}
		files, err := yamlFiles(filepath.Join(dir, "policies"))
		if err != nil {
			return nil, err
		}
		for _, path := range files {
			name := strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
			s.Policies = append(s.Policies, PolicyFile{Name: name, Path: path})
		}
		policy := filepath.Join(dir, "policies", "default.yaml")
		if fi, err := os.Stat(policy); err == nil {
			if !fi.Mode().IsRegular() {
				return nil, fmt.Errorf("%s is not a regular file", policy)
			}
			if s.DefaultPolicy != "" {
				return nil, fmt.Errorf("default policy is defined in %s and %s", s.DefaultPolicy, policy)
			}
			s.DefaultPolicy = policy
		} else if !errors.Is(err, fs.ErrNotExist) {
			return nil, err
		}
	}
	for name, ws := range s.Workspaces {
		slices.SortFunc(ws.Profiles, func(a, b ProfileFile) int { return strings.Compare(a.ID, b.ID) })
		slices.SortFunc(ws.Providers, func(a, b Provider) int { return strings.Compare(a.Name, b.Name) })
		s.Workspaces[name] = ws
	}
	slices.SortFunc(s.Policies, func(a, b PolicyFile) int {
		return cmp.Or(strings.Compare(a.Name, b.Name), strings.Compare(a.Path, b.Path))
	})
	return s, nil
}

// WorkspaceSet is what the config directories define for one workspace.
type WorkspaceSet struct {
	// Profiles are sorted by ID.
	Profiles []ProfileFile
	// Providers are sorted by name.
	Providers []Provider
}

// Profile returns the profile with the given ID, if the workspace has it.
func (w WorkspaceSet) Profile(id string) (ProfileFile, bool) {
	i := slices.IndexFunc(w.Profiles, func(p ProfileFile) bool { return p.ID == id })
	if i < 0 {
		return ProfileFile{}, false
	}
	return w.Profiles[i], true
}

// loadWorkspace reads base/profiles and base/providers into workspace ws.
// A profile ID or provider name may recur across workspaces but not within
// one.
func (s *Set) loadWorkspace(ws, base string, profileAt, providerAt map[string]string) error {
	set := s.Workspaces[ws]
	files, err := yamlFiles(filepath.Join(base, "profiles"))
	if err != nil {
		return err
	}
	for _, path := range files {
		p, err := loadProfile(path)
		if err != nil {
			return err
		}
		key := ws + "\x00" + p.ID
		if prev, ok := profileAt[key]; ok {
			return fmt.Errorf("profile %s is defined in %s and %s", p.ID, prev, path)
		}
		profileAt[key] = path
		set.Profiles = append(set.Profiles, p)
	}
	if files, err = yamlFiles(filepath.Join(base, "providers")); err != nil {
		return err
	}
	for _, path := range files {
		p, err := loadProvider(path)
		if err != nil {
			return err
		}
		key := ws + "\x00" + p.Name
		if prev, ok := providerAt[key]; ok {
			return fmt.Errorf("provider %s is defined in %s and %s", p.Name, prev, path)
		}
		providerAt[key] = path
		set.Providers = append(set.Providers, p)
	}
	s.Workspaces[ws] = set
	return nil
}

// workspaceDirs lists the workspace directories in dir, which may be
// missing. Names are validated like OpenShell's.
func workspaceDirs(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var names []string
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".") {
			continue
		}
		if fi, err := os.Stat(filepath.Join(dir, e.Name())); err != nil || !fi.IsDir() {
			continue
		}
		if err := openshell.CheckWorkspace(e.Name()); err != nil {
			return nil, fmt.Errorf("%s: %w", dir, err)
		}
		names = append(names, e.Name())
	}
	return names, nil
}

// WorkspaceNames returns the names of the workspaces the set defines, the
// default workspace first and the others sorted.
func (s *Set) WorkspaceNames() []string {
	return orderedWorkspaces(slices.Collect(maps.Keys(s.Workspaces)))
}

// orderedWorkspaces returns the distinct names with the default workspace
// first, which is always included, and the others sorted.
func orderedWorkspaces(names []string) []string {
	others := slices.DeleteFunc(slices.Clone(names), func(n string) bool { return n == openshell.DefaultWorkspace })
	slices.Sort(others)
	return append([]string{openshell.DefaultWorkspace}, slices.Compact(others)...)
}

// yamlFiles lists the .yaml, .yml and .json files in dir, which may be
// missing. Hidden files are skipped: an editor's lock file, such as Emacs's
// ".#github.yaml", is a dangling symbolic link that is not a config file.
func yamlFiles(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var files []string
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".") {
			continue
		}
		switch filepath.Ext(e.Name()) {
		case ".yaml", ".yml", ".json":
			if e.Type().IsRegular() || e.Type()&fs.ModeSymlink != 0 {
				files = append(files, filepath.Join(dir, e.Name()))
			}
		}
	}
	return files, nil
}

// maxConfigFileSize bounds the files brig reads from config directories.
const maxConfigFileSize = 1 << 20

// readConfigFile reads a file from a config directory, following symbolic
// links, but only a regular file of bounded size: a shared directory must
// not make brig read a device or wait on a FIFO.
func readConfigFile(path string) ([]byte, error) {
	// Check the type before opening: opening a FIFO blocks until a writer
	// appears, and opening a device can have side effects.
	fi, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if !fi.Mode().IsRegular() {
		return nil, fmt.Errorf("%s is not a regular file", path)
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	// The file may have been replaced since the check above.
	fi, err = f.Stat()
	if err != nil {
		return nil, err
	}
	if !fi.Mode().IsRegular() {
		return nil, fmt.Errorf("%s is not a regular file", path)
	}
	data, err := io.ReadAll(io.LimitReader(f, maxConfigFileSize+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxConfigFileSize {
		return nil, fmt.Errorf("%s is larger than %d bytes", path, maxConfigFileSize)
	}
	return data, nil
}

// yamlSource returns the content of a config file in a form the YAML decoder
// reads as the file's author meant. The decoder's escape set is YAML's, not
// JSON's: it rejects \/ and the surrogate pairs that tools such as Python's
// json.dump write for characters outside the Basic Multilingual Plane. A
// .json file is therefore decoded as JSON and written out again, which
// leaves no escape the YAML decoder cannot read.
func yamlSource(path string, data []byte) ([]byte, error) {
	if filepath.Ext(path) != ".json" {
		return data, nil
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	var out bytes.Buffer
	enc := json.NewEncoder(&out)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return out.Bytes(), nil
}

// yaml.v3 puts a scalar into its error messages in several ways: in
// backticks, bare as a tag ("cannot unmarshal !tag `value` into T"), and
// in single quotes as an alias or anchor name.
var (
	quotedRE    = regexp.MustCompile("`[^`]*`")
	unmarshalRE = regexp.MustCompile(`cannot unmarshal .* into `)
	anchorRE    = regexp.MustCompile(`anchor '.*' `)
)

// redactYAMLError formats a YAML decoding error without the values it
// quotes, which in a provider file could be a secret written where a
// secret_tool reference belongs.
func redactYAMLError(err error) string {
	s := unmarshalRE.ReplaceAllString(err.Error(), "cannot unmarshal a value into ")
	s = anchorRE.ReplaceAllString(s, "anchor ")
	return quotedRE.ReplaceAllString(s, "a value")
}

// loadProfile reads what brig needs from an OpenShell profile file: its ID
// and endpoints. OpenShell validates the rest.
func loadProfile(path string) (ProfileFile, error) {
	data, err := readConfigFile(path)
	if err != nil {
		return ProfileFile{}, err
	}
	var doc struct {
		ID        string               `yaml:"id"`
		Endpoints []openshell.Endpoint `yaml:"endpoints"`
	}
	src, err := yamlSource(path, data)
	if err != nil {
		return ProfileFile{}, err
	}
	if err := yaml.Unmarshal(src, &doc); err != nil {
		return ProfileFile{}, fmt.Errorf("%s: %w", path, err)
	}
	if !nameRE.MatchString(doc.ID) {
		return ProfileFile{}, fmt.Errorf("%s: invalid or missing profile id %q", path, doc.ID)
	}
	if err := checkPortSpelling(src); err != nil {
		return ProfileFile{}, fmt.Errorf("%s: %w", path, err)
	}
	return ProfileFile{ID: doc.ID, Path: path, Data: data, Endpoints: doc.Endpoints}, nil
}

// leadingZeroRE matches a number that yaml.v3 reads as octal, which OpenShell's
// YAML 1.2 parser reads as decimal.
var leadingZeroRE = regexp.MustCompile(`^[+-]?0[0-9_]+$`)

// checkPortSpelling rejects endpoint ports written with a leading zero, like
// 0673. brig compares the endpoints it decodes (443) but sends the file as
// written, and OpenShell would enforce port 673, so a new port could slip past
// the hold on new endpoints. Aliases are followed, because an alias can pull the
// number in from an anchor anywhere in the file.
func checkPortSpelling(data []byte) error {
	var doc struct {
		Endpoints []struct {
			Port  yaml.Node `yaml:"port"`
			Ports yaml.Node `yaml:"ports"`
		} `yaml:"endpoints"`
	}
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return err
	}
	for _, e := range doc.Endpoints {
		for _, n := range append([]*yaml.Node{resolveAlias(&e.Port)}, sequenceItems(&e.Ports)...) {
			n = resolveAlias(n)
			if n.Kind == yaml.ScalarNode && leadingZeroRE.MatchString(n.Value) {
				return fmt.Errorf("endpoint port %s has a leading zero, which YAML parsers read differently; write it in decimal without one", n.Value)
			}
		}
	}
	return nil
}

func resolveAlias(n *yaml.Node) *yaml.Node {
	for n.Kind == yaml.AliasNode && n.Alias != nil {
		n = n.Alias
	}
	return n
}

func sequenceItems(n *yaml.Node) []*yaml.Node {
	n = resolveAlias(n)
	if n.Kind != yaml.SequenceNode {
		return nil
	}
	return n.Content
}

func loadProvider(path string) (Provider, error) {
	data, err := readConfigFile(path)
	if err != nil {
		return Provider{}, err
	}
	src, err := yamlSource(path, data)
	if err != nil {
		return Provider{}, err
	}
	var p Provider
	dec := yaml.NewDecoder(bytes.NewReader(src))
	dec.KnownFields(true)
	if err := dec.Decode(&p); err != nil {
		return Provider{}, fmt.Errorf("%s: %s (credentials take only secret_tool references, never values)", path, redactYAMLError(err))
	}
	p.Path = path
	if err := p.validate(); err != nil {
		return Provider{}, fmt.Errorf("%s: %w", path, err)
	}
	return p, nil
}

func (p Provider) validate() error {
	if !nameRE.MatchString(p.Name) {
		return fmt.Errorf("invalid or missing provider name %q", p.Name)
	}
	if !nameRE.MatchString(p.Type) {
		return fmt.Errorf("invalid or missing provider type %q", p.Type)
	}
	if len(p.Credentials) == 0 {
		return errors.New("a provider needs at least one credential")
	}
	for key, c := range p.Credentials {
		switch {
		case !envRE.MatchString(key):
			return fmt.Errorf("credential %q: not an environment variable name", key)
		case reservedEnvRE.MatchString(key):
			return fmt.Errorf("credential %q: brig cannot pass a credential of this name", key)
		case c.SecretTool == nil:
			return fmt.Errorf("credential %s: missing secret_tool", key)
		}
		lookup := c.SecretTool.Lookup
		if len(lookup) < 2 || len(lookup)%2 != 0 || slices.Contains(lookup, "") {
			return fmt.Errorf("credential %s: secret_tool.lookup needs attribute/value pairs", key)
		}
	}
	return nil
}
