// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

// Package version reports the version of the running brig binary.
package version

import "runtime/debug"

// version is injected at release build time via
// -ldflags "-X github.com/dennisklein/brig/internal/version.version=<v>".
var version string

// Version returns the release version, falling back to the module version
// recorded by `go install` and finally to "devel" for local builds.
func Version() string {
	if version != "" {
		return version
	}
	if bi, ok := debug.ReadBuildInfo(); ok && bi.Main.Version != "" && bi.Main.Version != "(devel)" {
		return bi.Main.Version
	}
	return "devel"
}
