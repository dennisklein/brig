// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

package ui

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"net/http"
	"net/url"
	"strconv"
	"sync"
)

// csrfHeader carries the session's CSRF token on every request that
// changes something. htmx sends it from the hx-headers of <body>.
const csrfHeader = "X-Brig-CSRF"

// newSecret returns 32 random bytes, URL-safe.
func newSecret() string {
	var b [32]byte
	// crypto/rand never fails; it ends the process when it cannot.
	_, _ = rand.Read(b[:])
	return base64.RawURLEncoding.EncodeToString(b[:])
}

// equalSecret compares secrets in constant time.
func equalSecret(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

// auth lets in only browsers that opened the URL with the token: the token
// is exchanged once for a session cookie, and each session has a CSRF token
// that changing requests must send back.
type auth struct {
	token string
	// cookie is the session cookie's name. Cookies do not tell ports
	// apart, so it names the port: another brig ui on another port keeps
	// its own session.
	cookie string

	mu sync.Mutex
	// sessions maps session IDs to their CSRF tokens.
	sessions map[string]string
}

func newAuth(token string, port int) *auth {
	return &auth{token: token, cookie: "brig_ui_" + strconv.Itoa(port), sessions: map[string]string{}}
}

// session returns the CSRF token of the request's session, if it has one.
func (a *auth) session(r *http.Request) (string, bool) {
	c, err := r.Cookie(a.cookie)
	if err != nil {
		return "", false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	csrf, ok := a.sessions[c.Value]
	return csrf, ok
}

// login starts a session and sets its cookie on w.
func (a *auth) login(w http.ResponseWriter) {
	id, csrf := newSecret(), newSecret()
	a.mu.Lock()
	a.sessions[id] = csrf
	a.mu.Unlock()
	// Secure would keep the cookie from plain HTTP, which is all a
	// loopback listener without a certificate speaks.
	http.SetCookie(w, &http.Cookie{ //nolint:gosec // G124: see above
		Name: a.cookie, Value: id, Path: "/",
		HttpOnly: true, SameSite: http.SameSiteStrictMode,
	})
}

// csrfKey is the context key of the request's CSRF token.
type csrfKey struct{}

// middleware runs next for requests of a session. A GET with the token in
// its query starts a session and redirects to the same URL without the
// token, so that it leaves the address bar and the history. Requests that
// may change something must come from our own origin and carry the
// session's CSRF token.
func (a *auth) middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if tok := r.URL.Query().Get("token"); tok != "" && (r.Method == http.MethodGet || r.Method == http.MethodHead) {
			if !equalSecret(tok, a.token) {
				forbidden(w, "This link holds an old or wrong token.")
				return
			}
			a.login(w)
			u := *r.URL
			q := u.Query()
			q.Del("token")
			u.RawQuery = q.Encode()
			http.Redirect(w, r, (&url.URL{Path: u.Path, RawQuery: u.RawQuery}).String(), http.StatusSeeOther)
			return
		}
		csrf, ok := a.session(r)
		if !ok {
			forbidden(w, "")
			return
		}
		if !safeMethod(r.Method) {
			if r.Header.Get("Origin") != "http://"+r.Host {
				forbidden(w, "The request came from another site.")
				return
			}
			if !equalSecret(r.Header.Get(csrfHeader), csrf) {
				forbidden(w, "The request lacks this page's CSRF token. Reload the page.")
				return
			}
		}
		next.ServeHTTP(w, r.WithContext(withCSRF(r.Context(), csrf)))
	})
}

// safeMethod reports whether requests of method m never change state.
func safeMethod(m string) bool {
	return m == http.MethodGet || m == http.MethodHead
}

// forbidden answers a request that is not let in. It never says what the
// token is or where to find it, only that the terminal has it.
func forbidden(w http.ResponseWriter, why string) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusForbidden)
	if why != "" {
		why += "\n"
	}
	_, _ = w.Write([]byte(why + "Open the address that brig ui printed in your terminal, or run brig ui again.\n"))
}
