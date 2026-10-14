// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"github.com/dennisklein/brig/internal/cli"
	"github.com/dennisklein/brig/internal/help"
)

const (
	// referenceDir holds the generated reference pages.
	referenceDir = "docs/reference"
	// cliDir holds one page for each command, and the index of them.
	cliDir = referenceDir + "/cli"
)

// commandTree returns brig's command tree as the program runs it, with the
// commands that cobra adds.
func commandTree() *cobra.Command {
	root := cli.Command()
	root.InitDefaultHelpCmd()
	root.InitDefaultCompletionCmd()
	root.InitDefaultHelpFlag()
	root.InitDefaultVersionFlag()
	return root
}

// documented reports whether c has a page: neither hidden, nor a help topic
// that has no command of its own to explain.
func documented(c *cobra.Command) bool {
	return c.IsAvailableCommand() && !c.IsAdditionalHelpTopicCommand()
}

// walk calls f for c and the commands below it, parents first.
func walk(c *cobra.Command, f func(*cobra.Command)) {
	f(c)
	for _, s := range c.Commands() {
		if documented(s) {
			walk(s, f)
		}
	}
}

// pageName is the file name of a command's page, such as
// brig-sandbox-create.md.
func pageName(c *cobra.Command) string {
	return strings.ReplaceAll(c.CommandPath(), " ", "-") + ".md"
}

// cliPages makes the reference page of each command, and their index.
func cliPages(root *cobra.Command, h *help.Help) files {
	known := map[string]*cobra.Command{}
	walk(root, func(c *cobra.Command) { known[c.CommandPath()] = c })
	g := &cliGen{root: root, known: known, help: h}
	out := files{}
	walk(root, func(c *cobra.Command) { out[cliDir+"/"+pageName(c)] = g.page(c) })
	out[cliDir+"/README.md"] = g.index()
	return out
}

type cliGen struct {
	root  *cobra.Command
	known map[string]*cobra.Command
	help  *help.Help
}

// page writes the reference page of c.
func (g *cliGen) page(c *cobra.Command) string {
	var b strings.Builder
	b.WriteString(generatedMark)
	fmt.Fprintf(&b, "\n# %s\n\n%s\n", c.CommandPath(), escape(c.Short))

	b.WriteString("\n## Synopsis\n\n")
	b.WriteString(fence("text", g.synopsis(c)...))
	if aliases := aliasPaths(c); len(aliases) > 0 {
		fmt.Fprintf(&b, "\nAlso: %s.\n", strings.Join(aliases, ", "))
	}

	if strings.TrimSpace(c.Long) != "" {
		b.WriteString("\n## Description\n\n")
		b.WriteString(textToMarkdown(c.Long, func(p string) string { return g.link(p, c) }))
		if c == g.root {
			b.WriteString("\n[Exit codes](../exit-codes.md) says what each status means.\n")
		}
	}

	if c.Example != "" {
		b.WriteString("\n## Examples\n\n")
		b.WriteString(fence("sh", strings.Split(strings.Trim(dedent(c.Example), "\n"), "\n")...))
	}

	if rows := flagRows(c.NonInheritedFlags()); len(rows) > 0 {
		b.WriteString("\n## Options\n\n| Option | Description |\n| --- | --- |\n")
		b.WriteString(strings.Join(rows, "\n") + "\n")
	}
	if c != g.root && flagRows(c.InheritedFlags()) != nil {
		b.WriteString("\nThe [global options](brig.md#options) of brig apply too.\n")
	}

	if subs := g.subcommands(c); len(subs) > 0 {
		b.WriteString("\n## Commands\n\n| Command | Description |\n| --- | --- |\n")
		for _, s := range subs {
			fmt.Fprintf(&b, "| [%s](%s) | %s |\n", code(s.CommandPath()), pageName(s), cell(s.Short))
		}
	}

	b.WriteString("\n## See also\n\n")
	if c.HasParent() {
		fmt.Fprintf(&b, "- [%s](%s)\n", code(c.Parent().CommandPath()), pageName(c.Parent()))
	}
	b.WriteString("- [All commands](README.md)\n")
	return b.String()
}

// subcommands lists the documented commands directly below c.
func (g *cliGen) subcommands(c *cobra.Command) []*cobra.Command {
	var out []*cobra.Command
	for _, s := range c.Commands() {
		if documented(s) {
			out = append(out, s)
		}
	}
	return out
}

