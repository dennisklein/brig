// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"regexp"
	"strings"
	"unicode"
)

// escape makes plain text safe to put in Markdown: what brig's help texts
// write as <name> or *.com must not turn into HTML or emphasis. Backslash
// escapes work in GitHub's Markdown and in man pages alike.
func escape(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch r {
		case '\\', '<', '>', '*', '_', '`':
			b.WriteByte('\\')
		}
		b.WriteRune(r)
	}
	return listMarkRE.ReplaceAllString(b.String(), `${1}\${2}${3}`)
}

// listMarkRE finds a line that starts like a list item or a heading.
var listMarkRE = regexp.MustCompile(`(?m)^( {0,3})([-+#]|\d+[.)])(\s|$)`)

// inline escapes text that may hold `code` spans, which stay as they are.
func inline(s string) string {
	if strings.Count(s, "`")%2 != 0 {
		return escape(s)
	}
	parts := strings.Split(s, "`")
	for i := range parts {
		if i%2 == 0 {
			parts[i] = escape(parts[i])
		} else {
			parts[i] = "`" + parts[i] + "`"
		}
	}
	return strings.Join(parts, "")
}

// cell makes text fit a table cell.
func cell(s string) string {
	return strings.ReplaceAll(strings.Join(strings.Fields(inline(s)), " "), "|", `\|`)
}

// code puts s in a code span that cannot contain a backtick.
func code(s string) string { return "`" + strings.ReplaceAll(s, "`", "'") + "`" }

// fence writes lines as a fenced block of the given kind.
func fence(kind string, lines ...string) string {
	return "```" + kind + "\n" + strings.Join(lines, "\n") + "\n```\n"
}

// dedent removes the indentation that all non-blank lines of s share.
func dedent(s string) string {
	lines := strings.Split(s, "\n")
	indent := -1
	for _, l := range lines {
		if strings.TrimSpace(l) == "" {
			continue
		}
		n := len(l) - len(strings.TrimLeft(l, " \t"))
		if indent < 0 || n < indent {
			indent = n
		}
	}
	if indent <= 0 {
		return s
	}
	for i, l := range lines {
		if len(l) >= indent {
			lines[i] = l[indent:]
		} else {
			lines[i] = strings.TrimLeft(l, " \t")
		}
	}
	return strings.Join(lines, "\n")
}

// textToMarkdown turns a help text such as a command's Long into Markdown.
// The texts are plain paragraphs, with the occasional indented block for
// flags, files or shell lines, which becomes a code block. A paragraph goes
// through link, which may mark up the references in it.
func textToMarkdown(text string, link func(paragraph string) string) string {
	var out []string
	var para []string
	flush := func() {
		if len(para) == 0 {
			return
		}
		p := escape(strings.Join(para, "\n"))
		if link != nil {
			p = link(p)
		}
		out = append(out, p+"\n")
		para = nil
	}
	lines := strings.Split(strings.Trim(text, "\n"), "\n")
	indented := func(l string) bool { return strings.HasPrefix(l, " ") || strings.HasPrefix(l, "\t") }
	for i := 0; i < len(lines); i++ {
		l := lines[i]
		switch {
		case strings.TrimSpace(l) == "":
			flush()
		case indented(l):
			flush()
			// The block goes on over blank lines if more indented lines follow.
			j := i
			for k := i; k < len(lines); k++ {
				if indented(lines[k]) {
					j = k
				} else if strings.TrimSpace(lines[k]) != "" {
					break
				}
			}
			out = append(out, fence("text", strings.Split(dedent(strings.Join(lines[i:j+1], "\n")), "\n")...))
			i = j
		default:
			para = append(para, strings.TrimRight(l, " "))
		}
	}
	flush()
	return strings.Join(out, "\n")
}

// slug returns the anchor that GitHub makes of a heading: lower case, spaces
// as hyphens, and everything but letters, digits, hyphens and underscores
// dropped.
func slug(heading string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(heading) {
		switch {
		case unicode.IsLetter(r) || unicode.IsDigit(r) || r == '-' || r == '_':
			b.WriteRune(r)
		case r == ' ':
			b.WriteByte('-')
		}
	}
	return b.String()
}

var (
	headingLinkRE = regexp.MustCompile(`\[([^\]]*)\]\([^)]*\)`)
	htmlTagRE     = regexp.MustCompile(`<[^>]*>`)
)

// headingText returns what a heading shows of its Markdown source: link
// texts, code and emphasis without their marks.
func headingText(src string) string {
	s := headingLinkRE.ReplaceAllString(src, "$1")
	s = htmlTagRE.ReplaceAllString(s, "")
	s = strings.NewReplacer("`", "", "*", "", `\`, "").Replace(s)
	return strings.TrimSpace(s)
}
