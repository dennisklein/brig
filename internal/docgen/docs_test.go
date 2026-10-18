// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"github.com/dennisklein/brig/internal/help"
)

// mdFile is a Markdown file, read for what the checks need of it.
type mdFile struct {
	path    string // relative to the source tree, with forward slashes
	anchors map[string]bool
	links   []mdLink
	blocks  []codeBlock
	text    string // the text outside code blocks
}

type mdLink struct {
	dest string
	line int
}

type codeBlock struct {
	info  string
	lines []string
	line  int // of the first line of code
}

var (
	fenceRE      = regexp.MustCompile("^ {0,3}(```+|~~~+)\\s*([^`\\s]*)")
	atxRE        = regexp.MustCompile(`^ {0,3}#{1,6}\s+(.*?)(?:\s+#+)?\s*$`)
	codeSpanRE   = regexp.MustCompile("`[^`]*`")
	inlineLinkRE = regexp.MustCompile(`\]\(\s*(<[^>]*>|[^)\s]*)(?:\s+(?:"[^"]*"|'[^']*'))?\s*\)`)
	refLinkRE    = regexp.MustCompile(`^ {0,3}\[[^\]]+\]:\s*(\S+)`)
	anchorTagRE  = regexp.MustCompile(`<a\s+(?:id|name)="([^"]+)"`)
	htmlSrcRE    = regexp.MustCompile(`<(?:img|source)\s[^>]*?\b(?:src|srcset)="([^"]+)"`)
)

// parseMarkdown reads the file at rel, below root.
func parseMarkdown(root, rel string) (*mdFile, error) {
	data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
	if err != nil {
		return nil, err
	}
	f := &mdFile{path: rel, anchors: map[string]bool{}}
	var text strings.Builder
	seen := map[string]int{}
	var cur *codeBlock
	var fenceMark string
	for i, line := range strings.Split(string(data), "\n") {
		if m := fenceRE.FindStringSubmatch(line); m != nil {
			switch {
			case cur == nil:
				cur, fenceMark = &codeBlock{info: m[2], line: i + 2}, m[1][:1]
			case strings.HasPrefix(m[1], fenceMark) && strings.TrimSpace(line[strings.Index(line, m[1])+len(m[1]):]) == "":
				f.blocks = append(f.blocks, *cur)
				cur = nil
			default:
				cur.lines = append(cur.lines, line)
			}
			continue
		}
		if cur != nil {
			cur.lines = append(cur.lines, line)
			continue
		}
		text.WriteString(line + "\n")
		if m := atxRE.FindStringSubmatch(line); m != nil {
			s := slug(headingText(m[1]))
			if n := seen[s]; n > 0 {
				f.anchors[s+"-"+itoa(n)] = true
			} else {
				f.anchors[s] = true
			}
			seen[s]++
		}
		for _, m := range anchorTagRE.FindAllStringSubmatch(line, -1) {
			f.anchors[m[1]] = true
		}
		prose := codeSpanRE.ReplaceAllString(line, "")
		for _, m := range inlineLinkRE.FindAllStringSubmatch(prose, -1) {
			f.links = append(f.links, mdLink{strings.Trim(m[1], "<>"), i + 1})
		}
		if m := refLinkRE.FindStringSubmatch(prose); m != nil {
			f.links = append(f.links, mdLink{strings.Trim(m[1], "<>"), i + 1})
		}
		for _, m := range htmlSrcRE.FindAllStringSubmatch(prose, -1) {
			f.links = append(f.links, mdLink{m[1], i + 1})
		}
	}
	f.text = text.String()
	return f, nil
}

// markdownFiles reads the README and the Markdown files below docs/, those
// of the generated reference only if generated says so.
func markdownFiles(t *testing.T, root string, generated bool) []*mdFile {
	t.Helper()
	var out []*mdFile
	add := func(rel string) {
		f, err := parseMarkdown(root, rel)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, f)
	}
	add("README.md")
	err := fs.WalkDir(os.DirFS(root), "docs", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(p, ".md") {
			return nil
		}
		if !generated && strings.HasPrefix(p, referenceDir+"/") && isGenerated(root, p) {
			return nil
		}
		add(p)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].path < out[j].path })
	return out
}

