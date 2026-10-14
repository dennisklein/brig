// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"
	"maps"

	"github.com/dennisklein/brig/internal/help"
)

// generate makes the reference pages from brig's code: the command tree, the
// exit codes, the config structs, the help texts. repo is the root of the
// source tree, which the config comments and the help files are read from.
func generate(repo string) (files, error) {
	h, err := help.Load()
	if err != nil {
		return nil, err
	}
	out := cliPages(commandTree(), h)
	out[referenceDir+"/exit-codes.md"] = exitCodesPage()
	out[referenceDir+"/glossary.md"] = glossaryPage(h)
	ui, err := uiPage(repo, h)
	if err != nil {
		return nil, fmt.Errorf("brig ui reference: %w", err)
	}
	out[referenceDir+"/ui.md"] = ui
	cfg, err := configPages(repo)
	if err != nil {
		return nil, fmt.Errorf("config reference: %w", err)
	}
	maps.Copy(out, cfg)
	return out, nil
}
