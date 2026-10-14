// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

package help

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"testing/fstest"
	"unicode"

	"github.com/dennisklein/brig/internal/docs"
)

func TestHelpIsConsistent(t *testing.T) {
	h, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	for _, err := range h.Check() {
		t.Error(err)
	}
	if len(h.Topics(Concept)) == 0 || len(h.Topics(Task)) == 0 {
		t.Fatal("no concepts or no tasks")
	}
}

func TestParseTopic(t *testing.T) {
	fsys := fstest.MapFS{
		"concepts/a.md": {Data: []byte("---\ntitle: A\nsummary: The\n  letter a.\nsee: [b]\ndoc: concepts/a.md#the-letter\n---\nFirst\nparagraph.\n\nSecond.\n")},
		"concepts/b.md": {Data: []byte("---\ntitle: B\nsummary: The letter b.\n---\n")},
		"tasks/do.md":   {Data: []byte("---\ntitle: Do it\nconcepts: [a]\n---\n1. One\n   more.\n2. Two.\n\nAfter.\n")},
		"pages.yaml":    {Data: []byte("home:\n  title: Home\n  summary: Start.\n  concepts: [a]\n  tasks: [do]\n  tabs:\n    x: [b]\n")},
		"actions.yaml":  {Data: []byte("go:\n  title: Go\n  summary: Goes.\n  command: brig go VM\n")},
	}
	h, err := load(fsys)
	if err != nil {
		t.Fatal(err)
	}
	if errs := h.Check(); len(errs) > 0 {
		t.Fatal(errs)
	}
	a, _ := h.Topic("a")
	if a.Summary != "The letter a." || len(a.Body) != 2 || a.Body[0].Text != "First paragraph." {
		t.Errorf("a = %+v", a)
	}
	if want := "More in brig's docs:\n  " + docs.URL("concepts/a.md#the-letter") + "\n"; !strings.Contains(h.Text(a), want) {
		t.Errorf("Text(a) lacks %q:\n%s", want, h.Text(a))
	}
	do, _ := h.Topic("do")
	if do.Kind != Task || len(do.Body) != 2 || len(do.Body[0].Steps) != 2 || do.Body[0].Steps[0] != "One more." || do.Body[1].Text != "After." {
		t.Errorf("do = %+v", do)
	}
	p, _ := h.Page("home")
	if got := p.PageConcepts("x"); strings.Join(got, ",") != "b,a" {
		t.Errorf("PageConcepts(x) = %v", got)
	}
	if hits := h.Search("letter"); len(hits) != 2 {
		t.Errorf("Search(letter) = %v", hits)
	}
	if hits := h.Search("goes"); len(hits) != 1 || hits[0].Kind != "action" {
		t.Errorf("Search(goes) = %v", hits)
	}
	if hits := h.Search("b letter"); len(hits) != 1 || hits[0].ID != "b" {
		t.Errorf("Search(b letter) = %v", hits)
	}
}

func TestCheckFindsDanglingReferences(t *testing.T) {
	fsys := fstest.MapFS{
		"concepts/a.md": {Data: []byte("---\ntitle: A\nsummary: A.\nsee: [nope]\n---\n")},
		"tasks/do.md":   {Data: []byte("---\ntitle: Do\n---\nNo steps.\n")},
		"pages.yaml":    {Data: []byte("home:\n  title: Home\n  summary: S.\n  tasks: [gone]\n")},
		"actions.yaml":  {Data: []byte("go:\n  title: Go\n  summary: Goes.\n")},
	}
	h, err := load(fsys)
	if err != nil {
		t.Fatal(err)
	}
	if errs := h.Check(); len(errs) != 4 {
		t.Errorf("Check() = %v, want 4 errors", errs)
	}
}

func TestText(t *testing.T) {
	h, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	f2, ok := h.Topic("fence-2")
	if !ok {
		t.Fatal("no fence-2")
	}
	text := h.Text(f2)
	for _, want := range []string{"Fence 2, the network profile\n\n  The VM's network profile", "In a terminal:\n  brig update dev --profile default", "brig help fence-1", "More in brig's docs:\n  https://"} {
		if !strings.Contains(text, want) {
			t.Errorf("Text(fence-2) lacks %q:\n%s", want, text)
		}
	}
	for _, l := range strings.Split(text, "\n") {
		if len(l) > width && !strings.HasPrefix(l, "  brig") && !strings.HasPrefix(l, "  secret-tool") && !strings.HasPrefix(l, "  https://") {
			t.Errorf("line longer than %d: %q", width, l)
		}
	}
	if !strings.Contains(h.Index(), "fence-2") {
		t.Error("Index lacks fence-2")
	}
}

// TestDocsExist checks that every topic links to a page of brig's docs, and
// that the page and its heading exist.
func TestDocsExist(t *testing.T) {
	h, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range []Kind{Concept, Task} {
		for _, tp := range h.Topics(k) {
			if tp.Doc == "" {
				t.Errorf("%s %s: it needs a doc", k, tp.ID)
				continue
			}
			page, heading, _ := strings.Cut(tp.Doc, "#")
			data, err := os.ReadFile(filepath.Join("../../docs", page))
			if err != nil {
				t.Errorf("%s %s: %v", k, tp.ID, err)
				continue
			}
			if heading != "" && !slices.Contains(headings(string(data)), heading) {
				t.Errorf("%s %s: %s has no heading #%s", k, tp.ID, page, heading)
			}
		}
	}
}

// headings returns the anchors that GitHub gives the headings of a
// Markdown page, outside code blocks.
func headings(md string) []string {
	var out []string
	inCode := false
	for _, l := range strings.Split(md, "\n") {
		if strings.HasPrefix(l, "```") {
			inCode = !inCode
		}
		if inCode || !strings.HasPrefix(l, "#") {
			continue
		}
		var b strings.Builder
		for _, r := range strings.ToLower(strings.TrimSpace(strings.TrimLeft(l, "#"))) {
			switch {
			case r == ' ':
				b.WriteRune('-')
			case r == '-' || r == '_' || unicode.IsLetter(r) || unicode.IsDigit(r):
				b.WriteRune(r)
			}
		}
		out = append(out, b.String())
	}
	return out
}
