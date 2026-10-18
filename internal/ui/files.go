// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

package ui

import (
	"context"
	"net/http"
	"net/url"
	"path"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/GSI-HPC/go-clikit/termtext"

	"github.com/dennisklein/brig/internal/osclient"
	"github.com/dennisklein/brig/internal/ui/action"
	"github.com/dennisklein/brig/internal/ui/store"
	"github.com/dennisklein/brig/internal/vm"
)

// sandboxDir is the directory of a sandbox that the Files tab shows. Its
// listings stay inside it.
const sandboxDir = "/sandbox"

// maxPath bounds the path of a listing.
const maxPath = 1024

// FileLister lists a directory in a sandbox.
type FileLister interface {
	ListFiles(ctx context.Context, vm, workspace, sandbox, dir string) (store.Dir, error)
}

// workspacePath returns p as a clean path in the workspace, and whether it
// is one: absolute, without control characters, in /sandbox.
func workspacePath(p string) (string, bool) {
	if p == "" || len(p) > maxPath || !path.IsAbs(p) || strings.ContainsFunc(p, func(r rune) bool { return r < ' ' || r == 0x7f }) {
		return "", false
	}
	p = path.Clean(p)
	return p, p == sandboxDir || strings.HasPrefix(p, sandboxDir+"/")
}

// filesPath returns the directory that a page's query names, else the
// workspace.
func filesPath(q string) string {
	if p, ok := workspacePath(q); ok {
		return p
	}
	return sandboxDir
}

// fileSize writes a file's size for a table: bytes below a KiB, else one
// decimal in the largest binary unit that keeps it above 1.
func fileSize(n int64) string {
	if n < 1024 {
		return strconv.FormatInt(n, 10) + " B"
	}
	f := float64(n)
	for _, u := range []string{"KiB", "MiB", "GiB", "TiB"} {
		f /= 1024
		if f < 1024 || u == "TiB" {
			return strconv.FormatFloat(f, 'f', 1, 64) + " " + u
		}
	}
	return ""
}

// crumb is a directory on the way to the one listed.
type crumb struct{ Label, Href string }

// fileRow is an entry of a listing as the page shows it.
type fileRow struct {
	// Name is the entry's name with control characters escaped.
	Name string
	Kind string
	Size int64
	Mod  time.Time
	// Href opens a directory; empty for the others, and for a name that
	// cannot be put in a link unchanged.
	Href string
}

// dirView is what the listing of a directory is rendered from.
type dirView struct {
	*view
	Dir    string
	Crumbs []crumb
	Rows   []fileRow
	// Truncated says that the directory holds more entries than shown.
	Truncated bool
	// Err says why there is no listing.
	Err string
	// Cmd is the openshell command that lists the same, to copy.
	Cmd string
}

// filesURL is the link to the Files tab of the sandbox, in the directory dir.
func filesURL(vmName, ws, sandbox, dir string) string {
	return sandboxURL(vmName, ws, sandbox) + "?" + url.Values{"tab": {"files"}, "path": {dir}}.Encode()
}

// crumbs lists the directories from the workspace down to dir.
func crumbs(vmName, ws, sandbox, dir string) []crumb {
	out := []crumb{{Label: sandboxDir, Href: filesURL(vmName, ws, sandbox, sandboxDir)}}
	cur := sandboxDir
	for _, seg := range strings.Split(strings.TrimPrefix(dir, sandboxDir), "/") {
		if seg == "" {
			continue
		}
		cur = path.Join(cur, seg)
		out = append(out, crumb{Label: seg, Href: filesURL(vmName, ws, sandbox, cur)})
	}
	return out
}

// rows turns a listing into rows: directories first, then by name.
func rows(vmName, ws, sandbox, dir string, d store.Dir) []fileRow {
	out := make([]fileRow, 0, len(d.Files))
	for _, f := range d.Files {
		r := fileRow{Name: termtext.Escape(f.Name), Kind: f.Kind, Size: max(0, f.Size), Mod: f.Mod}
		if f.Kind == "dir" && r.Name == f.Name && f.Name != "" {
			r.Href = filesURL(vmName, ws, sandbox, path.Join(dir, f.Name))
		}
		out = append(out, r)
	}
	slices.SortStableFunc(out, func(a, b fileRow) int {
		if (a.Kind == "dir") != (b.Kind == "dir") {
			if a.Kind == "dir" {
				return -1
			}
			return 1
		}
		return strings.Compare(strings.ToLower(a.Name), strings.ToLower(b.Name))
	})
	return out
}

// sandboxFiles answers with the listing of a directory in a sandbox, which
// the Files tab loads. Listing runs a read-only command in the sandbox
// through its gateway; a sandbox that does not run is not asked.
func (s *Server) sandboxFiles(w http.ResponseWriter, r *http.Request) {
	if !sameOriginFetch(r) {
		forbidden(w, "The request came from another page.")
		return
	}
	vmName, ws, name := r.PathValue("vm"), r.PathValue("workspace"), r.PathValue("name")
	dir, ok := workspacePath(r.URL.Query().Get("path"))
	if vm.ValidateName(vmName) != nil || !action.ValidWorkspaceName(ws) || !action.ValidSandboxName(name) || !ok {
		s.notFound(w, r)
		return
	}
	v := s.newView(r, "", "")
	var found bool
	if v.Gateway, found = v.Snap.Gateway(vmName); found {
		v.Sandbox, found = v.Gateway.Sandbox(ws, name)
	}
	if !found {
		s.notFound(w, r)
		return
	}
	d := &dirView{view: v, Dir: dir, Crumbs: crumbs(vmName, ws, name, dir)}
	d.Cmd = oscmd(vmName, ws, append([]string{"sandbox", "exec", "-n", name, "--no-tty", "--"}, osclient.ListCommand(dir)...)...)
	switch {
	case s.files == nil:
		d.Err = "brig ui cannot list files here."
	case v.Gateway.State != store.GatewayReady:
		d.Err = v.Gateway.Name() + " is not reachable, so brig ui cannot list the sandbox's files."
	case !v.Sandbox.Ready():
		d.Err = name + " is " + strings.ToLower(v.Sandbox.Phase) + "; its files can be listed once it runs."
	default:
		ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
		listing, err := s.files.ListFiles(ctx, vmName, v.Sandbox.Workspace, name, dir)
		cancel()
		if err != nil {
			s.log.Warn("listing files", "vm", vmName, "workspace", ws, "sandbox", name, "dir", dir, "err", err)
			d.Err = "The listing failed: " + termtext.EscapeLines(err.Error())
			break
		}
		d.Rows, d.Truncated = rows(vmName, ws, name, dir, listing), listing.Truncated
	}
	s.fragmentResponse(w, http.StatusOK, "sandbox-dir", d)
}
