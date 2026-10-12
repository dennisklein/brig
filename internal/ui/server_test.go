// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

package ui

import (
	"bufio"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dennisklein/brig/internal/ui/demo"
	"github.com/dennisklein/brig/internal/ui/store"
)

const testPort = 7878

// testServer is a Server with the demo's data and a logged-in session.
type testServer struct {
	t      *testing.T
	srv    *Server
	h      http.Handler
	st     *store.Store
	token  string
	cookie *http.Cookie
	csrf   string
	checks atomic.Int32
	starts *fakeStarter
	files  *fakeFiles
}

func newTestServer(t *testing.T) *testServer {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	st := store.New()
	now := time.Now()
	st.SetHost(store.Host{Name: "zeta", BrigVersion: "v0.6.0", OpenShellCLI: "0.1.2", Addr: "127.0.0.1:7878"})
	st.SetVMs(store.VMState{VMs: demo.VMs(now), Images: demo.Images(now), Profiles: demo.Profiles(), Config: demo.Config()})
	st.SetActivity(demo.Activity(now))
	for _, g := range demo.Gateways(now) {
		st.UpdateGateway(g.VM, func(gw *store.Gateway) bool { *gw = g; return true })
	}
	ts := &testServer{t: t, st: st, token: "the-token", starts: &fakeStarter{st: st}, files: &fakeFiles{}}
	srv, err := newServer(ctx, serverConfig{
		Store: st, Token: ts.token, Port: testPort, Home: "/home/dklein",
		CheckHost: func(context.Context, *store.Store) { ts.checks.Add(1) },
		Starter:   ts.starts,
		Files:     ts.files,
		Log:       slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	ts.srv, ts.h = srv, srv.Handler()
	return ts
}

// request builds a request to the server as a browser on 127.0.0.1 would.
func request(method, target string) *http.Request {
	r := httptest.NewRequest(method, "http://127.0.0.1:7878"+target, nil)
	r.Host = "127.0.0.1:7878"
	return r
}

func (ts *testServer) do(r *http.Request) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	ts.h.ServeHTTP(w, r)
	return w
}

// login exchanges the token for a session, as opening the printed URL does.
func (ts *testServer) login() {
	ts.t.Helper()
	w := ts.do(request("GET", "/vms?token="+ts.token))
	if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/vms" {
		ts.t.Fatalf("token exchange: %d, Location %q", w.Code, w.Header().Get("Location"))
	}
	for _, c := range w.Result().Cookies() {
		ts.cookie = c
	}
	if ts.cookie == nil || !ts.cookie.HttpOnly || ts.cookie.SameSite != http.SameSiteStrictMode || ts.cookie.Name != "brig_ui_7878" {
		ts.t.Fatalf("session cookie: %+v", ts.cookie)
	}
	ts.csrf = ts.srv.auth.sessions[ts.cookie.Value]
}

// get requests target in the session.
func (ts *testServer) get(target string) *httptest.ResponseRecorder {
	r := request("GET", target)
	r.AddCookie(ts.cookie)
	return ts.do(r)
}

func TestTokenExchange(t *testing.T) {
	ts := newTestServer(t)
	if w := ts.do(request("GET", "/")); w.Code != http.StatusForbidden {
		t.Errorf("without a session: %d, want 403", w.Code)
	}
	if w := ts.do(request("GET", "/?token=wrong")); w.Code != http.StatusForbidden || len(w.Result().Cookies()) != 0 {
		t.Errorf("wrong token: %d, cookies %v", w.Code, w.Result().Cookies())
	}
	ts.login()
	if w := ts.get("/"); w.Code != http.StatusOK {
		t.Errorf("with the session: %d", w.Code)
	}
	r := request("GET", "/")
	r.AddCookie(&http.Cookie{Name: ts.cookie.Name, Value: "made-up"}) //nolint:gosec // G124: a forged cookie
	if w := ts.do(r); w.Code != http.StatusForbidden {
		t.Errorf("unknown session: %d, want 403", w.Code)
	}
}

func TestTokenExchangeKeepsQuery(t *testing.T) {
	ts := newTestServer(t)
	w := ts.do(request("GET", "/vms/dev?tab=mounts&token="+ts.token))
	if loc := w.Header().Get("Location"); loc != "/vms/dev?tab=mounts" {
		t.Errorf("Location %q", loc)
	}
}

func TestHostHeader(t *testing.T) {
	ts := newTestServer(t)
	ts.login()
	for host, want := range map[string]int{
		"127.0.0.1:7878":      http.StatusOK,
		"localhost:7878":      http.StatusOK,
		"rebind.example:7878": http.StatusMisdirectedRequest,
		"127.0.0.1:7879":      http.StatusMisdirectedRequest,
		"127.0.0.1":           http.StatusMisdirectedRequest,
	} {
		r := request("GET", "/")
		r.Host = host
		r.AddCookie(ts.cookie)
		if w := ts.do(r); w.Code != want {
			t.Errorf("Host %s: %d, want %d", host, w.Code, want)
		}
	}
	// Even the token exchange needs our host name.
	r := request("GET", "/?token="+ts.token)
	r.Host = "rebind.example:7878"
	if w := ts.do(r); w.Code != http.StatusMisdirectedRequest {
		t.Errorf("token exchange on another host: %d", w.Code)
	}
}