// synopsis returns the usage lines of c. A hand-written Use may hold more
// than one, the later ones complete.
func (g *cliGen) synopsis(c *cobra.Command) []string {
	lines := strings.Split(c.Use, "\n")
	for i := range lines {
		lines[i] = strings.TrimSpace(lines[i])
	}
	if c == g.root {
		lines[0] = "brig COMMAND [OPTIONS]"
	} else {
		lines[0] = c.Parent().CommandPath() + " " + lines[0]
		if len(g.subcommands(c)) > 0 && !c.Runnable() {
			lines[0] += " COMMAND"
		}
	}
	return lines
}

// aliasPaths lists c's aliases as commands.
func aliasPaths(c *cobra.Command) []string {
	var out []string
	for _, a := range c.Aliases {
		out = append(out, code(c.Parent().CommandPath()+" "+a))
	}
	return out
}

// flagRows describes the flags for a table; the help flag is left out, since
// every command has one.
func flagRows(fs *pflag.FlagSet) []string {
	var rows []string
	fs.VisitAll(func(f *pflag.Flag) {
		if f.Hidden || f.Name == "help" || f.Deprecated != "" {
			return
		}
		opt := "--" + f.Name
		if f.Shorthand != "" {
			opt = "-" + f.Shorthand + ", " + opt
		}
		if typ := f.Value.Type(); typ != "bool" {
			opt += " " + typ
		}
		desc := cell(f.Usage)
		switch f.DefValue {
		case "", "false", "[]", "0", "0s":
		default:
			desc += fmt.Sprintf(" (default %s)", code(f.DefValue))
		}
		rows = append(rows, fmt.Sprintf("| %s | %s |", code(opt), desc))
	})
	return rows
}

// commandRE finds brig followed by words, the start of a command in prose.
var commandRE = regexp.MustCompile(`\bbrig(?:[ \n]+[a-z][a-z0-9-]*)+`)

// wordRE finds the words of such a match.
var wordRE = regexp.MustCompile(`[a-z][a-z0-9-]*`)

// link links the commands and help topics that paragraph p of c's text
// names, such as brig sync or brig help fence-2, to their pages.
func (g *cliGen) link(p string, self *cobra.Command) string {
	return commandRE.ReplaceAllStringFunc(p, func(m string) string {
		words := wordRE.FindAllStringIndex(m, -1)
		word := func(i int) string { return m[words[i][0]:words[i][1]] }
		if len(words) >= 3 && word(1) == "help" {
			if t, ok := g.help.Topic(word(2)); ok {
				text := strings.Join([]string{word(0), word(1), word(2)}, " ")
				return fmt.Sprintf("[%s](%s)%s", code(text), "../glossary.md#"+t.ID, m[words[2][1]:])
			}
		}
		for n := len(words); n >= 2; n-- {
			path := make([]string, n)
			for i := range path {
				path[i] = word(i)
			}
			target, ok := g.known[strings.Join(path, " ")]
			if !ok {
				continue
			}
			if target == self {
				return m
			}
			return fmt.Sprintf("[%s](%s)%s", code(target.CommandPath()), pageName(target), m[words[n-1][1]:])
		}
		return m
	})
}

// index lists the commands by group, with what each one does.
func (g *cliGen) index() string {
	var b strings.Builder
	b.WriteString(generatedMark)
	b.WriteString("\n# Commands\n\n")
	b.WriteString("Every command of brig, as [`brig`](brig.md) lists them. Run `brig COMMAND --help` for the same text in a terminal, or `man brig-COMMAND`.\n")
	groups := append([]*cobra.Group{}, g.root.Groups()...)
	groups = append(groups, &cobra.Group{ID: "", Title: "Other commands:"})
	for _, grp := range groups {
		var rows []string
		for _, c := range g.subcommands(g.root) {
			if c.GroupID != grp.ID {
				continue
			}
			walk(c, func(c *cobra.Command) {
				rows = append(rows, fmt.Sprintf("| [%s](%s) | %s |", code(c.CommandPath()), pageName(c), cell(c.Short)))
			})
		}
		if len(rows) == 0 {
			continue
		}
		fmt.Fprintf(&b, "\n## %s\n\n| Command | Description |\n| --- | --- |\n%s\n", strings.TrimSuffix(grp.Title, ":"), strings.Join(rows, "\n"))
	}
	b.WriteString("\nThe concepts and how-tos that `brig help TOPIC` explains are in the [glossary](../glossary.md).\n")
	return b.String()
}
