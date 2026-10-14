// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"go.yaml.in/yaml/v3"

	"github.com/dennisklein/brig/internal/help"
)

// topicLinks lists the topics as links to their glossary entries.
func topicLinks(h *help.Help, ids []string, prefix string) string {
	links := make([]string, 0, len(ids))
	for _, id := range ids {
		title := id
		if t, ok := h.Topic(id); ok {
			title = t.Title
		}
		links = append(links, fmt.Sprintf("[%s](%s#%s)", escape(title), prefix, id))
	}
	return strings.Join(links, ", ")
}

// glossaryPage lists every concept with its summary, and every how-to, as
// brig help prints them. A topic's anchor is its ID, so brig help fence-2 and
// glossary.md#fence-2 match.
func glossaryPage(h *help.Help) string {
	var b strings.Builder
	b.WriteString(generatedMark)
	b.WriteString("\n# Glossary\n\n")
	b.WriteString("The words that brig and `brig ui` use. `brig help TOPIC` prints the same texts in a terminal, and `brig ui` shows them as hints and in its help pane; `brig help topics` lists them.\n")

	b.WriteString("\n## Concepts\n")
	for _, t := range h.Topics(help.Concept) {
		fmt.Fprintf(&b, "\n### %s<a id=\"%s\"></a>\n\n%s\n", escape(t.Title), t.ID, inline(t.Summary))
		if len(t.Aliases) > 0 {
			fmt.Fprintf(&b, "\nAlso called: %s.\n", strings.Join(t.Aliases, ", "))
		}
		if len(t.See) > 0 {
			fmt.Fprintf(&b, "\nSee also: %s.\n", topicLinks(h, t.See, ""))
		}
		if len(t.Commands) > 0 {
			b.WriteString("\n" + fence("sh", t.Commands...))
		}
		if t.UI != "" {
			fmt.Fprintf(&b, "\nIn `brig ui`: %s.\n", code(t.UI))
		}
		if t.Doc != "" {
			fmt.Fprintf(&b, "\nThe whole story: %s.\n", docLink(t.Doc))
		}
		fmt.Fprintf(&b, "\nIn a terminal: %s.\n", code("brig help "+t.ID))
	}

	b.WriteString("\n## How-tos\n\n| How to | In a terminal | In the docs |\n| --- | --- | --- |\n")
	for _, t := range h.Topics(help.Task) {
		page := ""
		if t.Doc != "" {
			page = docLink(t.Doc)
		}
		fmt.Fprintf(&b, "| <a id=\"%s\"></a>%s | %s | %s |\n", t.ID, cell(t.Title), code("brig help "+t.ID), page)
	}
	return b.String()
}

// docLink links the page of the docs that a help topic names, a path under
// docs/ with an optional #heading, from a page of docs/reference.
func docLink(doc string) string {
	page, _, _ := strings.Cut(doc, "#")
	return fmt.Sprintf("[%s](../%s)", code(page), doc)
}

// yamlKeys returns the keys of the YAML mapping in file, in the file's order.
func yamlKeys(repo, file string) ([]string, error) {
	data, err := os.ReadFile(filepath.Join(repo, filepath.FromSlash(file)))
	if err != nil {
		return nil, err
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("%s: %w", file, err)
	}
	if len(doc.Content) != 1 || doc.Content[0].Kind != yaml.MappingNode {
		return nil, fmt.Errorf("%s is no mapping", file)
	}
	var keys []string
	for i := 0; i < len(doc.Content[0].Content); i += 2 {
		keys = append(keys, doc.Content[0].Content[i].Value)
	}
	return keys, nil
}

// uiPage describes every page and action of brig ui, in the order of the
// files that hold their help.
func uiPage(repo string, h *help.Help) (string, error) {
	pages, err := yamlKeys(repo, "internal/help/pages.yaml")
	if err != nil {
		return "", err
	}
	actions, err := yamlKeys(repo, "internal/help/actions.yaml")
	if err != nil {
		return "", err
	}
	if !slices.Equal(slices.Sorted(slices.Values(pages)), h.PageIDs()) || !slices.Equal(slices.Sorted(slices.Values(actions)), h.ActionIDs()) {
		return "", fmt.Errorf("internal/help: pages.yaml and actions.yaml disagree with the loaded help")
	}
	list, err := loadShots(repo)
	if err != nil {
		return "", err
	}
	for _, s := range list {
		switch {
		case s.Page != "" && !slices.Contains(pages, s.Page):
			return "", fmt.Errorf("%s: shot %s is of the page %q, which brig ui has no help for", shotsFile, s.Name, s.Page)
		case s.Action != "" && !slices.Contains(actions, s.Action):
			return "", fmt.Errorf("%s: shot %s is of the action %q, which brig ui has no help for", shotsFile, s.Name, s.Action)
		}
	}
	byTitle := map[string]string{}
	for _, id := range actions {
		a, _ := h.Action(id)
		byTitle[a.Title] = id
	}

	var b strings.Builder
	b.WriteString(generatedMark)
	b.WriteString("\n# brig ui pages and actions\n\n")
	b.WriteString("What each page of `brig ui` shows and what each of its actions does. The help pane of `brig ui` (press `?`) shows the same texts. ")
	b.WriteString("Every action runs a command that its dialog shows first, and Activity keeps afterwards.\n")

	b.WriteString("\n## Pages\n")
	for _, id := range pages {
		p, _ := h.Page(id)
		fmt.Fprintf(&b, "\n### %s<a id=\"page-%s\"></a>\n\n%s\n", escape(p.Title), id, inline(p.Summary))
		for _, s := range shotsOf(list, func(s shot) string { return s.Page }, id) {
			b.WriteString("\n" + s.picture(referenceDir))
		}
		if len(p.Concepts) > 0 {
			fmt.Fprintf(&b, "\nWords: %s.\n", topicLinks(h, p.Concepts, "glossary.md"))
		}
		if len(p.Tasks) > 0 {
			fmt.Fprintf(&b, "\nHow to: %s.\n", topicLinks(h, p.Tasks, "glossary.md"))
		}
		for _, tab := range slices.Sorted(maps.Keys(p.Tabs)) {
			fmt.Fprintf(&b, "\nTab %s: %s.\n", code(tab), topicLinks(h, p.Tabs[tab], "glossary.md"))
		}
	}

	b.WriteString("\n## Actions\n")
	for _, id := range actions {
		a, _ := h.Action(id)
		fmt.Fprintf(&b, "\n### %s<a id=\"action-%s\"></a>\n\n%s\n\nRuns: %s\n", escape(a.Title), id, inline(a.Summary), code(a.Command))
		for _, s := range shotsOf(list, func(s shot) string { return s.Action }, id) {
			b.WriteString("\n" + s.picture(referenceDir))
		}
		switch {
		case a.Danger != "":
			fmt.Fprintf(&b, "\n**%s**\n", escape(a.Danger))
		case a.Undo != "":
			undo := escape(a.Undo)
			if target, ok := byTitle[a.Undo]; ok {
				undo = fmt.Sprintf("[%s](#action-%s)", undo, target)
			}
			fmt.Fprintf(&b, "\nUndo: %s.\n", undo)
		}
		if len(a.Concepts) > 0 {
			fmt.Fprintf(&b, "\nWords: %s.\n", topicLinks(h, a.Concepts, "glossary.md"))
		}
	}
	return b.String(), nil
}
