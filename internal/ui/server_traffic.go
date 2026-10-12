// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

package ui

import (
	"net/http"
	"net/url"
	"regexp"

	"github.com/dennisklein/brig/internal/ui/action"
	"github.com/dennisklein/brig/internal/vm"
)

// topoIDRE matches the IDs of the topology's nodes.
var topoIDRE = regexp.MustCompile(`^(host|gw:[a-z0-9-]+|(sb|pr):[a-z0-9-]+/[A-Za-z0-9._-]+/[A-Za-z0-9._-]+|ep:[A-Za-z0-9*._-]+(:[0-9]+)?)$`)

// trafficQueryOf reads the Traffic page's filters from a query: verdict,
// vm, workspace and sandbox. It drops those that are not valid. A sandbox
// needs its VM and workspace to be told from one of the same name.
func trafficQueryOf(q url.Values) trafficQuery {
	t := trafficQuery{Denied: q.Get("verdict") == "deny"}
	if n := q.Get("vm"); n != "" && vm.ValidateName(n) == nil {
		t.VM = n
	}
	if ws := q.Get("workspace"); action.ValidWorkspaceName(ws) {
		t.Workspace = ws
	}
	if sb := q.Get("sandbox"); t.VM != "" && t.Workspace != "" && action.ValidSandboxName(sb) {
		t.Sandbox = sb
	}
	return t
}

// traffic shows the requests of every sandbox, or of those the query picks.
func (s *Server) traffic(w http.ResponseWriter, r *http.Request) {
	v := s.newView(r, "traffic", "Traffic")
	v.TrafficQ = trafficQueryOf(r.URL.Query())
	params := url.Values{"vm": {v.TrafficQ.VM}, "workspace": {v.TrafficQ.Workspace}, "sandbox": {v.TrafficQ.Sandbox}}
	if v.TrafficQ.Denied {
		params.Set("verdict", "deny")
	}
	v.Events = eventsURL([]string{"rail", "traffic-table"}, params)
	s.render(w, http.StatusOK, "traffic", v)
}

// topology draws the VMs' gateways, sandboxes, providers and endpoints, or
// those of the VM in the query.
func (s *Server) topology(w http.ResponseWriter, r *http.Request) {
	v := s.newView(r, "topology", "Topology")
	q := r.URL.Query()
	if f := q.Get("vm"); f != "" && vm.ValidateName(f) == nil {
		v.Filter = f
	}
	if sel := q.Get("sel"); topoIDRE.MatchString(sel) {
		v.Sel = sel
	}
	v.Events = eventsURL([]string{"rail", "topology"}, url.Values{"filter": {v.Filter}, "sel": {v.Sel}})
	s.render(w, http.StatusOK, "topology", v)
}