// isGenerated reports whether the file carries the mark of generated files.
func isGenerated(root, rel string) bool {
	data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
	return err == nil && strings.HasPrefix(string(data), generatedMark)
}

var schemeRE = regexp.MustCompile(`^[a-zA-Z][a-zA-Z0-9+.-]*:`)

// TestLinksResolve checks that every relative link of the README and of docs/
// points at a file that exists, and every #anchor at a heading of it.
func TestLinksResolve(t *testing.T) {
	root := repoRoot(t)
	cache := map[string]*mdFile{}
	load := func(rel string) *mdFile {
		if f, ok := cache[rel]; ok {
			return f
		}
		f, err := parseMarkdown(root, rel)
		if err != nil {
			t.Fatal(err)
		}
		cache[rel] = f
		return f
	}
	for _, f := range markdownFiles(t, root, true) {
		for _, l := range f.links {
			if schemeRE.MatchString(l.dest) {
				continue
			}
			target, frag, _ := strings.Cut(l.dest, "#")
			if un, err := url.PathUnescape(frag); err == nil {
				frag = un
			}
			rel := f.path
			if target != "" {
				un, err := url.PathUnescape(target)
				if err != nil {
					t.Errorf("%s:%d: cannot read the link %q", f.path, l.line, l.dest)
					continue
				}
				rel = path.Join(path.Dir(f.path), un)
			}
			if strings.HasPrefix(rel, "../") {
				t.Errorf("%s:%d: the link %q leaves the source tree", f.path, l.line, l.dest)
				continue
			}
			info, err := os.Stat(filepath.Join(root, filepath.FromSlash(rel)))
			if err != nil {
				t.Errorf("%s:%d: the link %q points at %s, which does not exist", f.path, l.line, l.dest, rel)
				continue
			}
			if frag == "" || info.IsDir() || !strings.HasSuffix(rel, ".md") {
				continue
			}
			if a := load(rel).anchors; !a[frag] && !a[strings.ToLower(frag)] {
				t.Errorf("%s:%d: the link %q points at no heading or anchor of %s", f.path, l.line, l.dest, rel)
			}
		}
	}
}

// shellWords splits a line of shell into words, with quotes and backslashes
// taken out and a comment cut off.
func shellWords(line string) []string {
	var words []string
	var cur strings.Builder
	has := false
	var quote rune
	rs := []rune(line)
	for i := 0; i < len(rs); i++ {
		r := rs[i]
		switch {
		case quote == '\'':
			if r == '\'' {
				quote = 0
			} else {
				cur.WriteRune(r)
			}
		case quote == '"':
			switch {
			case r == '"':
				quote = 0
			case r == '\\' && i+1 < len(rs):
				i++
				cur.WriteRune(rs[i])
			default:
				cur.WriteRune(r)
			}
		case r == '\'' || r == '"':
			quote, has = r, true
		case r == '\\' && i+1 < len(rs):
			i++
			cur.WriteRune(rs[i])
			has = true
		case r == '#' && !has:
			return words
		case r == ' ' || r == '\t':
			if has {
				words = append(words, cur.String())
				cur.Reset()
				has = false
			}
		default:
			cur.WriteRune(r)
			has = true
		}
	}
	if has {
		words = append(words, cur.String())
	}
	return words
}

// commandsIn finds the brig commands that a line of shell runs, each as the
// words after brig. A command is one that starts a line or follows ;, &&,
// || or |, or one in $( ).
func commandsIn(line string) [][]string {
	var out [][]string
	for _, m := range subshellRE.FindAllStringSubmatch(line, -1) {
		out = append(out, shellWords(m[1])[1:])
	}
	line = subshellRE.ReplaceAllString(line, "")
	words := shellWords(line)
	start := true
	for i, w := range words {
		switch w {
		case ";", "&&", "||", "|":
			start = true
			continue
		}
		if start && w == "brig" {
			end := len(words)
			for j := i + 1; j < len(words); j++ {
				if strings.ContainsAny(words[j], ";|&>") && len(words[j]) <= 2 || strings.HasPrefix(words[j], "2>") {
					end = j
					break
				}
			}
			out = append(out, words[i+1:end])
		}
		start = false
	}
	return out
}

