// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

package ui

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dennisklein/brig/internal/ui/store"
)

// fakeFiles lists the directories it is given.
type fakeFiles struct {
	mu    sync.Mutex
	dirs  map[string]store.Dir
	err   error
	asked []string
}

func (f *fakeFiles) ListFiles(_ context.Context, vm, _, sandbox, dir string) (store.Dir, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.asked = append(f.asked, vm+"/"+sandbox+":"+dir)
	return f.dirs[dir], f.err
}

func (f *fakeFiles) calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.asked)
}

func TestWorkspacePath(t *testing.T) {
	for in, want := range map[string]string{
		"/sandbox": "/sandbox", "/sandbox/": "/sandbox", "/sandbox/a/../b": "/sandbox/b", "/sandbox//x/./y": "/sandbox/x/y",
	} {
		if got, ok := workspacePath(in); !ok || got != want {
			t.Errorf("workspacePath(%q) = %q, %v, want %q", in, got, ok, want)
		}
	}
	for _, in := range []string{
		"", "sandbox", "/", "/etc", "/sandboxx", "/sandbox/../etc", "/sandbox/..", "/tmp/sandbox", "/sandbox/a\nb", "/sandbox/a\x00b",
		"/sandbox/" + strings.Repeat("a", maxPath),
	} {
		if got, ok := workspacePath(in); ok {
			t.Errorf("workspacePath(%q) = %q, want a refusal", in, got)
		}
	}
	if filesPath("/etc") != sandboxDir || filesPath("") != sandboxDir || filesPath("/sandbox/x") != "/sandbox/x" {
		t.Error("filesPath does not fall back to the workspace")
	}
}

func TestSandboxFiles(t *testing.T) {
	ts := newTestServer(t)
	ts.login()
	ts.files.dirs = map[string]store.Dir{"/sandbox/src": {Files: []store.File{
		{Name: "zeta.txt", Kind: "file", Size: 2048, Mod: time.Now().Add(-time.Hour)},
		{Name: "Beta", Kind: "dir", Size: 4096},
		{Name: "alpha", Kind: "dir", Size: 4096},
		{Name: "latest", Kind: "link", Size: 4},
		{Name: "<b>x</b>", Kind: "file", Size: 1},
		{Name: "bad\nname", Kind: "dir", Size: 4096},
	}}}
	w := ts.get("/sandboxes/dev/default/webapp/files?path=/sandbox/src")
	body := w.Body.String()
	if w.Code != http.StatusOK {
		t.Fatalf("%d\n%s", w.Code, body)
	}
	for _, want := range []string{
		`<a href="/sandboxes/dev/default/webapp?path=%2Fsandbox&amp;tab=files">/sandbox</a>`,
		`<a href="/sandboxes/dev/default/webapp?path=%2Fsandbox%2Fsrc&amp;tab=files">src</a>`,
		`<a href="/sandboxes/dev/default/webapp?path=%2Fsandbox%2Fsrc%2Falpha&amp;tab=files">alpha/</a>`,
		"2.0 KiB", "latest <span", "&lt;b&gt;x&lt;/b&gt;", `bad\nname/`,
		"openshell -g brig-dev --workspace default sandbox exec -n webapp --no-tty -- find /sandbox/src -mindepth 1 -maxdepth 1 -printf",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("listing lacks %q:\n%s", want, body)
		}
	}
	if strings.Contains(body, "<b>x</b>") || strings.Contains(body, `href="/sandboxes/dev/default/webapp?path=%2Fsandbox%2Fsrc%2Fbad`) {
		t.Errorf("a name was not escaped, or a name with a control character got a link:\n%s", body)
	}
	// Directories first, each group by name.
	order := []string{"alpha/", "bad\\nname/", "Beta/", "&lt;b&gt;x", "latest", "zeta.txt"}
	last := -1
	for _, n := range order {
		i := strings.Index(body, n)
		if i < last {
			t.Errorf("%q is out of order in:\n%s", n, body)
		}
		last = max(last, i)
	}
	if got := ts.files.asked; len(got) != 1 || got[0] != "dev/webapp:/sandbox/src" {
		t.Errorf("asked %v", got)
	}
}

