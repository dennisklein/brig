// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// pngMagic starts every PNG file.
var pngMagic = []byte("\x89PNG\r\n\x1a\n")

// TestScreenshotsAreTaken checks that the images are those of the shot list:
// each shot has an image for each of its themes and sizes, and the directory
// holds no other. A change to the list needs tools/screenshots run again.
func TestScreenshotsAreTaken(t *testing.T) {
	root := repoRoot(t)
	list, err := loadShots(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) == 0 {
		t.Fatalf("%s lists no shots", shotsFile)
	}
	want := map[string]bool{}
	for _, s := range list {
		if s.Name == "" || s.Alt == "" {
			t.Errorf("%s: a shot needs a name and an alt, not %+v", shotsFile, s)
		}
		for _, f := range s.files() {
			if want[f] {
				t.Errorf("%s: the image %s is of two shots", shotsFile, f)
			}
			want[f] = true
			data, err := os.ReadFile(filepath.Join(root, imagesDir, f))
			switch {
			case err != nil:
				t.Errorf("shot %s has no image: %v; run tools/screenshots", s.Name, err)
			case !bytes.HasPrefix(data, pngMagic):
				t.Errorf("%s is no PNG", path.Join(imagesDir, f))
			}
		}
	}
	entries, err := os.ReadDir(filepath.Join(root, imagesDir))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if n := e.Name(); strings.HasSuffix(n, ".png") && !want[n] {
			t.Errorf("%s is the image of no shot; delete it", path.Join(imagesDir, n))
		}
	}
}

// TestScreenshotsAreShown checks that every shot is in the docs, in both
// themes, and that every image of the docs is a shot.
func TestScreenshotsAreShown(t *testing.T) {
	root := repoRoot(t)
	list, err := loadShots(root)
	if err != nil {
		t.Fatal(err)
	}
	known := map[string]bool{}
	for _, s := range list {
		for _, f := range s.files() {
			known[f] = true
		}
	}
	shown := map[string]bool{}
	for _, f := range markdownFiles(t, root, true) {
		dests := map[string]bool{}
		for _, l := range f.links {
			dests[path.Join(path.Dir(f.path), l.dest)] = true
		}
		for d := range dests {
			if path.Dir(d) != imagesDir || !strings.HasSuffix(d, ".png") {
				continue
			}
			name := path.Base(d)
			if !known[name] {
				t.Errorf("%s shows %s, which is the image of no shot", f.path, d)
				continue
			}
			shown[name] = true
			other := strings.Replace(name, "-light.png", "-dark.png", 1)
			if strings.HasSuffix(name, "-dark.png") {
				other = strings.Replace(name, "-dark.png", "-light.png", 1)
			}
			if !dests[path.Join(imagesDir, other)] {
				t.Errorf("%s shows %s but not %s; a picture has both themes", f.path, d, other)
			}
		}
	}
	for _, s := range list {
		if !slices.ContainsFunc(s.files(), func(f string) bool { return shown[f] }) {
			t.Errorf("no page of the docs shows the shot %s; show it or drop it from %s", s.Name, shotsFile)
		}
	}
}