func TestPostNeedsOriginAndCSRF(t *testing.T) {
	ts := newTestServer(t)
	ts.login()
	post := func(origin, csrf string) int {
		r := request("POST", "/host/check")
		r.AddCookie(ts.cookie)
		if origin != "" {
			r.Header.Set("Origin", origin)
		}
		if csrf != "" {
			r.Header.Set(csrfHeader, csrf)
		}
		return ts.do(r).Code
	}
	for _, c := range []struct {
		name, origin, csrf string
		want               int
	}{
		{"no origin", "", ts.csrf, http.StatusForbidden},
		{"other origin", "http://evil.example", ts.csrf, http.StatusForbidden},
		{"other port", "http://127.0.0.1:7879", ts.csrf, http.StatusForbidden},
		{"no csrf", "http://127.0.0.1:7878", "", http.StatusForbidden},
		{"wrong csrf", "http://127.0.0.1:7878", "nope", http.StatusForbidden},
		{"ok", "http://127.0.0.1:7878", ts.csrf, http.StatusOK},
	} {
		if got := post(c.origin, c.csrf); got != c.want {
			t.Errorf("%s: %d, want %d", c.name, got, c.want)
		}
	}
	deadline := time.Now().Add(time.Second)
	for ts.srv.checking.Load() && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if n := ts.checks.Load(); n != 1 {
		t.Errorf("host checks ran %d times, want 1", n)
	}
	// A POST with the token does not log in.
	r := request("POST", "/host/check?token="+ts.token)
	r.Header.Set("Origin", "http://127.0.0.1:7878")
	if w := ts.do(r); w.Code != http.StatusForbidden {
		t.Errorf("POST with the token: %d", w.Code)
	}
}

func TestSecurityHeaders(t *testing.T) {
	ts := newTestServer(t)
	ts.login()
	for _, target := range []string{"/", "/nope"} {
		w := ts.get(target)
		h := w.Header()
		for k, want := range map[string]string{
			"Content-Security-Policy": contentSecurityPolicy,
			"Referrer-Policy":         "no-referrer",
			"X-Content-Type-Options":  "nosniff",
			"X-Frame-Options":         "DENY",
			"Cache-Control":           "no-store",
		} {
			if got := h.Get(k); got != want {
				t.Errorf("%s: %s = %q, want %q", target, k, got, want)
			}
		}
		if h.Get("Access-Control-Allow-Origin") != "" {
			t.Errorf("%s allows CORS", target)
		}
	}
	// Not even a refused request goes without them.
	if w := ts.do(request("GET", "/")); w.Header().Get("Content-Security-Policy") == "" {
		t.Error("403 without CSP")
	}
}

func TestPages(t *testing.T) {
	ts := newTestServer(t)
	ts.login()
	for target, want := range map[string]string{
		"/":                         "Needs you",
		"/vms":                      "brig-review",
		"/vms/dev":                  "f44-openshell0.1.2-20261010T091500Z",
		"/vms/dev?tab=mounts":       "~/src/pi-kit",
		"/vms/review?tab=openshell": "secret-tool found no secret",
		"/vms/dev?tab=network":      "Host ports 11434",
		"/vms/dev?tab=shell":        "brig ssh dev --root",
		"/vms/dev?tab=bogus":        "Image and gateway",
		"/images":                   "newest",
		"/host":                     "Check again",
		"/activity":                 "brig start review",
		"/palette?q=rev":            "/vms/review",
	} {
		w := ts.get(target)
		if w.Code != http.StatusOK {
			t.Errorf("%s: %d", target, w.Code)
			continue
		}
		if body := w.Body.String(); !strings.Contains(body, want) {
			t.Errorf("%s lacks %q", target, want)
		}
	}
	for _, target := range []string{"/vms/nope", "/vms/NOT..valid", "/nope"} {
		if w := ts.get(target); w.Code != http.StatusNotFound {
			t.Errorf("%s: %d, want 404", target, w.Code)
		}
	}
}

func TestPagesEscapeUntrustedText(t *testing.T) {
	ts := newTestServer(t)
	ts.login()
	sn := ts.st.Snapshot()
	vms := append([]store.VM(nil), sn.VMs...)
	vms[1].LastSync = &store.SyncRecord{Time: time.Now(), Error: `<script>alert(1)</script>`}
	ts.st.SetVMs(store.VMState{VMs: vms, Images: sn.Images, Profiles: sn.Profiles, Config: sn.Config})
	body := ts.get("/").Body.String()
	if strings.Contains(body, "<script>alert") || !strings.Contains(body, "&lt;script&gt;alert") {
		t.Error("an error from the guest was not escaped")
	}
}

