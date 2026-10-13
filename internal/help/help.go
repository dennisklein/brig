// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

// Package help holds brig's help texts: what its concepts mean, how to do
// common things, what each page of brig ui shows and what each of its
// actions does. brig help TOPIC prints them, and brig ui shows them as hints
// and in its help pane, so both say the same.
//
// Concepts and tasks are Markdown files with a YAML header, one per topic;
// pages and actions are YAML files, since they belong to brig ui's screens.
package help

import (
	"bytes"
	"embed"
	"fmt"
	"io/fs"
	"path"
	"regexp"
	"slices"
	"strings"

	"go.yaml.in/yaml/v3"
)

//go:embed concepts tasks pages.yaml actions.yaml
var files embed.FS

// Kind is what a topic is.
type Kind string

const (
	// Concept is a word that brig and brig ui use, such as fence-2.
	Concept Kind = "concept"
	// Task is a way to do something, step by step.
	Task Kind = "task"
)

// Topic is a concept or a task.
type Topic struct {
	Kind Kind
	// ID names the topic in brig help ID and in URLs.
	ID    string
	Title string
	// Summary is one sentence, short enough for a hint card. Tasks have
	// none.
	Summary string
	// Aliases are other words for the topic, for search.
	Aliases []string
	// See names related concepts.
	See []string
	// Concepts names the concepts a task uses.
	Concepts []string
	// Commands are commands to run in a terminal, such as examples.
	Commands []string
	// UI is the path of the page of brig ui where the topic lives, if any.
	UI string
	// Show is where a task's Show me goes: a page, and the action whose
	// button it points at there.
	Show *Show
	// Body is the text after the header: paragraphs, and a task's steps.
	Body []Block
}

// Show is where a task's Show me goes.
type Show struct {
	Href   string `yaml:"href"`
	Action string `yaml:"action"`
}

// Block is a paragraph, or a numbered list of steps.
type Block struct {
	Text  string
	Steps []string
}

// Page is the help for a page of brig ui.
type Page struct {
	ID       string
	Title    string   `yaml:"title"`
	Summary  string   `yaml:"summary"`
	Concepts []string `yaml:"concepts"`
	Tasks    []string `yaml:"tasks"`
	// Tabs lists, per tab, the concepts that come first there.
	Tabs map[string][]string `yaml:"tabs"`
}

// Action is the help for an action of brig ui, one of its dialogs.
type Action struct {
	ID      string
	Title   string `yaml:"title"`
	Summary string `yaml:"summary"`
	// Command is the command the action runs, with placeholders in capitals.
	Command string `yaml:"command"`
	// Undo names the action that undoes it, if one does.
	Undo string `yaml:"undo"`
	// Danger says what cannot be undone.
	Danger   string   `yaml:"danger"`
	Concepts []string `yaml:"concepts"`
}

// Help is the parsed help texts.
type Help struct {
	topics  map[string]*Topic
	order   []string
	pages   map[string]*Page
	actions map[string]*Action
}

// Load parses the help texts.
func Load() (*Help, error) { return load(files) }

func load(fsys fs.FS) (*Help, error) {
	h := &Help{topics: map[string]*Topic{}, pages: map[string]*Page{}, actions: map[string]*Action{}}
	for _, k := range []Kind{Concept, Task} {
		dir := string(k) + "s"
		names, err := fs.Glob(fsys, dir+"/*.md")
		if err != nil {
			return nil, err
		}
		for _, name := range names {
			data, err := fs.ReadFile(fsys, name)
			if err != nil {
				return nil, err
			}
			t, err := parseTopic(k, strings.TrimSuffix(path.Base(name), ".md"), data)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", name, err)
			}
			if _, dup := h.topics[t.ID]; dup {
				return nil, fmt.Errorf("%s: there is another topic %s", name, t.ID)
			}
			h.topics[t.ID] = t
			h.order = append(h.order, t.ID)
		}
	}
	if err := readYAML(fsys, "pages.yaml", h.pages); err != nil {
		return nil, err
	}
	for id, p := range h.pages {
		p.ID = id
	}
	if err := readYAML(fsys, "actions.yaml", h.actions); err != nil {
		return nil, err
	}
	for id, a := range h.actions {
		a.ID = id
	}
	return h, nil
}

