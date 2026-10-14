// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"
	"html"
	"os"
	"path"
	"path/filepath"
	"slices"

	"go.yaml.in/yaml/v3"
)

// imagesDir holds the screenshots of brig ui and the list of them.
const (
	imagesDir = "docs/images"
	shotsFile = imagesDir + "/shots.yaml"
)

// shot is a screenshot of brig ui, as docs/images/shots.yaml lists it. The
// list is read by tools/screenshots, which takes the images; this is what
// the docs need of it.
type shot struct {
	Name   string   `yaml:"name"`
	Alt    string   `yaml:"alt"`
	Page   string   `yaml:"page"`
	Action string   `yaml:"action"`
	Sizes  []string `yaml:"sizes"`
}

// themes are the colour schemes every shot is taken in.
var themes = []string{"light", "dark"}

// loadShots reads the shot list.
func loadShots(repo string) ([]shot, error) {
	data, err := os.ReadFile(filepath.Join(repo, filepath.FromSlash(shotsFile)))
	if err != nil {
		return nil, err
	}
	var l struct {
		Shots []shot `yaml:"shots"`
	}
	if err := yaml.Unmarshal(data, &l); err != nil {
		return nil, fmt.Errorf("%s: %w", shotsFile, err)
	}
	return l.Shots, nil
}

// windows returns the names of the windows the shot is taken in.
func (s shot) windows() []string {
	if len(s.Sizes) == 0 {
		return []string{"desktop"}
	}
	return s.Sizes
}

// file returns the name of the image of the shot in a window and theme, as
// tools/screenshots names it.
func (s shot) file(window, theme string) string {
	if window == "desktop" {
		return s.Name + "-" + theme + ".png"
	}
	return s.Name + "-" + window + "-" + theme + ".png"
}

// files returns the names of all the images of the shot.
func (s shot) files() []string {
	var out []string
	for _, w := range s.windows() {
		for _, t := range themes {
			out = append(out, s.file(w, t))
		}
	}
	return out
}

// picture returns the HTML that shows the desktop image of the shot, in the
// theme of the reader's browser, from a Markdown file in the directory dir
// (relative to the root of the source tree).
func (s shot) picture(dir string) string {
	src := func(theme string) string {
		rel, _ := filepath.Rel(dir, path.Join(imagesDir, s.file("desktop", theme)))
		return filepath.ToSlash(rel)
	}
	return fmt.Sprintf("<picture>\n  <source media=\"(prefers-color-scheme: dark)\" srcset=\"%s\">\n  <img alt=\"%s\" src=\"%s\">\n</picture>\n",
		src("dark"), html.EscapeString(s.Alt), src("light"))
}

// shotsOf returns the shots that belong to the page or action with the ID,
// in the order of the list.
func shotsOf(list []shot, field func(shot) string, id string) []shot {
	return slices.DeleteFunc(slices.Clone(list), func(s shot) bool { return field(s) != id })
}
