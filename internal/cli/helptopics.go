// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/dennisklein/brig/internal/help"
)

// addHelpTopics makes brig help TOPIC print brig's concepts and how-tos,
// the same texts that brig ui shows. brig help topics lists them; each
// topic is a hidden command, so that brig --help stays short. A topic named
// like a command, such as sync, is left to that command's own help.
func addHelpTopics(root *cobra.Command) {
	h, err := help.Load()
	if err != nil {
		// The texts are embedded and tested; this cannot happen in a build.
		panic(fmt.Sprintf("brig's help texts: %v", err))
	}
	index := &cobra.Command{
		Use:   "topics",
		Short: "List the concepts and how-tos that brig help explains",
		Long:  h.Index(),
	}
	index.SetHelpFunc(func(c *cobra.Command, _ []string) { fmt.Fprint(c.OutOrStdout(), h.Index()) })
	root.AddCommand(index)
	for _, k := range []help.Kind{help.Concept, help.Task} {
		for _, t := range h.Topics(k) {
			if c, _, err := root.Find([]string{t.ID}); err == nil && c != root {
				continue
			}
			c := &cobra.Command{Use: t.ID, Short: help.Plain(t.Summary), Hidden: true}
			text := h.Text(t)
			c.SetHelpFunc(func(c *cobra.Command, _ []string) { fmt.Fprint(c.OutOrStdout(), text) })
			root.AddCommand(c)
		}
	}
}
