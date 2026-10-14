// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/cpuguy83/go-md2man/v2/md2man"
	"github.com/spf13/cobra"
	"github.com/spf13/cobra/doc"

	"github.com/dennisklein/brig/internal/help"
)

// manDate is the date the man pages carry: SOURCE_DATE_EPOCH if set, so
// that builds are reproducible, else today.
func manDate() (time.Time, error) {
	if epoch := os.Getenv("SOURCE_DATE_EPOCH"); epoch != "" {
		sec, err := strconv.ParseInt(epoch, 10, 64)
		if err != nil {
			return time.Time{}, fmt.Errorf("invalid SOURCE_DATE_EPOCH: %w", err)
		}
		return time.Unix(sec, 0).UTC(), nil
	}
	return time.Now().UTC(), nil
}

// writeMan writes the man pages into dir: brig(1) and brig-COMMAND(1) for
// every command, from the same command tree as the reference, and
// brig-concepts(7) from the glossary.
func writeMan(dir, version string) error {
	date, err := manDate()
	if err != nil {
		return err
	}
	h, err := help.Load()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil { //nolint:gosec // G301: man pages are for everyone to read
		return err
	}
	root := commandTree()
	// The man pages are Markdown for go-md2man, like the reference pages.
	walk(root, func(c *cobra.Command) {
		c.Short = escape(c.Short)
		c.Long = textToMarkdown(c.Long, nil)
		c.DisableAutoGenTag = true
	})
	header := &doc.GenManHeader{Section: "1", Date: &date, Source: "brig " + version, Manual: "brig Manual"}
	if err := doc.GenManTree(root, header, dir); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "brig-concepts.7"), conceptsMan(h, date, version), 0o644) //nolint:gosec // G306: man pages are for everyone to read
}

// conceptsMan writes brig-concepts(7), the concepts of brig help.
func conceptsMan(h *help.Help, date time.Time, version string) []byte {
	var b strings.Builder
	fmt.Fprintf(&b, "%% \"BRIG\\-CONCEPTS\" \"7\" \"%s\" \"brig %s\" \"brig Manual\"\n", date.Format("Jan 2006"), version)
	b.WriteString("# NAME\nbrig\\-concepts \\- the words that brig uses\n\n")
	b.WriteString("# DESCRIPTION\nbrig and brig ui use these words. **brig help** *TOPIC* prints the same texts, and **brig help topics** lists them.\n\n")
	b.WriteString("# CONCEPTS\n")
	for _, t := range h.Topics(help.Concept) {
		fmt.Fprintf(&b, "%s\n:   %s\n\n", escape(t.Title), inline(t.Summary))
		if len(t.Aliases) > 0 {
			fmt.Fprintf(&b, "    Also called: %s.\n\n", escape(strings.Join(t.Aliases, ", ")))
		}
		fmt.Fprintf(&b, "    **brig help %s**\n\n", t.ID)
	}
	b.WriteString("# SEE ALSO\n**brig**(1)\n")
	return md2man.Render([]byte(b.String()))
}