func readYAML(fsys fs.FS, name string, v any) error {
	data, err := fs.ReadFile(fsys, name)
	if err != nil {
		return err
	}
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(v); err != nil {
		return fmt.Errorf("%s: %w", name, err)
	}
	return nil
}

// header is a topic file's YAML header.
type header struct {
	Title    string   `yaml:"title"`
	Summary  string   `yaml:"summary"`
	Aliases  []string `yaml:"aliases"`
	See      []string `yaml:"see"`
	Concepts []string `yaml:"concepts"`
	Commands []string `yaml:"commands"`
	UI       string   `yaml:"ui"`
	Show     *Show    `yaml:"show"`
}

var stepRE = regexp.MustCompile(`^\d+\. `)

func parseTopic(k Kind, id string, data []byte) (*Topic, error) {
	rest, ok := bytes.CutPrefix(data, []byte("---\n"))
	if !ok {
		return nil, fmt.Errorf("it does not start with a --- header")
	}
	head, body, ok := bytes.Cut(rest, []byte("\n---\n"))
	if !ok {
		return nil, fmt.Errorf("its header does not end with ---")
	}
	var hd header
	dec := yaml.NewDecoder(bytes.NewReader(head))
	dec.KnownFields(true)
	if err := dec.Decode(&hd); err != nil {
		return nil, err
	}
	t := &Topic{
		Kind: k, ID: id, Title: hd.Title, Summary: oneLine(hd.Summary), Aliases: hd.Aliases, See: hd.See,
		Concepts: hd.Concepts, Commands: hd.Commands, UI: hd.UI, Show: hd.Show,
	}
	for _, para := range strings.Split(strings.TrimSpace(string(body)), "\n\n") {
		lines := strings.Split(strings.TrimSpace(para), "\n")
		if len(lines) == 0 || lines[0] == "" {
			continue
		}
		if stepRE.MatchString(lines[0]) {
			var b Block
			for _, l := range lines {
				if stepRE.MatchString(l) {
					b.Steps = append(b.Steps, stepRE.ReplaceAllString(l, ""))
				} else if len(b.Steps) > 0 {
					b.Steps[len(b.Steps)-1] += " " + strings.TrimSpace(l)
				}
			}
			t.Body = append(t.Body, b)
			continue
		}
		t.Body = append(t.Body, Block{Text: oneLine(para)})
	}
	return t, nil
}

// oneLine joins the lines of s with spaces.
func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }

// Topic returns the topic id.
func (h *Help) Topic(id string) (*Topic, bool) {
	t, ok := h.topics[id]
	return t, ok
}

// Topics lists the topics of kind k by title.
func (h *Help) Topics(k Kind) []*Topic {
	var out []*Topic
	for _, id := range h.order {
		if t := h.topics[id]; t.Kind == k {
			out = append(out, t)
		}
	}
	slices.SortFunc(out, func(a, b *Topic) int { return strings.Compare(strings.ToLower(a.Title), strings.ToLower(b.Title)) })
	return out
}

// Page returns the help for the page id.
func (h *Help) Page(id string) (*Page, bool) {
	p, ok := h.pages[id]
	return p, ok
}

