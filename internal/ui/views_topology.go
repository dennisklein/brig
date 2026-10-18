// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

package ui

import (
	"cmp"
	"fmt"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/dennisklein/brig/internal/ui/store"
)

// The topology is an SVG in five columns: this host, the VMs' gateways,
// their sandboxes, the providers attached to them, and the endpoints that
// the providers' credentials go to or that sandboxes reached or were denied
// in the last hour. Fence 1, OpenShell's proxy, stands between sandboxes
// and the world.

const (
	topoNodeH = 40
	topoGap   = 14
	topoTop   = 60
	// topoMaxEndpoints bounds the endpoints shown; the rest are counted.
	topoMaxEndpoints = 40
)

var (
	topoX     = [5]int{20, 200, 420, 640, 860}
	topoW     = [5]int{150, 180, 180, 180, 230}
	topoHeads = [5]string{"Host", "VM gateway", "Sandbox", "Provider", "Endpoint"}
)

// topoNode is a box of the topology.
type topoNode struct {
	ID, Label, Sub string
	X, Y, W        int
	// Off is a VM or sandbox that does not run; Bad an endpoint that was
	// denied in the last hour.
	Off, Bad bool
	// Sel is the selected node; Dim one not connected to it.
	Sel, Dim bool
	// Link selects the node.
	Link string
}

// TextX is where the node's labels start.
func (n topoNode) TextX() int { return n.X + 22 }

// DotX is where the node's state dot is.
func (n topoNode) DotX() int { return n.X + 12 }

// DotY is where the node's state dot is.
func (n topoNode) DotY() int { return n.Y + 14 }

// LabelY is the baseline of the node's label.
func (n topoNode) LabelY() int { return n.Y + 18 }

// SubY is the baseline of the line below the node's label.
func (n topoNode) SubY() int { return n.Y + 32 }

// topoEdge is a line between two nodes.
type topoEdge struct {
	From, To string
	// Class is "" for configured, hot for a provider of a running
	// sandbox, seen for traffic allowed in the last hour and deny for
	// traffic denied in it.
	Class string
	D     string
	Dim   bool
}

// topoHead is a column's heading.
type topoHead struct {
	X     int
	Label string
}

// topology is what the Topology page draws.
type topology struct {
	Width, Height int
	Heads         []topoHead
	// FenceX and FenceH place the band of fence 1.
	FenceX, FenceH, FenceTextX, FenceTextY int
	Nodes                                  []topoNode
	Edges                                  []topoEdge
	// More counts the endpoints left out.
	More int
	// Sel is the selected node, if any, with what it tells.
	Sel *topoDetail
}

// topoDetail is what the panel below the topology says about the selected
// node.
type topoDetail struct {
	Kind, Name, VM string
	// Workspace is that of a sandbox's or provider's node.
	Workspace string
	Node      topoNode
	// Gateway and Sandbox are those of a gateway's or sandbox's node.
	Gateway store.Gateway
	Sandbox store.Sandbox
	// Provider and Profile are those of a provider's node.
	Provider store.Provider
	Profile  *store.Profile
	// Denial is the latest denial of an endpoint's node; Allowed counts
	// its allowed requests in the last hour, and Providers names the
	// providers whose credentials go there.
	Denial    *trafficRow
	Allowed   int
	Providers []string
}