func TestHostMemoryIsMarked(t *testing.T) {
	ts := newTestServer(t)
	ts.login()
	sn := ts.st.Snapshot()
	vms := append([]store.VM(nil), sn.VMs...)
	st := *vms[0].Stats
	st.MemoryFromHost = true
	vms[0].Stats = &st
	ts.st.SetVMs(store.VMState{VMs: vms, Images: sn.Images, Profiles: sn.Profiles, Config: sn.Config})
	body := ts.get("/").Body.String()
	if !strings.Contains(body, "≈63%") || strings.Count(body, "≈") != 1 {
		t.Error("memory as the host sees it is not marked, or not only for that VM")
	}
}

func TestPageCSRFHeader(t *testing.T) {
	ts := newTestServer(t)
	ts.login()
	body := ts.get("/host").Body.String()
	if !strings.Contains(body, `hx-headers='{"X-Brig-CSRF": "`+ts.csrf+`"}'`) {
		t.Error("the page does not hand htmx the CSRF token")
	}
}

func TestStaticFiles(t *testing.T) {
	ts := newTestServer(t)
	ts.login()
	url := ts.srv.assets.url("app.css")
	w := ts.get(url)
	if w.Code != http.StatusOK || !strings.HasPrefix(w.Header().Get("Content-Type"), "text/css") {
		t.Fatalf("%s: %d %s", url, w.Code, w.Header().Get("Content-Type"))
	}
	if !strings.Contains(w.Header().Get("Cache-Control"), "immutable") {
		t.Errorf("Cache-Control %q", w.Header().Get("Cache-Control"))
	}
	for _, target := range []string{
		"/static/0123456789ab/app.css",
		"/static/" + ts.srv.assets.version + "/nope.js",
	} {
		if w := ts.get(target); w.Code != http.StatusNotFound {
			t.Errorf("%s: %d, want 404", target, w.Code)
		}
	}
	// The mux cleans the path and redirects; the templates stay unserved.
	if w := ts.get("/static/" + ts.srv.assets.version + "/../templates/layout.tmpl"); w.Code == http.StatusOK || strings.Contains(w.Body.String(), "define") {
		t.Errorf("a template was served: %d", w.Code)
	}
	if w := ts.get(ts.srv.assets.url("fonts/ibm-plex-sans-latin-400-normal.woff2")); w.Code != http.StatusOK || w.Header().Get("Content-Type") != "font/woff2" {
		t.Errorf("font: %d %s", w.Code, w.Header().Get("Content-Type"))
	}
}

func TestEvents(t *testing.T) {
	ts := newTestServer(t)
	ts.login()
	srv := httptest.NewServer(ts.h)
	defer srv.Close()

	for _, target := range []string{"/events", "/events?f=nope", "/events?f=vm&vm=Bad..Name"} {
		if w := ts.get(target); w.Code != http.StatusBadRequest {
			t.Errorf("%s: %d, want 400", target, w.Code)
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", srv.URL+"/events?f=vm-table&f=host-checks", nil)
	req.Host = "127.0.0.1:7878"
	req.AddCookie(ts.cookie)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("Content-Type %q", ct)
	}
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(nil, 1<<20)
	next := func() (string, string) {
		var name string
		var data []string
		for sc.Scan() {
			line := sc.Text()
			switch {
			case line == "" && name != "":
				return name, strings.Join(data, "\n")
			case strings.HasPrefix(line, "event: "):
				name = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "data: "):
				data = append(data, strings.TrimPrefix(line, "data: "))
			}
		}
		t.Fatalf("stream ended: %v", sc.Err())
		return "", ""
	}

	// A change to the VMs sends the table, and only the table.
	sn := ts.st.Snapshot()
	vms := append([]store.VM(nil), sn.VMs...)
	vms[2].State = "running"
	vms[2].Name = "airgap2"
	ts.st.SetVMs(store.VMState{VMs: vms, Images: sn.Images, Profiles: sn.Profiles, Config: sn.Config})
	name, data := next()
	if name != "vm-table" || !strings.Contains(data, "airgap2") {
		t.Errorf("got event %q: %.200s", name, data)
	}
	ts.st.SetDoctor(&store.DoctorReport{Running: true})
	if name, data = next(); name != "host-checks" || !strings.Contains(data, "Checking") {
		t.Errorf("got event %q: %.200s", name, data)
	}
}

func TestWriteEvent(t *testing.T) {
	var b strings.Builder
	if err := writeEvent(&b, "rail", "<a>\r\nevent: forged\rdata: x</a>\n"); err != nil {
		t.Fatal(err)
	}
	want := "event: rail\ndata: <a>&#13;\ndata: event: forged&#13;data: x</a>\ndata: \n\n"
	if b.String() != want {
		t.Errorf("got %q\nwant %q", b.String(), want)
	}
}
