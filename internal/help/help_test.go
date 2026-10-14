// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

package help

import (
	"strings"
	"testing"
	"testing/fstest"

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
	for _, want := range []string{"Fence 2, the network profile\n\n  The VM's network profile", "In a terminal:\n  brig update dev --profile default", "brig help fence-1"} {
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
