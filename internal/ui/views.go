// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

package ui

import (
	"bytes"
	"fmt"
	"html/template"
	"io/fs"
	"math"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/dennisklein/brig/internal/bytesize"
	"github.com/dennisklein/brig/internal/config"
	"github.com/dennisklein/brig/internal/doctor"
	"github.com/dennisklein/brig/internal/ui/store"
)

// pageNames are the page templates, one file each in web/templates.
var pageNames = []string{"overview", "vms", "vm", "host", "activity", "notfound"}

// fragments are the parts of pages that live updates replace: each is a
// template of the same name, rendered again when one of its topics changes,
// and sent as the SSE event of that name.
var fragments = map[string][]store.Topic{
	"rail":            {store.TopicVMs},
	"overview-sub":    {store.TopicVMs, store.TopicActivity},
	"vm-cards":        {store.TopicVMs},
	"needs":           {store.TopicVMs, store.TopicHost},
	"recent-activity": {store.TopicActivity},
	"vm-table":        {store.TopicVMs},
	"vm":              {store.TopicVMs},
	"host-checks":     {store.TopicHost},
	"activity-list":   {store.TopicActivity},
}

// vmTabs are the tabs of a VM's page, in order.
var vmTabs = []struct{ Key, Label string }{
	{"summary", "Summary"},
	{"mounts", "Mounts"},
	{"openshell", "OpenShell config"},
	{"network", "Network"},
	{"shell", "Shell"},
}

// templates holds the parsed templates: a base set with the layout and the
// fragments, and per page a clone of it with the page added.
type templates struct {
	load  func() (*template.Template, map[string]*template.Template, error)
	dev   bool
	mu    sync.Mutex
	base  *template.Template
	pages map[string]*template.Template
}

func newTemplates(fsys fs.FS, funcs template.FuncMap, dev bool) (*templates, error) {
	t := &templates{dev: dev, load: func() (*template.Template, map[string]*template.Template, error) {
		base, err := template.New("").Funcs(funcs).ParseFS(fsys, "templates/layout.tmpl", "templates/parts.tmpl")
		if err != nil {
			return nil, nil, err
		}
		pages := map[string]*template.Template{}
		for _, name := range pageNames {
			c, err := base.Clone()
			if err != nil {
				return nil, nil, err
			}
			if pages[name], err = c.ParseFS(fsys, "templates/"+name+".tmpl"); err != nil {
				return nil, nil, err
			}
		}
		return base, pages, nil
	}}
	var err error
	t.base, t.pages, err = t.load()
	return t, err
}

