// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

package vm

import (
	"fmt"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// ParseMount parses a --mount argument of the form
//
//	SOURCE[:TARGET][:OPTIONS]
//
// SOURCE is a host directory, made absolute relative to the working
// directory. TARGET is an absolute directory in the VM and defaults to
// /mnt/<basename of SOURCE>. OPTIONS is a comma-separated list of
//
//	ro       read-only (default)
//	rw       read-write
//	sandbox  also offer it to OpenShell sandboxes as a volume (implies ro)
func ParseMount(arg string) (Mount, error) {
	parts := strings.Split(arg, ":")
	if len(parts) > 3 || parts[0] == "" {
		return Mount{}, fmt.Errorf("invalid mount %q: want SOURCE[:TARGET][:OPTIONS]", arg)
	}
	src, err := filepath.Abs(parts[0])
	if err != nil {
		return Mount{}, err
	}
	// The source reaches libvirt as XML and the VM record as JSON, and both
	// replace what they cannot encode, so virtiofsd would share another path.
	if !utf8.ValidString(src) || strings.ContainsFunc(src, func(r rune) bool {
		return unicode.IsControl(r) || r == '￾' || r == '￿'
	}) {
		return Mount{}, fmt.Errorf("invalid mount %q: source must be valid UTF-8 without control characters", arg)
	}
	m := Mount{Source: src, ReadOnly: true}
	var opts string
	switch len(parts) {
	case 2:
		// SOURCE:TARGET or SOURCE:OPTIONS
		if strings.HasPrefix(parts[1], "/") {
			m.Target = parts[1]
		} else {
			opts = parts[1]
		}
	case 3:
		m.Target, opts = parts[1], parts[2]
	}
	if m.Target == "" {
		m.Target = path.Join("/mnt", filepath.Base(src))
	}
	if !path.IsAbs(m.Target) || path.Clean(m.Target) != m.Target || m.Target == "/" {
		return Mount{}, fmt.Errorf("invalid mount %q: target %q must be a clean absolute path other than /", arg, m.Target)
	}
	if strings.ContainsAny(m.Target, " \t\n\\") {
		return Mount{}, fmt.Errorf("invalid mount %q: target must not contain whitespace or backslashes", arg)
	}
	// A comma means options in the wrong place, as in /src:/work,sandbox.
	if strings.Contains(m.Target, ",") {
		return Mount{}, fmt.Errorf("invalid mount %q: target must not contain commas; options follow a colon, as in SOURCE:TARGET:ro,sandbox", arg)
	}
	for _, o := range strings.Split(opts, ",") {
		switch o {
		case "":
		case "ro":
			m.ReadOnly = true
		case "rw":
			m.ReadOnly = false
		case "sandbox":
			m.Sandbox = true
		default:
			return Mount{}, fmt.Errorf("invalid mount %q: unknown option %q (want ro, rw or sandbox)", arg, o)
		}
	}
	if m.Sandbox && !m.ReadOnly {
		return Mount{}, fmt.Errorf("invalid mount %q: sandbox mounts are read-only", arg)
	}
	return m, nil
}

// String formats the mount in ParseMount's syntax.
func (m Mount) String() string {
	opts := "rw"
	if m.ReadOnly {
		opts = "ro"
	}
	if m.Sandbox {
		opts += ",sandbox"
	}
	return m.Source + ":" + m.Target + ":" + opts
}

// AssignMountTags gives every mount of the VM that has no virtiofs tag a new
// one: brig0, brig1 and so on. Tags are never reused, not even those of
// removed mounts, so that a sandbox volume, which is named after its mount's
// tag, never comes to stand for another directory.
func (v *VM) AssignMountTags() {
	for _, m := range v.Mounts {
		if n, err := strconv.Atoi(strings.TrimPrefix(m.Tag, "brig")); err == nil && n >= v.NextMountTag {
			v.NextMountTag = n + 1
		}
	}
	for i := range v.Mounts {
		if v.Mounts[i].Tag == "" {
			v.Mounts[i].Tag = "brig" + strconv.Itoa(v.NextMountTag)
			v.NextMountTag++
		}
	}
}
