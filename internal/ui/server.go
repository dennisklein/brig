// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

package ui

import (
	"context"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"sync/atomic"

	"github.com/dennisklein/brig/internal/audit"
	"github.com/dennisklein/brig/internal/help"
	"github.com/dennisklein/brig/internal/ui/run"
	"github.com/dennisklein/brig/internal/ui/store"
	"github.com/dennisklein/brig/internal/vm"
)

// contentSecurityPolicy allows only brig ui's own files: no inline script
// or style, no other origin, no framing.
const contentSecurityPolicy = "default-src 'none'; script-src 'self'; style-src 'self'; img-src 'self' data:; " +
	"font-src 'self'; connect-src 'self'; form-action 'self'; base-uri 'none'; frame-ancestors 'none'"

// Server serves brig ui's pages and live updates for one store.
type Server struct {
	st        *store.Store
	auth      *auth
	port      int
	tmpl      *templates
	assets    *assets
	help      *help.Help
	demo      bool
	log       *slog.Logger
	checkHost func(context.Context, *store.Store)
	checking  atomic.Bool
	starter   run.Starter
	files     FileLister
	// history reads the audit trail; nil where there is none.
	history audit.Reader
	home    string
	// ctx ends the live update streams when the server stops.
	ctx context.Context
	// goRun starts a goroutine that the owner of the server waits for.
	goRun func(func())
	mux   *http.ServeMux
}

// serverConfig configures a Server.
type serverConfig struct {
	Store *store.Store
	Token string
	Port  int
	Demo  bool
	// Home is shown as ~ in paths.
	Home string
	// DevAssets serves templates and static files from this directory.
	DevAssets string
	// CheckHost runs the host checks into the store.
	CheckHost func(context.Context, *store.Store)
	// Starter runs the commands that dialogs make.
	Starter run.Starter
	// Files lists directories in sandboxes; nil lists none.
	Files FileLister
	// History reads the audit trail; nil reads none.
	History audit.Reader
	Log     *slog.Logger
	// Go starts the goroutines of the server, which end with the context.
	// Nil starts them without a wait.
	Go func(func())
}

func newServer(ctx context.Context, c serverConfig) (*Server, error) {
	a, err := newAssets(c.DevAssets)
	if err != nil {
		return nil, err
	}
	h, err := help.Load()
	if err != nil {
		return nil, err
	}
	fm := funcs(c.Home)
	for name, fn := range helpFuncs(h) {
		fm[name] = fn
	}
	t, err := newTemplates(a.fs, fm, a.dev)
	if err != nil {
		return nil, err
	}
	goRun := c.Go
	if goRun == nil {
		goRun = func(f func()) { go f() }
	}
	s := &Server{
		goRun: goRun,
		st:    c.Store, auth: newAuth(c.Token, c.Port), port: c.Port, tmpl: t, assets: a, help: h,
		demo: c.Demo, log: c.Log, checkHost: c.CheckHost, starter: c.Starter, files: c.Files, history: c.History, home: c.Home, ctx: ctx, mux: http.NewServeMux(),
	}
	s.mux.Handle("GET /static/", a)
	s.mux.HandleFunc("GET /{$}", s.overview)
	s.mux.HandleFunc("GET /vms", s.vms)
	s.mux.HandleFunc("GET /vms/{name}", s.vm)
	s.mux.HandleFunc("GET /images", s.images)
	s.mux.HandleFunc("GET /host", s.host)
	s.mux.HandleFunc("POST /host/check", s.check)
	s.mux.HandleFunc("GET /activity", s.activity)
	s.mux.HandleFunc("GET /sandboxes", s.sandboxes)
	s.mux.HandleFunc("GET /sandboxes/{vm}/{workspace}/{name}", s.sandbox)
	s.mux.HandleFunc("GET /sandboxes/{vm}/{workspace}/{name}/files", s.sandboxFiles)
	s.mux.HandleFunc("GET /sandboxes/{vm}/{workspace}/{name}/history", s.sandboxHistory)
	s.mux.HandleFunc("GET /gateway", s.firstGateway)
	s.mux.HandleFunc("GET /gateways/{vm}", s.gateway)
	s.mux.HandleFunc("GET /traffic", s.traffic)
	s.mux.HandleFunc("GET /topology", s.topology)
	s.mux.HandleFunc("GET /palette", s.palette)
	s.mux.HandleFunc("GET /help", s.helpPane)
	s.mux.HandleFunc("GET /help/search", s.helpSearch)
	s.mux.HandleFunc("GET /help/all", s.helpAll)
	s.mux.HandleFunc("GET /help/{kind}/{id}", s.helpEntry)
	s.mux.HandleFunc("GET /help/hint/{kind}/{id}", s.helpEntry)
	s.mux.HandleFunc("GET /terminal", s.terminal)
	s.mux.HandleFunc("GET /a/{id}", s.dialog)
	s.mux.HandleFunc("POST /a/{id}/preview", s.preview)
	s.mux.HandleFunc("POST /a/{id}/run", s.runAction)
	s.mux.HandleFunc("POST /tasks/{id}/dismiss", s.dismiss)
	s.mux.HandleFunc("GET /events", s.events)
	s.mux.HandleFunc("/", s.notFound)
	return s, nil
}