// get returns the base set and the page sets, parsed again in dev mode.
func (t *templates) get() (*template.Template, map[string]*template.Template, error) {
	if !t.dev {
		return t.base, t.pages, nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.load()
}

// page renders the named page.
func (t *templates) page(name string, v *view) ([]byte, error) {
	_, pages, err := t.get()
	if err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	err = pages[name].ExecuteTemplate(&buf, "layout", v)
	return buf.Bytes(), err
}

// fragment renders the named fragment.
func (t *templates) fragment(name string, v *view) (string, error) {
	base, _, err := t.get()
	if err != nil {
		return "", err
	}
	var buf bytes.Buffer
	err = base.ExecuteTemplate(&buf, name, v)
	return buf.String(), err
}

// view is what a page or a fragment is rendered from.
type view struct {
	// Page names the page, and the rail entry it lights up.
	Page  string
	Title string
	Snap  store.Snapshot
	Now   time.Time
	Demo  bool
	CSRF  string
	// Events is the URL of the page's live updates, empty for none.
	Events string
	// VM and Tab are set on a VM's page.
	VM  store.VM
	Tab string
	// Query is the jump box's query, for the palette.
	Query string
	// Asset returns the URL of a static file.
	Asset func(string) string
}

// eventsURL returns the URL of the live updates of the named fragments,
// with the parameters they need.
func eventsURL(names []string, params url.Values) string {
	q := url.Values{"f": names}
	for k, v := range params {
		q[k] = v
	}
	return "/events?" + q.Encode()
}

// need is an item of the Overview's "Needs you" list.
type need struct {
	Sev    string // bad, warn or info
	Title  string
	Detail string
	Link   string
	Action string
}

// needs lists what the user should look at, worst first.
func needs(sn store.Snapshot) []need {
	var out []need
	if sn.VMsErr != "" {
		out = append(out, need{Sev: "bad", Title: "brig cannot read everything about its VMs", Detail: sn.VMsErr, Link: "/host", Action: "Check host"})
	}
	for _, v := range sn.VMs {
		if v.LastSync != nil && v.LastSync.Error != "" {
			out = append(out, need{Sev: "bad", Title: "The last sync of " + v.Name + " failed", Detail: v.LastSync.Error, Link: "/vms/" + v.Name + "?tab=openshell", Action: "Open"})
		}
	}
	if d := sn.Doctor; d != nil {
		for _, r := range d.Results {
			if r.Status == doctor.OK {
				continue
			}
			sev := "warn"
			if r.Status == doctor.Fail {
				sev = "bad"
			}
			out = append(out, need{Sev: sev, Title: "Host check " + r.Name + ": " + r.Detail, Detail: firstLine(r.Hint), Link: "/host", Action: "Show"})
		}
	}
	for _, v := range sn.VMs {
		if olderImage(sn, v) {
			out = append(out, need{
				Sev: "info", Title: v.Name + " runs an older base image",
				Detail: fmt.Sprintf("%s (OpenShell %s) is newer. brig upgrade %s keeps the data disk.", sn.NewestImage.ID, sn.NewestImage.OpenShell, v.Name),
				Link:   "/vms/" + v.Name, Action: "Open",
			})
		}
	}
	order := map[string]int{"bad": 0, "warn": 1, "info": 2}
	slices.SortStableFunc(out, func(a, b need) int { return order[a.Sev] - order[b.Sev] })
	return out
}

// olderImage reports whether a newer base image than the VM's exists.
func olderImage(sn store.Snapshot, v store.VM) bool {
	return sn.NewestImage != nil && v.Image != sn.NewestImage.ID
}

func firstLine(s string) string {
	line, _, _ := strings.Cut(s, "\n")
	return line
}

// funcs returns the template functions. home is the user's home directory,
// shown as ~.
func funcs(home string) template.FuncMap {
	return template.FuncMap{
		"rel":   rel,
		"since": since,
		"clock": func(t time.Time) string { return t.Local().Format("15:04:05") },
		"stamp": func(t time.Time) string { return t.Local().Format("2006-01-02 15:04:05") },
		"size":  size,
		"pct": func(p *float64) string {
			if p == nil {
				return "–"
			}
			return fmt.Sprintf("%.0f%%", *p)
		},
		"width": func(p *float64) int {
			if p == nil {
				return 0
			}
			return int(math.Round(min(100, max(0, *p))))
		},
		"high":       func(p *float64) bool { return p != nil && *p > 80 },
		"stateClass": stateClass,
		"tilde": func(p string) string {
			if home != "" && (p == home || strings.HasPrefix(p, home+"/")) {
				return "~" + p[len(home):]
			}
			return p
		},
		"profile": func(sn store.Snapshot, name string) *config.NetworkProfile {
			if p, ok := sn.Profiles[name]; ok {
				return &p
			}
			return nil
		},
		"stat":     stat,
		"runClass": runClass,
		"cardOf": func(v *view, vm store.VM) card {
			return card{VM: vm, SyncErr: vm.LastSync != nil && vm.LastSync.Error != "", Older: olderImage(v.Snap, vm)}
		},
		"cmd": func(label, text string) command { return command{label, text} },
		"netrow": func(on bool, label, detail string) netRow {
			return netRow{on, label, detail}
		},
		"older":      olderImage,
		"needs":      needs,
		"running":    func(vms []store.VM) int { return count(vms, store.VM.Running) },
		"statusMark": statusMark,
		"statusClass": func(status string, done bool) string {
			switch {
			case !done:
				return "run"
			case status == "ok":
				return "done"
			case status == "failed" || status == "canceled":
				return "fail"
			}
			return "skip"
		},
		"checkClass": func(s doctor.Status) string {
			switch s {
			case doctor.OK:
				return ""
			case doctor.Warn:
				return "warn"
			}
			return "bad"
		},
		"checkCounts": func(rs []doctor.Result) map[string]int {
			m := map[string]int{}
			for _, r := range rs {
				m[r.Status.String()]++
			}
			return m
		},
		"vmTabs": func() any { return vmTabs },
		"hostPorts": func(ports []uint16) string {
			s := make([]string, len(ports))
			for i, p := range ports {
				s[i] = fmt.Sprint(p)
			}
			return strings.Join(s, ", ")
		},
		"lines":   func(s string) []string { return strings.Split(s, "\n") },
		"palette": palette,
	}
}

// card is what a VM card shows.
type card struct {
	VM      store.VM
	SyncErr bool
	Older   bool
}

// command is a command block: a label, and the command to copy.
type command struct{ Label, Text string }

// netRow is a row of a network profile's table.
type netRow struct {
	On            bool
	Label, Detail string
}

// stat returns one of a VM's usage numbers: cpu, mem or data.
func stat(v store.VM, which string) *float64 {
	if v.Stats == nil {
		return nil
	}
	switch which {
	case "cpu":
		return v.Stats.CPU
	case "mem":
		return v.Stats.Memory
	case "data":
		return v.Stats.Data
	}
	return nil
}

// runClass is the colour of a run's dot: running, failed, with a step
// skipped, or ok.
func runClass(r store.Run) string {
	switch {
	case !r.Done():
		return "info"
	case r.Status == "failed" || r.Status == "canceled":
		return "bad"
	case r.Status == "skipped" || anySkipped(r.Steps):
		return "warn"
	}
	return ""
}

func anySkipped(spans []store.Span) bool {
	for _, s := range spans {
		if s.Status == "skipped" || s.Status == "failed" || anySkipped(s.Steps) {
			return true
		}
	}
	return false
}

func count[T any](xs []T, f func(T) bool) int {
	n := 0
	for _, x := range xs {
		if f(x) {
			n++
		}
	}
	return n
}

// rel says how long ago t was, such as "7 min ago".
func rel(t time.Time) string {
	if t.IsZero() {
		return "never"
	}
	return since(t) + " ago"
}

// since says how long ago t was, without "ago": "just now" is "now".
func since(t time.Time) string {
	d := time.Since(t)
	switch {
	case d < 45*time.Second:
		return "under a minute"
	case d < time.Hour:
		return fmt.Sprintf("%d min", int(math.Round(d.Minutes())))
	case d < 48*time.Hour:
		return fmt.Sprintf("%d h", int(math.Round(d.Hours())))
	}
	return fmt.Sprintf("%d d", int(math.Round(d.Hours()/24)))
}

// size writes a size as "8 GiB".
func size(s bytesize.Size) string {
	str := s.String()
	i := strings.IndexFunc(str, func(r rune) bool { return r < '0' || r > '9' })
	if i <= 0 {
		return str
	}
	return str[:i] + " " + str[i:]
}

// stateClass is the colour of a domain state's pill.
func stateClass(state string) string {
	switch state {
	case "running":
		return "ok"
	case "paused":
		return "warn"
	case "missing", "other", "unknown":
		return "bad"
	}
	return ""
}

// statusMark is the mark of a step in a progress tree, as the terminal
// draws it.
func statusMark(status string, done bool) string {
	switch {
	case !done:
		return "›"
	case status == "ok":
		return "✓"
	case status == "failed", status == "canceled":
		return "✗"
	}
	return "–"
}

// paletteItem is an entry of the jump box.
type paletteItem struct {
	Label, Kind, Link string
}

// palette lists the jump box's entries that match q.
func palette(sn store.Snapshot, q string) []paletteItem {
	items := []paletteItem{
		{"Overview", "page", "/"},
		{"VMs", "page", "/vms"},
		{"Host checks", "page", "/host"},
		{"Activity", "page", "/activity"},
	}
	for _, v := range sn.VMs {
		items = append(items, paletteItem{"VM " + v.Name, v.State, "/vms/" + v.Name})
		for _, t := range vmTabs[1:] {
			items = append(items, paletteItem{v.Name + " · " + t.Label, "VM " + v.Name, "/vms/" + v.Name + "?tab=" + t.Key})
		}
	}
	q = strings.ToLower(strings.TrimSpace(q))
	var out []paletteItem
	for _, it := range items {
		if strings.Contains(strings.ToLower(it.Label), q) {
			out = append(out, it)
		}
	}
	return out
}
