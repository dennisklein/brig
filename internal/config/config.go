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
	"sort"

	"go.yaml.in/yaml/v3"

	"github.com/dennisklein/brig/internal/bytesize"
)

// Config is the merged configuration: built-in defaults overlaid with the
// user's config file.
type Config struct {
	Defaults        Defaults                  `yaml:"defaults"`
	NetworkProfiles map[string]NetworkProfile `yaml:"network_profiles"`
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
	// and the networks of the host's own addresses when the VM starts.
	LAN bool `yaml:"lan" json:"lan"`
	// IPv6 allows IPv6 traffic; otherwise the VM gets no IPv6 connectivity.
	IPv6 bool `yaml:"ipv6" json:"ipv6"`
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
	if err := dec.Decode(&file); err != nil && !errors.Is(err, io.EOF) {
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
		return NetworkProfile{}, fmt.Errorf("unknown network profile %q", name)
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