var subshellRE = regexp.MustCompile(`\$\((brig [^)]*)\)`)

// shellLines joins the continued lines of a shell block.
func shellLines(lines []string) []string {
	var out []string
	var cur string
	for _, l := range lines {
		l = strings.TrimSpace(l)
		if cont, ok := strings.CutSuffix(l, "\\"); ok {
			cur += cont + " "
			continue
		}
		out = append(out, strings.TrimPrefix(cur+l, "$ "))
		cur = ""
	}
	if cur != "" {
		out = append(out, cur)
	}
	return out
}

var placeholderRE = regexp.MustCompile(`^[A-Z<\[(.]`)

// checkBrig checks that args, the words after brig, name a command of brig's
// and flags that it has. What follows -- is for another program.
func checkBrig(root *cobra.Command, args []string) error {
	c := root
	descend := true
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--":
			return nil
		case strings.HasPrefix(a, "--"):
			name, _, hasValue := strings.Cut(a[2:], "=")
			f := lookupFlag(c, name, "")
			if f == nil {
				return fmt.Errorf("brig %s has no flag --%s", c.CommandPath()[len("brig "):], name)
			}
			if !hasValue && f.Value.Type() != "bool" && f.NoOptDefVal == "" {
				i++
			}
		case strings.HasPrefix(a, "-") && len(a) > 1:
			for j := 1; j < len(a); j++ {
				f := lookupFlag(c, "", string(a[j]))
				if f == nil {
					return fmt.Errorf("%s has no flag -%c", c.CommandPath(), a[j])
				}
				if f.Value.Type() != "bool" && f.NoOptDefVal == "" {
					if j == len(a)-1 {
						i++
					}
					break
				}
			}
		case descend:
			if placeholderRE.MatchString(a) {
				descend = false
				continue
			}
			var next *cobra.Command
			for _, s := range c.Commands() {
				if s.Name() == a || s.HasAlias(a) {
					next = s
				}
			}
			switch {
			case next != nil:
				c = next
			case !c.Runnable() && len(c.Commands()) > 0:
				return fmt.Errorf("%s has no command %s", c.CommandPath(), a)
			default:
				descend = false
			}
		}
	}
	return nil
}

// lookupFlag finds the flag of c, its own or inherited, by name or shorthand.
func lookupFlag(c *cobra.Command, name, short string) *pflag.Flag {
	for _, fs := range []*pflag.FlagSet{c.Flags(), c.InheritedFlags()} {
		if name != "" {
			if f := fs.Lookup(name); f != nil {
				return f
			}
		} else if f := fs.ShorthandLookup(short); f != nil {
			return f
		}
	}
	return nil
}

func TestCheckBrig(t *testing.T) {
	root := commandTree()
	for _, ok := range []string{
		"sandbox create dev --connect -m ~/src/webapp:/sandbox/src -- --name webapp --template pi",
		"sandbox exec webapp --tty -- bash -l",
		"policy allow dev webapp --draft ch-17 --rule x --save --to webapp",
		"help fence-2", "--version", "--progress=plain list", "delete -y dev", "up --create -- --no-such-flag",
	} {
		if err := checkBrig(root, strings.Fields(ok)); err != nil {
			t.Errorf("brig %s: %v", ok, err)
		}
	}
	for _, bad := range []string{"sandbox nosuch", "create --nosuch", "list -Z", "frobnicate"} {
		if err := checkBrig(root, strings.Fields(bad)); err == nil {
			t.Errorf("brig %s is accepted", bad)
		}
	}
}