// topologyOf lays out the topology of the VM named vm, or of all, with the
// node sel selected.
func topologyOf(v *view, vm, sel string) topology {
	sn := v.Snap
	link := func(id string) string {
		q := url.Values{}
		if vm != "" {
			q.Set("vm", vm)
		}
		if id != sel {
			q.Set("sel", id)
		}
		if len(q) == 0 {
			return "/topology"
		}
		return "/topology?" + q.Encode()
	}
	var cols [5][]topoNode
	add := func(col int, n topoNode) {
		n.X, n.W = topoX[col], topoW[col]
		n.Y = topoTop + len(cols[col])*(topoNodeH+topoGap)
		n.Link = link(n.ID)
		cols[col] = append(cols[col], n)
	}
	var edges []topoEdge
	// edgeAt and hasIn index the edges: a sandbox's denials can fill the
	// log, and scanning the edges for each would be quadratic.
	edgeAt := map[[2]string]int{}
	hasIn := map[string]bool{}
	edge := func(from, to, class string) {
		if i, ok := edgeAt[[2]string{from, to}]; ok {
			// Denied outranks allowed, which outranks configured.
			if rank(class) > rank(edges[i].Class) {
				edges[i].Class = class
			}
			return
		}
		edgeAt[[2]string{from, to}] = len(edges)
		hasIn[to] = true
		edges = append(edges, topoEdge{From: from, To: to, Class: class})
	}

	host := cmp.Or(sn.Host.Name, "this host")
	hostSub := "openshell CLI"
	if sn.Host.OpenShellCLI != "" {
		hostSub += " " + sn.Host.OpenShellCLI
	}
	add(0, topoNode{ID: "host", Label: host, Sub: hostSub})

	type endpoint struct {
		denied, allowed bool
		last            time.Time
	}
	endpoints := map[string]*endpoint{}
	var epOrder []string
	addEndpoint := func(ep string) *endpoint {
		e := endpoints[ep]
		if e == nil {
			e = &endpoint{}
			endpoints[ep] = e
			epOrder = append(epOrder, ep)
		}
		return e
	}
	since := clock().Add(-recentWindow)
	for _, g := range sn.Gateways {
		if vm != "" && g.VM != vm {
			continue
		}
		vmv, _ := sn.VM(g.VM)
		gwID := "gw:" + g.VM
		sub := g.State
		if vmv.NetworkProfile != "" {
			sub += " · " + vmv.NetworkProfile
		}
		add(1, topoNode{ID: gwID, Label: g.Name(), Sub: sub, Off: g.State != store.GatewayReady})
		edge("host", gwID, "")
		for _, s := range g.Sandboxes {
			sbID := "sb:" + g.VM + "/" + s.Workspace + "/" + s.Name
			sub := s.Phase + " · " + g.VM
			if g.MultiWorkspace() {
				sub += " · " + s.Workspace
			}
			add(2, topoNode{ID: sbID, Label: s.Name, Sub: sub, Off: !s.Ready()})
			edge(gwID, sbID, "")
			for _, p := range s.Providers {
				prID := "pr:" + g.VM + "/" + s.Workspace + "/" + p
				class := ""
				if s.Ready() {
					class = "hot"
				}
				edge(sbID, prID, class)
			}
		}
		for _, p := range g.Providers {
			prID := "pr:" + g.VM + "/" + p.Workspace + "/" + p.Name
			if !hasIn[prID] {
				continue
			}
			sub := strings.Join(p.Credentials, ", ")
			if g.MultiWorkspace() {
				sub = cmp.Or(sub, "no credentials") + " · " + p.Workspace
			}
			add(3, topoNode{ID: prID, Label: p.Name, Sub: sub})
			if prof, ok := g.Profile(p.Workspace, p.Type); ok {
				for _, ep := range prof.Endpoints {
					addEndpoint(ep)
					edge(prID, "ep:"+ep, "")
				}
			}
		}
		for _, r := range sn.TrafficOf(g.VM).Requests {
			if r.Time.Before(since) || r.Host == "" || r.Verdict == "" {
				continue
			}
			ep := r.Endpoint()
			e := addEndpoint(ep)
			sbID := "sb:" + g.VM + "/" + r.Workspace + "/" + r.Sandbox
			if r.Denied() {
				e.denied = true
				edge(sbID, "ep:"+ep, "deny")
			} else {
				e.allowed = true
				edge(sbID, "ep:"+ep, "seen")
			}
			e.last = r.Time
		}
	}
	// Denied endpoints first, then the most recent.
	slices.SortStableFunc(epOrder, func(a, b string) int {
		ea, eb := endpoints[a], endpoints[b]
		if ea.denied != eb.denied {
			if ea.denied {
				return -1
			}
			return 1
		}
		return eb.last.Compare(ea.last)
	})
	more := 0
	if len(epOrder) > topoMaxEndpoints {
		more = len(epOrder) - topoMaxEndpoints
		epOrder = epOrder[:topoMaxEndpoints]
	}
	for _, ep := range epOrder {
		e := endpoints[ep]
		sub := "provider endpoint"
		switch {
		case e.denied:
			sub = "denied in the last hour"
		case e.allowed:
			sub = "reached in the last hour"
		}
		add(4, topoNode{ID: "ep:" + ep, Label: strings.TrimSuffix(ep, ":443"), Sub: sub, Bad: e.denied})
	}

	t := topology{Width: topoX[4] + topoW[4] + 20, More: more}
	byID := map[string]*topoNode{}
	for c := range cols {
		t.Heads = append(t.Heads, topoHead{X: topoX[c], Label: topoHeads[c]})
		for i := range cols[c] {
			n := &cols[c][i]
			byID[n.ID] = n
			t.Height = max(t.Height, n.Y+topoNodeH+30)
		}
	}
	t.Height = max(t.Height, topoTop+topoNodeH+30)
	t.FenceX, t.FenceH = topoX[3]-30, t.Height-40
	t.FenceTextX, t.FenceTextY = t.FenceX+9, t.Height-6

	near := map[string]bool{}
	if sel != "" {
		for _, e := range edges {
			switch sel {
			case e.From:
				near[e.To] = true
			case e.To:
				near[e.From] = true
			}
		}
	}
	touches := func(id string) bool { return sel == "" || id == sel || near[id] }
	if _, ok := byID[sel]; !ok {
		sel = ""
	}
	for _, e := range edges {
		a, b := byID[e.From], byID[e.To]
		if a == nil || b == nil {
			continue
		}
		x1, y1, x2, y2 := a.X+a.W, a.Y+topoNodeH/2, b.X, b.Y+topoNodeH/2
		mx := (x1 + x2) / 2
		e.D = fmt.Sprintf("M%d,%d C%d,%d %d,%d %d,%d", x1, y1, mx, y1, mx, y2, x2, y2)
		e.Dim = sel != "" && e.From != sel && e.To != sel
		t.Edges = append(t.Edges, e)
	}
	for c := range cols {
		for _, n := range cols[c] {
			n.Sel, n.Dim = n.ID == sel, !touches(n.ID)
			t.Nodes = append(t.Nodes, n)
		}
	}
	if sel != "" {
		t.Sel = topoDetailOf(v, *byID[sel], edges)
	}
	return t
}

