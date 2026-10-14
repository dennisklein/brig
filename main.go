// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

// Command brig manages the lifecycle of agent-sandbox VMs that run NVIDIA
// OpenShell on a Fedora host.
package main

//go:generate go run ./internal/docgen

import (
	"os"

	"github.com/dennisklein/brig/internal/cli"
)

func main() {
	os.Exit(cli.Execute())
}
