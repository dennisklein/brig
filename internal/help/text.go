// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

package help

import (
	"fmt"
	"strings"
)

// width is the width that Text wraps lines at.
const width = 79

// Text writes a topic for a terminal, as brig help ID prints it.
func (h *Help) Text(t *Topic) string {
	var b strings.Builder
	b.WriteString(t.Title + "\n")
	if t.Summary != "" {
		b.WriteString("\n")
		wrap(&b, Plain(t.Summary), "  ", "  ")
	}
	for _, bl := range t.Body {
		b.WriteString("\n")
		if bl.Text != "" {
			wrap(&b, Plain(bl.Text), "  ", "  ")
		}
		for i, s := range bl.Steps {
			n := fmt.Sprintf("  %d. ", i+1)
			wrap(&b, Plain(s), n, strings.Repeat(" ", len(n)))
		}
	}
	if len(t.Commands) > 0 {
		b.WriteString("\nIn a terminal:\n")
		for _, c := range t.Commands {
			b.WriteString("  " + c + "\n")
		}
	}
	if t.UI != "" {
		fmt.Fprintf(&b, "\nIn brig ui: %s\n", t.UI)
	}
	if t.Doc != "" {
		fmt.Fprintf(&b, "\nMore in brig's docs:\n  %s\n", t.DocURL())
	}
	if see := append(append([]string(nil), t.Concepts...), t.See...); len(see) > 0 {
		b.WriteString("\nSee also:\n")
		for _, id := range see {
			title := id
			if s, ok := h.topics[id]; ok {
				title = s.Title
			}
			fmt.Fprintf(&b, "  brig help %-18s %s\n", id, title)
		}
	}
	return b.String()
}

// Index lists the concepts and tasks for a terminal, as brig help topics
// prints it.
func (h *Help) Index() string {
	var b strings.Builder
	for _, sec := range []struct {
		k     Kind
		title string
	}{{Task, "How to"}, {Concept, "Concepts"}} {
		b.WriteString(sec.title + ":\n")
		for _, t := range h.Topics(sec.k) {
			fmt.Fprintf(&b, "  %-20s %s\n", t.ID, t.Title)
		}
		b.WriteString("\n")
	}
	b.WriteString("Run brig help TOPIC for one of them, such as brig help fence-2.\n")
	return b.String()
}

// Plain removes the Markdown marks for code from s.
func Plain(s string) string { return strings.ReplaceAll(s, "`", "") }

// wrap writes s wrapped at width, its first line indented by first and the
// others by rest.
func wrap(b *strings.Builder, s, first, rest string) {
	line := first
	empty := true
	for _, w := range strings.Fields(s) {
		if !empty && len(line)+1+len(w) > width {
			b.WriteString(line + "\n")
			line, empty = rest, true
		}
		if !empty {
			line += " "
		}
		line += w
		empty = false
	}
	b.WriteString(line + "\n")
}
