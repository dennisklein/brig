// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

// Package paths resolves where brig keeps its configuration, data and caches,
// following the XDG Base Directory Specification.
package paths

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Dirs holds brig's base directories.
type Dirs struct {
	// Config holds user configuration (config.yaml).
	Config string
	// Data holds base images and VM state, including private keys.
	Data string
	// Cache holds re-downloadable data such as the mkosi package cache.
	Cache string
}

// Default returns the XDG base directories for the current user.
func Default() (Dirs, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return Dirs{}, err
	}
	dir := func(env, fallback string) (string, error) {
		if v := os.Getenv(env); v != "" {
			if !filepath.IsAbs(v) {
				return "", errors.New(env + " must be an absolute path")
			}
			return filepath.Join(v, "brig"), nil
		}
		return filepath.Join(home, fallback, "brig"), nil
	}
	var d Dirs
	if d.Config, err = dir("XDG_CONFIG_HOME", ".config"); err != nil {
		return Dirs{}, err
	}
	if d.Data, err = dir("XDG_DATA_HOME", filepath.Join(".local", "share")); err != nil {
		return Dirs{}, err
	}
	// ssh expands % and ${NAME} in the key and known_hosts paths. It has no
	// escape for ${, and -i is checked before it is expanded, so % cannot be
	// escaped either. Such a directory could not hold a VM's files.
	if strings.ContainsRune(d.Data, '%') || strings.Contains(d.Data, "${") {
		return Dirs{}, fmt.Errorf("data directory %q must not contain %% or ${: ssh expands them", d.Data)
	}
	if d.Cache, err = dir("XDG_CACHE_HOME", ".cache"); err != nil {
		return Dirs{}, err
	}
	return d, nil
}

// ConfigFile is the user configuration file.
func (d Dirs) ConfigFile() string { return filepath.Join(d.Config, "config.yaml") }

// ImagesDir holds one directory per base image.
func (d Dirs) ImagesDir() string { return filepath.Join(d.Data, "images") }

// VMsDir holds one directory per VM.
func (d Dirs) VMsDir() string { return filepath.Join(d.Data, "vms") }

// MkosiCacheDir holds mkosi's package cache.
func (d Dirs) MkosiCacheDir() string { return filepath.Join(d.Cache, "mkosi") }

// EnsurePrivate creates dir and its parents with mode 0700 and tightens the
// mode of dir itself if it already exists.
func EnsurePrivate(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	return os.Chmod(dir, 0o700) //nolint:gosec // G302 targets files; directories need the execute bit
}
