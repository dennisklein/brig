// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

package ui

import (
	"net/http"
	"net/url"

	"github.com/dennisklein/brig/internal/ui/action"
	"github.com/dennisklein/brig/internal/vm"
)

// sandboxes lists the sandboxes of every VM, or of the VM and the workspace
// in the query.
func (s *Server) sandboxes(w http.ResponseWriter, r *http.Request) {
	v := s.newView(r, "sandboxes", "Sandboxes")
	q := r.URL.Query()
	if f := q.Get("vm"); f != "" && vm.ValidateName(f) == nil {
		v.Filter = f
	}
	if f := q.Get("workspace"); action.ValidWorkspaceName(f) {
		v.WSFilter = f
	}
	v.Events = eventsURL([]string{"rail", "sandbox-table"}, sandboxFilterQuery(v.Filter, v.WSFilter))
	s.render(w, http.StatusOK, "sandboxes", v)
}

// sandbox shows a sandbox of a workspace of a VM's gateway.
func (s *Server) sandbox(w http.ResponseWriter, r *http.Request) {
	vmName, ws, name := r.PathValue("vm"), r.PathValue("workspace"), r.PathValue("name")
	if vm.ValidateName(vmName) != nil || !action.ValidWorkspaceName(ws) || !action.ValidSandboxName(name) {
		s.notFound(w, r)
		return
	}
	v := s.newView(r, "sandboxes", name)
	var ok bool
	if v.Gateway, ok = v.Snap.Gateway(vmName); !ok {
		s.notFound(w, r)
		return
	}
	if v.Sandbox, ok = v.Gateway.Sandbox(ws, name); !ok {
		s.notFound(w, r)
		return
	}
	v.Tab = pickTab(sandboxTabs, r.URL.Query().Get("tab"))
	v.Path = filesPath(r.URL.Query().Get("path"))
	names := []string{"rail", "sandbox"}
	if v.Tab == "traffic" {
		names = append(names, "sandbox-traffic")
	}
	v.Events = eventsURL(names, url.Values{"gw": {vmName}, "ws": {ws}, "sb": {name}, "tab": {v.Tab}, "path": {v.Path}})
	s.render(w, http.StatusOK, "sandbox", v)
}

// firstGateway sends the browser to the gateway that the Gateway page
// shows when none is named.
func (s *Server) firstGateway(w http.ResponseWriter, r *http.Request) {
	g, ok := firstGateway(s.st.Snapshot())
	if !ok {
		v := s.newView(r, "gateway", "Gateway")
		s.render(w, http.StatusOK, "gateway", v)
		return
	}
	http.Redirect(w, r, "/gateways/"+g.VM, http.StatusSeeOther)
}

// gateway shows a VM's gateway.
func (s *Server) gateway(w http.ResponseWriter, r *http.Request) {
	vmName := r.PathValue("vm")
	if vm.ValidateName(vmName) != nil {
		s.notFound(w, r)
		return
	}
	v := s.newView(r, "gateway", "brig-"+vmName)
	var ok bool
	if v.Gateway, ok = v.Snap.Gateway(vmName); !ok {
		s.notFound(w, r)
		return
	}
	v.VM, _ = v.Snap.VM(vmName)
	v.Tab = pickTab(gatewayTabs, r.URL.Query().Get("tab"))
	v.Events = eventsURL([]string{"rail", "gateway"}, url.Values{"gw": {vmName}, "tab": {v.Tab}})
	s.render(w, http.StatusOK, "gateway", v)
}

// liveOpenShell sets what the OpenShell fragments of a live update need,
// from its query: the gateway (gw), its sandbox (sb) in the workspace (ws),
// their tab (tab), the Sandboxes page's VM and workspace filters (filter,
// wsfilter), the selected node of the topology (sel) and the Traffic page's
// filters, checked by checkOpenShellQuery.
func liveOpenShell(v *view, q url.Values) {
	v.Filter = q.Get("filter")
	v.WSFilter = q.Get("wsfilter")
	v.Sel = q.Get("sel")
	v.TrafficQ = trafficQueryOf(q)
	gw := q.Get("gw")
	if gw == "" {
		return
	}
	v.Gateway, _ = v.Snap.Gateway(gw)
	v.VM, _ = v.Snap.VM(gw)
	if sb := q.Get("sb"); sb != "" {
		v.Sandbox, _ = v.Gateway.Sandbox(q.Get("ws"), sb)
		v.Tab = pickTab(sandboxTabs, q.Get("tab"))
		v.Path = filesPath(q.Get("path"))
		return
	}
	v.Tab = pickTab(gatewayTabs, q.Get("tab"))
}

// checkOpenShellQuery checks the OpenShell parameters of a live update's
// query.
func checkOpenShellQuery(q url.Values) bool {
	for _, k := range []string{"gw", "filter"} {
		if n := q.Get(k); n != "" && vm.ValidateName(n) != nil {
			return false
		}
	}
	if p := q.Get("path"); p != "" {
		if _, ok := workspacePath(p); !ok {
			return false
		}
	}
	if sel := q.Get("sel"); sel != "" && !topoIDRE.MatchString(sel) {
		return false
	}
	if ws := q.Get("wsfilter"); ws != "" && !action.ValidWorkspaceName(ws) {
		return false
	}
	sb := q.Get("sb")
	return sb == "" || (q.Get("gw") != "" && action.ValidWorkspaceName(q.Get("ws")) && action.ValidSandboxName(sb))
}