func TestSandboxFilesRefuses(t *testing.T) {
	ts := newTestServer(t)
	ts.login()
	for _, target := range []string{
		"/sandboxes/dev/default/webapp/files", "/sandboxes/dev/default/webapp/files?path=/etc", "/sandboxes/dev/default/webapp/files?path=/sandbox/../etc",
		"/sandboxes/dev/default/webapp/files?path=sandbox", "/sandboxes/dev/default/nope/files?path=/sandbox", "/sandboxes/nope/default/webapp/files?path=/sandbox",
		"/sandboxes/dev/default/--x/files?path=/sandbox", "/sandboxes/dev/default/webapp/files?path=" + url.QueryEscape("/sandbox/a\nb"),
	} {
		if w := ts.get(target); w.Code != http.StatusNotFound {
			t.Errorf("%s: %d, want 404", target, w.Code)
		}
	}
	if n := ts.files.calls(); n != 0 {
		t.Errorf("listed %d times", n)
	}
}

func TestSandboxFilesSaysWhyNot(t *testing.T) {
	ts := newTestServer(t)
	ts.login()
	for target, want := range map[string]string{
		"/sandboxes/dev/default/tools/files?path=/sandbox":          "tools is stopped; its files can be listed once it runs",
		"/sandboxes/airgap/default/corpus-scan/files?path=/sandbox": "brig-airgap is not reachable",
	} {
		if body := ts.get(target).Body.String(); !strings.Contains(body, want) {
			t.Errorf("%s lacks %q:\n%s", target, want, body)
		}
	}
	ts.files.err = errors.New("find: '/sandbox/x': No such file or directory\x1b[31m")
	body := ts.get("/sandboxes/dev/default/webapp/files?path=/sandbox/x").Body.String()
	if !strings.Contains(body, "The listing failed: find: ") || !strings.Contains(body, "No such file or directory") || strings.ContainsRune(body, 0x1b) {
		t.Errorf("a failed listing:\n%s", body)
	}
	if n := ts.files.calls(); n != 1 {
		t.Errorf("listed %d times, want only the sandbox that runs", n)
	}
	ts.srv.files = nil
	if body := ts.get("/sandboxes/dev/default/webapp/files?path=/sandbox").Body.String(); !strings.Contains(body, "cannot list files") {
		t.Errorf("without a lister:\n%s", body)
	}
}

func TestFilesTab(t *testing.T) {
	ts := newTestServer(t)
	ts.login()
	body := ts.get("/sandboxes/dev/default/webapp?tab=files&path=/sandbox/src").Body.String()
	for _, want := range []string{
		`hx-get="/sandboxes/dev/default/webapp/files?path=%2Fsandbox%2Fsrc"`, `hx-trigger="load"`, "sandbox download webapp /sandbox/PATH .",
		"path=%2Fsandbox%2Fsrc", // the live updates keep the directory
	} {
		if !strings.Contains(body, want) {
			t.Errorf("Files tab lacks %q:\n%s", want, body)
		}
	}
	// A path outside the workspace falls back to it.
	if body := ts.get("/sandboxes/dev/default/webapp?tab=files&path=/etc").Body.String(); !strings.Contains(body, `files?path=%2Fsandbox"`) {
		t.Errorf("Files tab with a path outside the workspace:\n%s", body)
	}
	// A sandbox that does not run lists nothing.
	if body := ts.get("/sandboxes/dev/default/tools?tab=files").Body.String(); strings.Contains(body, "/files?path=") || !strings.Contains(body, "tools is stopped") {
		t.Errorf("Files tab of a stopped sandbox:\n%s", body)
	}
	if w := ts.get("/events?f=sandbox&gw=dev&sb=webapp&tab=files&path=/etc"); w.Code != http.StatusBadRequest {
		t.Errorf("events with a path outside the workspace: %d", w.Code)
	}
}

func TestFileSize(t *testing.T) {
	for n, want := range map[int64]string{
		0: "0 B", 1023: "1023 B", 1024: "1.0 KiB", 183420: "179.1 KiB", 5 << 20: "5.0 MiB", 3<<30 + 1<<29: "3.5 GiB", 5 << 40: "5.0 TiB", 2000 << 40: "2000.0 TiB",
	} {
		if got := fileSize(n); got != want {
			t.Errorf("fileSize(%d) = %q, want %q", n, got, want)
		}
	}
}