// rank orders the classes of edges.
func rank(class string) int {
	switch class {
	case "deny":
		return 3
	case "seen":
		return 2
	case "hot":
		return 1
	}
	return 0
}

// topoDetailOf says what the panel below the topology shows for n.
func topoDetailOf(v *view, n topoNode, edges []topoEdge) *topoDetail {
	kind, rest, _ := strings.Cut(n.ID, ":")
	d := &topoDetail{Kind: kind, Name: rest, Node: n}
	sn := v.Snap
	switch kind {
	case "gw":
		d.VM = rest
		d.Gateway, _ = sn.Gateway(rest)
	case "sb", "pr":
		d.VM, rest, _ = strings.Cut(rest, "/")
		d.Workspace, d.Name, _ = strings.Cut(rest, "/")
		d.Gateway, _ = sn.Gateway(d.VM)
		if kind == "sb" {
			d.Sandbox, _ = d.Gateway.Sandbox(d.Workspace, d.Name)
			break
		}
		d.Provider, _ = d.Gateway.Provider(d.Workspace, d.Name)
		if p, ok := d.Gateway.Profile(d.Workspace, d.Provider.Type); ok {
			d.Profile = &p
		}
	case "ep":
		since := clock().Add(-recentWindow)
		for _, t := range sn.Traffic {
			for _, r := range t.Requests {
				if r.Time.Before(since) || r.Endpoint() != rest {
					continue
				}
				if r.Denied() {
					row := trafficRow{Request: r}
					if dr, ok := t.DraftFor(r); ok {
						row.Draft = &dr
					}
					d.Denial = &row
				} else if r.Verdict == store.VerdictAllow {
					d.Allowed++
				}
			}
		}
		for _, e := range edges {
			if e.To == n.ID && strings.HasPrefix(e.From, "pr:") {
				_, p, _ := strings.Cut(e.From, "/")
				_, p, _ = strings.Cut(p, "/")
				d.Providers = append(d.Providers, p)
			}
		}
	}
	return d
}
