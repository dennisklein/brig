// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

package openshell

import (
	"fmt"
	"regexp"
)

// DefaultWorkspace is the workspace that every gateway has.
const DefaultWorkspace = "default"

// workspaceRE matches the names OpenShell accepts for workspaces: DNS-1123
// labels of at most 19 characters.
var workspaceRE = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]{0,17}[a-z0-9])?$`)

// CheckWorkspace reports whether name is a workspace name OpenShell accepts.
func CheckWorkspace(name string) error {
	if !workspaceRE.MatchString(name) {
		return fmt.Errorf("invalid workspace name %q: use up to 19 lowercase letters, digits and -, starting and ending with a letter or digit", name)
	}
	return nil
}