// PageIDs lists the pages that have help.
func (h *Help) PageIDs() []string {
	ids := make([]string, 0, len(h.pages))
	for id := range h.pages {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	return ids
}

// PageConcepts lists the concepts of the page id, those of its tab first.
func (p *Page) PageConcepts(tab string) []string {
	out := slices.Clone(p.Tabs[tab])
	for _, c := range p.Concepts {
		if !slices.Contains(out, c) {
			out = append(out, c)
		}
	}
	return out
}

// Action returns the help for the action id.
func (h *Help) Action(id string) (*Action, bool) {
	a, ok := h.actions[id]
	return a, ok
}

// ActionIDs lists the actions that have help.
func (h *Help) ActionIDs() []string {
	ids := make([]string, 0, len(h.actions))
	for id := range h.actions {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	return ids
}

// Hit is a search result.
type Hit struct {
	Kind  string // concept, task or action
	ID    string
	Title string
	score int
}

// Search returns the topics and actions that match every word of q, best
// first.
func (h *Help) Search(q string) []Hit {
	words := strings.Fields(strings.ToLower(q))
	if len(words) == 0 {
		return nil
	}
	var hits []Hit
	add := func(kind, id, title string, head []string, rest []string) {
		hd := strings.ToLower(strings.Join(head, " "))
		all := strings.ToLower(strings.Join(rest, " "))
		score := 0
		for _, w := range words {
			switch {
			case strings.Contains(hd, w):
				score += 3
			case strings.Contains(all, w):
				score++
			default:
				return
			}
		}
		hits = append(hits, Hit{Kind: kind, ID: id, Title: title, score: score})
	}
	for _, id := range h.order {
		t := h.topics[id]
		rest := append([]string{t.Summary}, t.Commands...)
		for _, b := range t.Body {
			rest = append(append(rest, b.Text), b.Steps...)
		}
		add(string(t.Kind), t.ID, t.Title, append([]string{t.ID, t.Title}, t.Aliases...), rest)
	}
	for _, id := range h.ActionIDs() {
		a := h.actions[id]
		add("action", a.ID, a.Title, []string{a.Title}, []string{a.Summary, a.Command})
	}
	slices.SortStableFunc(hits, func(a, b Hit) int {
		if a.score != b.score {
			return b.score - a.score
		}
		return strings.Compare(a.Title, b.Title)
	})
	return hits
}

// Check reports references to topics that do not exist and texts that are
// too long for where they show.
func (h *Help) Check() []error {
	var errs []error
	concept := func(where, id string) {
		if t, ok := h.topics[id]; !ok || t.Kind != Concept {
			errs = append(errs, fmt.Errorf("%s: there is no concept %s", where, id))
		}
	}
	for _, id := range h.order {
		t := h.topics[id]
		where := string(t.Kind) + " " + id
		if t.Title == "" || (t.Kind == Concept && t.Summary == "") {
			errs = append(errs, fmt.Errorf("%s: it needs a title, and a concept a summary", where))
		}
		if len(t.Summary) > maxSummary {
			errs = append(errs, fmt.Errorf("%s: its summary has %d characters; keep it under %d for a hint card", where, len(t.Summary), maxSummary))
		}
		for _, s := range t.See {
			concept(where, s)
		}
		for _, s := range t.Concepts {
			concept(where, s)
		}
		if t.Kind == Task && (len(t.Body) == 0 || len(t.Body[0].Steps) == 0) {
			errs = append(errs, fmt.Errorf("%s: a task starts with numbered steps", where))
		}
		if t.Show != nil {
			if _, ok := h.actions[t.Show.Action]; !ok && t.Show.Action != "" {
				errs = append(errs, fmt.Errorf("%s: its show names no action %s", where, t.Show.Action))
			}
		}
	}
	for _, id := range h.PageIDs() {
		p := h.pages[id]
		for _, c := range p.Concepts {
			concept("page "+id, c)
		}
		for tab, cs := range p.Tabs {
			for _, c := range cs {
				concept("page "+id+" tab "+tab, c)
			}
		}
		for _, t := range p.Tasks {
			if tt, ok := h.topics[t]; !ok || tt.Kind != Task {
				errs = append(errs, fmt.Errorf("page %s: there is no task %s", id, t))
			}
		}
	}
	for _, id := range h.ActionIDs() {
		a := h.actions[id]
		if a.Title == "" || a.Summary == "" || a.Command == "" {
			errs = append(errs, fmt.Errorf("action %s: it needs a title, a summary and a command", id))
		}
		if len(a.Summary) > maxSummary {
			errs = append(errs, fmt.Errorf("action %s: its summary has %d characters; keep it under %d", id, len(a.Summary), maxSummary))
		}
		for _, c := range a.Concepts {
			concept("action "+id, c)
		}
	}
	return errs
}

// maxSummary is the longest summary, in bytes, that fits a hint card.
const maxSummary = 160
