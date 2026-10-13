// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

package ui

import (
	"fmt"
	"html/template"
	"net/http"
	"regexp"
	"strings"

	"github.com/dennisklein/brig/internal/help"
	"github.com/dennisklein/brig/internal/ui/store"
)

// brig ui explains itself in layers, each opened by the user, none by
// itself: hint cards on the words and buttons that templates mark, the help
// pane that ? opens, and help entries in the jump box. The texts are those
// of brig help TOPIC, from package help.

// helpFuncs returns the template functions that show help.
func helpFuncs(h *help.Help) template.FuncMap {
	return template.FuncMap{
		// term marks text as the concept id, which a hint card explains.
		"term": func(id, text string) (template.HTML, error) {
			if t, ok := h.Topic(id); !ok || t.Kind != help.Concept {
				return "", fmt.Errorf("term: there is no concept %s", id)
			}
			return template.HTML(`<span class="term" tabindex="0" data-term="` + template.HTMLEscapeString(id) + `">` + //nolint:gosec // G203: both parts are escaped
				template.HTMLEscapeString(text) + `</span>`), nil
		},
		"helpText":   helpText,
		"helpTopic":  func(id string) *help.Topic { t, _ := h.Topic(id); return t },
		"helpAction": func(id string) *help.Action { a, _ := h.Action(id); return a },
		"helpTasks":  func() []*help.Topic { return h.Topics(help.Task) },
		"helpTerms":  func() []*help.Topic { return h.Topics(help.Concept) },
		"palette": func(sn store.Snapshot, q string) []paletteItem {
			return helpPalette(h, sn, q)
		},
	}
}

var codeRE = regexp.MustCompile("`([^`]+)`")

// helpText writes a help text as HTML, with its `code` marked.
func helpText(s string) template.HTML {
	return template.HTML(codeRE.ReplaceAllString(template.HTMLEscapeString(s), "<code>$1</code>")) //nolint:gosec // G203: escaped first
}

// helpPalette lists the jump box's entries for q: pages and objects first,
// then concepts and how-tos. A query that starts with ? finds help only.
func helpPalette(h *help.Help, sn store.Snapshot, q string) []paletteItem {
	q = strings.TrimSpace(q)
	rest, only := strings.CutPrefix(q, "?")
	var out []paletteItem
	if !only {
		out = palette(sn, q)
	}
	rest = strings.TrimSpace(rest)
	if rest == "" {
		if !only {
			return out
		}
		for _, t := range h.Topics(help.Task) {
			out = append(out, paletteItem{t.Title, "how to", helpURL(string(t.Kind), t.ID)})
		}
		for _, t := range h.Topics(help.Concept) {
			out = append(out, paletteItem{t.Title, "concept", helpURL(string(t.Kind), t.ID)})
		}
		return out
	}
	for _, hit := range h.Search(rest) {
		kind := hit.Kind
		switch kind {
		case "action":
			continue
		case string(help.Task):
			kind = "how to"
		}
		out = append(out, paletteItem{hit.Title, kind, helpURL(hit.Kind, hit.ID)})
	}
	return out
}

// helpURL is the URL of the help pane's entry for a topic or an action.
func helpURL(kind, id string) string { return "/help/" + kind + "/" + id }

// helpView is what the help pane's parts are rendered from.
type helpView struct {
	*view
	Page   *help.Page
	Topic  *help.Topic
	Action *help.Action
	Query  string
	Hits   []help.Hit
}

// helpPane serves GET /help: the pane's entry for a page and its tab.
func (s *Server) helpPane(w http.ResponseWriter, r *http.Request) {
	p, ok := s.help.Page(r.URL.Query().Get("page"))
	if !ok {
		p, _ = s.help.Page("overview")
	}
	v := &helpView{view: s.newView(r, "", ""), Page: p}
	v.Tab = r.URL.Query().Get("tab")
	s.writeFragment(w, "help-page", v)
}

// helpAll serves GET /help/all, the pane's index of every topic.
func (s *Server) helpAll(w http.ResponseWriter, r *http.Request) {
	s.writeFragment(w, "help-all", &helpView{view: s.newView(r, "", "")})
}

// helpEntry serves GET /help/KIND/ID, the pane's entry for a concept, a task
// or an action, and GET /help/hint/KIND/ID, its hint card.
func (s *Server) helpEntry(w http.ResponseWriter, r *http.Request) {
	kind, id := r.PathValue("kind"), r.PathValue("id")
	v := &helpView{view: s.newView(r, "", "")}
	switch kind {
	case string(help.Concept), string(help.Task):
		t, ok := s.help.Topic(id)
		if !ok || string(t.Kind) != kind {
			http.NotFound(w, r)
			return
		}
		v.Topic = t
	case "action":
		a, ok := s.help.Action(id)
		if !ok {
			http.NotFound(w, r)
			return
		}
		v.Action = a
	default:
		http.NotFound(w, r)
		return
	}
	name := "help-entry"
	if strings.HasPrefix(r.URL.Path, "/help/hint/") {
		name = "help-hint"
	}
	s.writeFragment(w, name, v)
}

// helpSearch serves GET /help/search?q=, the pane's search results.
func (s *Server) helpSearch(w http.ResponseWriter, r *http.Request) {
	v := &helpView{view: s.newView(r, "", ""), Query: r.URL.Query().Get("q")}
	v.Hits = s.help.Search(v.Query)
	s.writeFragment(w, "help-search", v)
}

// writeFragment writes the named fragment, or a server error.
func (s *Server) writeFragment(w http.ResponseWriter, name string, data any) {
	html, err := s.tmpl.fragment(name, data)
	if err != nil {
		s.log.Error("rendering", "fragment", name, "err", err)
		http.Error(w, "rendering failed", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write([]byte(html))
}