func TestCommandsIn(t *testing.T) {
	for line, want := range map[string]string{
		`brig use dev && brig sandbox create webapp`: "use dev|sandbox create webapp",
		`eval "$(brig env dev)"`:                     "env dev",
		`sudo dnf install $(brig print-fedora-deps)`: "print-fedora-deps",
		`echo hi | brig ssh dev -- cat`:              "ssh dev -- cat",
		`brig show dev -o json > show.json # note`:   "show dev -o json",
		`echo brig is nice`:                          "",
	} {
		var got []string
		for _, c := range commandsIn(line) {
			got = append(got, strings.Join(c, " "))
		}
		if strings.Join(got, "|") != want {
			t.Errorf("commandsIn(%q) = %q, want %q", line, got, want)
		}
	}
}

// TestCommandsInDocsParse checks the brig commands in the shell blocks of the
// docs and in the help texts: each must name a command and flags that exist.
func TestCommandsInDocsParse(t *testing.T) {
	root := repoRoot(t)
	tree := commandTree()
	checked := 0
	for _, f := range markdownFiles(t, root, true) {
		for _, b := range f.blocks {
			switch b.info {
			case "sh", "bash", "shell", "console":
			default:
				continue
			}
			for _, line := range shellLines(b.lines) {
				for _, args := range commandsIn(line) {
					checked++
					if err := checkBrig(tree, args); err != nil {
						t.Errorf("%s:%d: %q: %v", f.path, b.line, line, err)
					}
				}
			}
		}
	}
	if checked < 50 {
		t.Errorf("checked only %d commands; the docs hold more", checked)
	}
}

// TestCommandsInHelpParse does the same for the commands in the help texts
// that brig help prints and brig ui shows.
func TestCommandsInHelpParse(t *testing.T) {
	h, err := help.Load()
	if err != nil {
		t.Fatal(err)
	}
	tree := commandTree()
	check := func(where, line string) {
		for _, args := range commandsIn(line) {
			if err := checkBrig(tree, args); err != nil {
				t.Errorf("%s: %q: %v", where, line, err)
			}
		}
	}
	for _, k := range []help.Kind{help.Concept, help.Task} {
		for _, topic := range h.Topics(k) {
			for _, c := range topic.Commands {
				check(string(k)+" "+topic.ID, c)
			}
		}
	}
	for _, id := range h.ActionIDs() {
		a, _ := h.Action(id)
		check("action "+id, a.Command)
	}
}

// unmentioned are commands that no page that people wrote names yet. Name
// one in a page and remove it from here.
var unmentioned = map[string]string{
	"brig version": "prints the version; the guides have no reason to say more",
}

// TestCommandsAreDocumented checks that each command, or the command it is
// a subcommand of, is named in a page that people wrote, not only in the
// generated reference.
func TestCommandsAreDocumented(t *testing.T) {
	root := repoRoot(t)
	var text strings.Builder
	for _, f := range markdownFiles(t, root, false) {
		text.WriteString(f.text)
		for _, b := range f.blocks {
			text.WriteString(strings.Join(b.lines, "\n") + "\n")
		}
	}
	prose := regexp.MustCompile(`\s+`).ReplaceAllString(text.String(), " ")
	named := func(path string) bool {
		return regexp.MustCompile(`\b` + regexp.QuoteMeta(path) + `\b`).MatchString(prose)
	}
	walk(commandTree(), func(c *cobra.Command) {
		// cobra's own completion command explains itself.
		if c.Parent() == nil || strings.HasPrefix(c.CommandPath(), "brig completion") {
			return
		}
		// A page about brig sandbox covers brig sandbox detach.
		covered := false
		for p := c; p.Parent() != nil; p = p.Parent() {
			covered = covered || named(p.CommandPath())
		}
		switch reason, listed := unmentioned[c.CommandPath()]; {
		case covered && listed:
			t.Errorf("%s is named in a page now; remove it from unmentioned (%s)", c.CommandPath(), reason)
		case !covered && !listed:
			t.Errorf("no page that people wrote names %s; mention it in a guide or how-to", c.CommandPath())
		}
	})
}
