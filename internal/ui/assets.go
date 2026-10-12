// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

package ui

import (
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"io/fs"
	"net/http"
	"os"
	"path"
	"strings"
)

// web holds the templates and the static files: CSS, JavaScript, htmx and
// the fonts, so that brig ui works offline and loads nothing from elsewhere.
//
//go:embed web
var web embed.FS

// assets serves the static files under /static/VERSION/, where VERSION is a
// hash of all of them: a new brig serves them under a new path, so browsers
// may cache them for good.
type assets struct {
	fs      fs.FS
	version string
	// dev serves files from disk, read again on each request, and has
	// browsers check them each time.
	dev bool
}

// newAssets returns the embedded files, or those in dir when dir is not
// empty (brig ui --dev-assets, for working on the templates).
func newAssets(dir string) (*assets, error) {
	if dir != "" {
		return &assets{fs: os.DirFS(dir), version: "dev", dev: true}, nil
	}
	sub, err := fs.Sub(web, "web")
	if err != nil {
		return nil, err
	}
	h := sha256.New()
	err = fs.WalkDir(sub, "static", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		data, err := fs.ReadFile(sub, p)
		if err != nil {
			return err
		}
		h.Write([]byte(p))
		h.Write([]byte{0})
		h.Write(data)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &assets{fs: sub, version: hex.EncodeToString(h.Sum(nil))[:12]}, nil
}

// url returns the path under which the static file name is served.
func (a *assets) url(name string) string {
	return "/static/" + a.version + "/" + name
}

// ServeHTTP serves GET /static/VERSION/NAME.
func (a *assets) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/static/")
	version, name, ok := strings.Cut(rest, "/")
	if !ok || version != a.version || name == "" || !fs.ValidPath(name) {
		http.NotFound(w, r)
		return
	}
	data, err := fs.ReadFile(a.fs, path.Join("static", name))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if a.dev {
		w.Header().Set("Cache-Control", "no-cache")
	} else {
		w.Header().Set("Cache-Control", "private, max-age=31536000, immutable")
	}
	if ct := contentType(name); ct != "" {
		w.Header().Set("Content-Type", ct)
	}
	_, _ = w.Write(data) //nolint:gosec // G705: an embedded file, served with its own type
}

// contentType returns the media type of a static file by its extension.
// The list is fixed, so the host's mime database plays no part.
func contentType(name string) string {
	switch path.Ext(name) {
	case ".css":
		return "text/css; charset=utf-8"
	case ".js":
		return "text/javascript; charset=utf-8"
	case ".woff2":
		return "font/woff2"
	case ".svg":
		return "image/svg+xml"
	case ".txt":
		return "text/plain; charset=utf-8"
	}
	return "application/octet-stream"
}
