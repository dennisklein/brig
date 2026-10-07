// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

// Command brig manages the lifecycle of agent-sandbox VMs that run NVIDIA
// OpenShell on a Fedora host.
package main

import (
	"os"

	"github.com/dennisklein/brig/internal/cli"
)

func main() {
	os.Exit(cli.Execute())
}
