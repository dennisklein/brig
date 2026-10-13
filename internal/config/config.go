// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

// Package config loads brig's user configuration: VM defaults and the named
// network profiles that restrict what a VM may reach.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"

	"github.com/dennisklein/brig/internal/bytesize"
)

// Config is the merged configuration: built-in defaults overlaid with the
// user's config file.
type Config struct {
	Defaults        Defaults                  `yaml:"defaults"`
	NetworkProfiles map[string]NetworkProfile `yaml:"network_profiles"`
	OpenShell       OpenShell                 `yaml:"openshell"`
	Audit           Audit                     `yaml:"audit"`
}

// Audit configures the audit trail that brig keeps of each VM.
type Audit struct {
	// Journal says what the collector mirrors to the user journal:
	// changes (denials, policy and draft changes, sandboxes and gaps), all
	// (allowed requests too) or off.
	Journal string `yaml:"journal"`
	// Retention deletes the days of history older than it; zero keeps
	// everything.
	Retention Duration `yaml:"retention"`
}

// Duration is a length of time as config.yaml writes it: a number of days
// (90d) or weeks (12w), or what time.ParseDuration takes (36h).
type Duration time.Duration

// UnmarshalYAML reads a duration.
func (d *Duration) UnmarshalYAML(n *yaml.Node) error {
	v, err := ParseDuration(n.Value)
	if err != nil {
		return err
	}
	*d = Duration(v)
	return nil
}

// ParseDuration reads a duration as Duration writes it.
func ParseDuration(s string) (time.Duration, error) {
	s = strings.TrimSpace(s)
	for suffix, unit := range map[string]time.Duration{"d": 24 * time.Hour, "w": 7 * 24 * time.Hour} {
		if n, ok := strings.CutSuffix(s, suffix); ok {
			days, err := strconv.Atoi(n)
			if err != nil || days < 0 {
				return 0, fmt.Errorf("invalid duration %q", s)
			}
			return time.Duration(days) * unit, nil
		}
	}
	v, err := time.ParseDuration(s)
	if err != nil || v < 0 {
		return 0, fmt.Errorf("invalid duration %q (want e.g. 90d, 12w or 36h)", s)
	}
	return v, nil
}

// OpenShell configures how brig applies OpenShell config directories to the
// VMs' gateways.
type OpenShell struct {
	// SecretTool is the program brig runs as `SECRET_TOOL lookup ATTRIBUTE
	// VALUE...` to read provider secrets: a name to look up in PATH or an
	// absolute path. Config directories cannot set it, so that one shared
	// with others cannot run programs on the user's host.
	SecretTool string `yaml:"secret_tool"`
	// Configs are the config directories of new VMs.
	Configs []string `yaml:"configs"`
}

// Defaults are used for settings a command does not set explicitly.
type Defaults struct {
	CPUs           int           `yaml:"cpus"`
	Memory         bytesize.Size `yaml:"memory"`
	RootDisk       bytesize.Size `yaml:"root_disk"`
	DataDisk       bytesize.Size `yaml:"data_disk"`
	NetworkProfile string        `yaml:"network_profile"`
	FedoraRelease  int           `yaml:"fedora_release"`
}

// NetworkProfile restricts the network destinations a VM can reach. All
// destinations are denied unless allowed. brig enforces profiles outside the
// VM, so nothing in the VM can lift them.
type NetworkProfile struct {
	// Internet allows destinations outside the host, LAN and special-purpose
	// address ranges.
	Internet bool `yaml:"internet" json:"internet"`
	// Host allows all services on the host's loopback interface. Without
	// it, the host's own addresses are blocked too.
	Host bool `yaml:"host" json:"host"`
	// HostPorts allows these TCP ports on the host's loopback interface even
	// when Host is false, e.g. a local model server.
	HostPorts []uint16 `yaml:"host_ports,omitempty" json:"host_ports,omitempty"`
	// LAN allows private, shared, link-local and multicast address ranges,
	// and the networks of the host's own addresses and routes other than
	// default ones, such as a VPN's intranet, when the VM starts.
	LAN bool `yaml:"lan" json:"lan"`
	// IPv6 allows IPv6 traffic; otherwise the VM gets no IPv6 connectivity.
	IPv6 bool `yaml:"ipv6" json:"ipv6"`
}

// Allows describes what the profile lets a VM reach, such as "internet, host
// ports 11434", for people who want to know what a profile's name means.
func (p NetworkProfile) Allows() string {
	var parts []string
	if p.Internet {
		parts = append(parts, "internet")
	}
	if p.LAN {
		parts = append(parts, "LAN")
	}
	if p.Host {
		parts = append(parts, "all host services")
	} else if len(p.HostPorts) > 0 {
		ports := make([]string, len(p.HostPorts))
		for i, port := range p.HostPorts {
			ports[i] = strconv.Itoa(int(port))
		}
		parts = append(parts, "host ports "+strings.Join(ports, ", "))
	}
	if p.IPv6 {
		parts = append(parts, "IPv6")
	}
	if len(parts) == 0 {
		return "nothing outside the VM"
	}
	return strings.Join(parts, ", ")
}

// Builtin returns the configuration used when no config file exists.
func Builtin() *Config {
	return &Config{
		Defaults: Defaults{
			CPUs:           4,
			Memory:         8 * bytesize.GiB,
			RootDisk:       20 * bytesize.GiB,
			DataDisk:       40 * bytesize.GiB,
			NetworkProfile: "default",
			FedoraRelease:  44,
		},
		NetworkProfiles: map[string]NetworkProfile{
			"default":  {Internet: true},
			"open":     {Internet: true, Host: true, LAN: true, IPv6: true},
			"isolated": {},
		},
		OpenShell: OpenShell{SecretTool: "secret-tool"},
		Audit:     Audit{Journal: "changes"},
	}
}