// Handler returns the server's handler with every check in front of it.
func (s *Server) Handler() http.Handler {
	return s.headers(s.checkHostHeader(s.auth.middleware(s.mux)))
}

// headers sets the security headers of every response.
func (s *Server) headers(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", contentSecurityPolicy)
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Cross-Origin-Opener-Policy", "same-origin")
		h.Set("Cross-Origin-Resource-Policy", "same-origin")
		h.Set("Cache-Control", "no-store")
		next.ServeHTTP(w, r)
	})
}

// checkHostHeader refuses requests for any host name but ours, so that a
// web page whose name resolves to 127.0.0.1 (DNS rebinding) cannot read
// brig ui as its own origin. Not even localhost is ours: a browser may
// resolve it to ::1, where another user's process can listen on our port.
func (s *Server) checkHostHeader(next http.Handler) http.Handler {
	host := "127.0.0.1:" + strconv.Itoa(s.port)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Host != host {
			http.Error(w, "brig ui answers only to "+host, http.StatusMisdirectedRequest)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// csrf context helpers.

func withCSRF(ctx context.Context, csrf string) context.Context {
	return context.WithValue(ctx, csrfKey{}, csrf)
}

func csrfFrom(ctx context.Context) string {
	s, _ := ctx.Value(csrfKey{}).(string)
	return s
}

// newView returns the view of a request, from the current snapshot.
func (s *Server) newView(r *http.Request, page, title string) *view {
	return &view{
		Page: page, Title: title, Snap: s.st.Snapshot(), Now: clock(), Demo: s.demo,
		CSRF: csrfFrom(r.Context()), Asset: s.assets.url,
	}
}

// render writes a page, or a server error.
func (s *Server) render(w http.ResponseWriter, status int, name string, v *view) {
	v.HelpPage = name
	html, err := s.tmpl.page(name, v)
	if err != nil {
		s.log.Error("rendering page", "page", name, "err", err)
		http.Error(w, "brig ui could not render this page; its log says why", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write(html) //nolint:gosec // G705: html/template output, escaped in context
}

func (s *Server) overview(w http.ResponseWriter, r *http.Request) {
	v := s.newView(r, "overview", "Overview")
	v.Events = eventsURL([]string{"rail", "overview-sub", "vm-cards", "needs", "recent-activity"}, nil)
	s.render(w, http.StatusOK, "overview", v)
}

func (s *Server) vms(w http.ResponseWriter, r *http.Request) {
	v := s.newView(r, "vms", "VMs")
	v.Events = eventsURL([]string{"rail", "vm-table"}, nil)
	s.render(w, http.StatusOK, "vms", v)
}

func (s *Server) vm(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	v := s.newView(r, "vms", name)
	if vm.ValidateName(name) != nil {
		s.notFound(w, r)
		return
	}
	var ok bool
	if v.VM, ok = v.Snap.VM(name); !ok {
		s.notFound(w, r)
		return
	}
	v.Tab = tab(r.URL.Query().Get("tab"))
	v.Events = eventsURL([]string{"rail", "vm"}, url.Values{"vm": {name}, "tab": {v.Tab}})
	s.render(w, http.StatusOK, "vm", v)
}

// tab returns the VM tab named key, or the first.
func tab(key string) string {
	for _, t := range vmTabs {
		if t.Key == key {
			return key
		}
	}
	return vmTabs[0].Key
}

func (s *Server) images(w http.ResponseWriter, r *http.Request) {
	v := s.newView(r, "images", "Images")
	v.Events = eventsURL([]string{"rail", "images"}, nil)
	s.render(w, http.StatusOK, "images", v)
}

func (s *Server) host(w http.ResponseWriter, r *http.Request) {
	// A GET changes nothing. The first visit has no checks to show, and
	// the page asks for them with a POST.
	v := s.newView(r, "host", "Host")
	v.Events = eventsURL([]string{"rail", "host-checks", "host-audit"}, nil)
	s.render(w, http.StatusOK, "host", v)
}

// check runs the host checks again. They change nothing on the host but
// for a VM network they start and stop, so it is a POST.
func (s *Server) check(w http.ResponseWriter, r *http.Request) {
	s.startCheck()
	// The live update brings the result; the answer shows them running.
	html, err := s.tmpl.fragment("host-checks", s.newView(r, "host", "Host"))
	if err != nil {
		s.log.Error("rendering host checks", "err", err)
		http.Error(w, "rendering failed", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write([]byte(html)) //nolint:gosec // G705: html/template output, escaped in context
}

// startCheck runs the host checks in the background unless they run. The
// store keeps the previous outcome meanwhile, marked as running, so that a
// page rendered now says so.
func (s *Server) startCheck() {
	if !s.checking.CompareAndSwap(false, true) {
		return
	}
	running := &store.DoctorReport{}
	if prev := s.st.Snapshot().Doctor; prev != nil {
		*running = *prev
	}
	running.Running = true
	s.st.SetDoctor(running)
	s.goRun(func() {
		defer s.checking.Store(false)
		s.checkHost(s.ctx, s.st)
	})
}

func (s *Server) activity(w http.ResponseWriter, r *http.Request) {
	v := s.newView(r, "activity", "Activity")
	v.Events = eventsURL([]string{"rail", "activity-list"}, nil)
	s.render(w, http.StatusOK, "activity", v)
}

func (s *Server) palette(w http.ResponseWriter, r *http.Request) {
	v := s.newView(r, "", "")
	v.Query = r.URL.Query().Get("q")
	html, err := s.tmpl.fragment("palette-list", v)
	if err != nil {
		s.log.Error("rendering palette", "err", err)
		http.Error(w, "rendering failed", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write([]byte(html)) //nolint:gosec // G705: html/template output, escaped in context
}

func (s *Server) notFound(w http.ResponseWriter, r *http.Request) {
	v := s.newView(r, "", "Not found")
	v.Events = eventsURL([]string{"rail"}, nil)
	s.render(w, http.StatusNotFound, "notfound", v)
}

// listen opens brig ui's listener on 127.0.0.1: on port, or a free port if
// port is 0. Only the user's own processes get through it.
func listen(port int, log *slog.Logger) (net.Listener, error) {
	l, err := net.Listen("tcp4", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	if err != nil {
		return nil, err
	}
	return &peerListener{Listener: l, uid: os.Getuid(), owner: procNetTCPOwner, refused: func(err error) {
		log.Warn("closed a connection", "err", err)
	}}, nil
}
