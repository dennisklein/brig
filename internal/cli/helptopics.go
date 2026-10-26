// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"fmt"

	"github.com/GSI-HPC/go-clikit/cobratree"

	"github.com/dennisklein/brig/internal/help"
)

// helpTopics makes brig help TOPIC print brig's concepts and how-tos, the
// same texts that brig ui shows. brig help topics lists them; they are no
// commands, so that brig --help stays short. A topic named like a command,
// such as sync, is left to that command's own help.
func helpTopics() cobratree.Option {
	h, err := help.Load()
	if err != nil {
		// The texts are embedded and tested; this cannot happen in a build.
		panic(fmt.Sprintf("brig's help texts: %v", err))
	}
	var ids []string
	for _, k := range []help.Kind{help.Concept, help.Task} {
		for _, t := range h.Topics(k) {
			ids = append(ids, t.ID)
		}
	}
	lookup := func(name string) (string, bool) {
		if name == "topics" {
			return h.Index(), true
		}
		t, ok := h.Topic(name)
		if !ok {
			return "", false
		}
		return h.Text(t), true
	}
	return cobratree.Topics(lookup, func() []string { return append([]string{"topics"}, ids...) })
}