// Load reads the config file at path and overlays it on Builtin. A missing
// file yields the built-in configuration. Unknown keys are errors.
//
// Non-zero defaults in the file replace built-in ones; a network profile in
// the file replaces a built-in profile of the same name as a whole.
func Load(path string) (*Config, error) {
	cfg := Builtin()
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return cfg, nil
	}
	if err != nil {
		return nil, err
	}
	var file Config
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	err = dec.Decode(&file)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if err == nil {
		// Only the first document would be applied, so a second one
		// (even an empty one after a trailing "---") must not be ignored.
		var extra yaml.Node
		switch err := dec.Decode(&extra); {
		case errors.Is(err, io.EOF):
		case err != nil:
			return nil, fmt.Errorf("%s: %w", path, err)
		default:
			return nil, fmt.Errorf("%s: more than one YAML document", path)
		}
	}
	if err := file.OpenShell.expandHome(); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	cfg.overlay(&file)
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return cfg, nil
}

func (c *Config) overlay(o *Config) {
	d, od := &c.Defaults, o.Defaults
	if od.CPUs != 0 {
		d.CPUs = od.CPUs
	}
	if od.Memory != 0 {
		d.Memory = od.Memory
	}
	if od.RootDisk != 0 {
		d.RootDisk = od.RootDisk
	}
	if od.DataDisk != 0 {
		d.DataDisk = od.DataDisk
	}
	if od.NetworkProfile != "" {
		d.NetworkProfile = od.NetworkProfile
	}
	if od.FedoraRelease != 0 {
		d.FedoraRelease = od.FedoraRelease
	}
	for name, p := range o.NetworkProfiles {
		c.NetworkProfiles[name] = p
	}
	if o.OpenShell.SecretTool != "" {
		c.OpenShell.SecretTool = o.OpenShell.SecretTool
	}
	if o.OpenShell.Configs != nil {
		c.OpenShell.Configs = o.OpenShell.Configs
	}
	if o.Audit.Journal != "" {
		c.Audit.Journal = o.Audit.Journal
	}
	if o.Audit.Retention != 0 {
		c.Audit.Retention = o.Audit.Retention
	}
}

// expandHome replaces a leading "~/" in the paths with the home directory.
func (o *OpenShell) expandHome() error {
	expand := func(p *string) error {
		rest, ok := strings.CutPrefix(*p, "~/")
		if !ok {
			return nil
		}
		home, err := os.UserHomeDir()
		if err != nil {
			return err
		}
		*p = filepath.Join(home, rest)
		return nil
	}
	if err := expand(&o.SecretTool); err != nil {
		return err
	}
	for i := range o.Configs {
		if err := expand(&o.Configs[i]); err != nil {
			return err
		}
	}
	return nil
}

// The least resources of a VM, for the defaults and for each VM.
const (
	MinCPUs     = 1
	MinMemory   = 512 * bytesize.MiB
	MinRootDisk = 8 * bytesize.GiB
	MinDataDisk = 1 * bytesize.GiB
)

// Validate checks the configuration for values brig cannot use.
func (c *Config) Validate() error {
	d := c.Defaults
	switch {
	case d.CPUs < MinCPUs:
		return fmt.Errorf("defaults.cpus must be at least %d", MinCPUs)
	case d.Memory < MinMemory:
		return fmt.Errorf("defaults.memory must be at least %s", MinMemory)
	case d.Memory%bytesize.MiB != 0:
		return errors.New("defaults.memory must be a whole number of MiB")
	case d.RootDisk < MinRootDisk:
		return fmt.Errorf("defaults.root_disk must be at least %s", MinRootDisk)
	case d.DataDisk < MinDataDisk:
		return fmt.Errorf("defaults.data_disk must be at least %s", MinDataDisk)
	case d.FedoraRelease < 1:
		return errors.New("defaults.fedora_release must be a Fedora release number")
	}
	if _, err := c.Profile(d.NetworkProfile); err != nil {
		return fmt.Errorf("defaults.network_profile: %w", err)
	}
	for _, name := range c.ProfileNames() {
		if err := c.NetworkProfiles[name].validate(); err != nil {
			return fmt.Errorf("network_profiles.%s: %w", name, err)
		}
	}
	if t := c.OpenShell.SecretTool; t == "" || (strings.Contains(t, "/") && !filepath.IsAbs(t)) {
		return errors.New("openshell.secret_tool must be a program name or an absolute path")
	}
	for _, dir := range c.OpenShell.Configs {
		if !filepath.IsAbs(dir) {
			return fmt.Errorf("openshell.configs: %q is not an absolute path", dir)
		}
	}
	switch c.Audit.Journal {
	case "changes", "all", "off":
	default:
		return fmt.Errorf("audit.journal must be changes, all or off, not %q", c.Audit.Journal)
	}
	if r := time.Duration(c.Audit.Retention); r != 0 && r < 24*time.Hour {
		return errors.New("audit.retention must be at least a day")
	}
	return nil
}

func (p NetworkProfile) validate() error {
	for _, port := range p.HostPorts {
		if port == 0 {
			return errors.New("host_ports must not contain 0")
		}
	}
	return nil
}

// Profile returns the named network profile.
func (c *Config) Profile(name string) (NetworkProfile, error) {
	p, ok := c.NetworkProfiles[name]
	if !ok {
		return NetworkProfile{}, fmt.Errorf("unknown network profile %q (have %s)", name, strings.Join(c.ProfileNames(), ", "))
	}
	return p, nil
}

// ProfileNames returns the network profile names in sorted order.
func (c *Config) ProfileNames() []string {
	names := make([]string, 0, len(c.NetworkProfiles))
	for name := range c.NetworkProfiles {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}
