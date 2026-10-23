// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

package ui

import (
	"embed"
	"io/fs"
	"os"
)

// web holds the templates and the static files: CSS, JavaScript and the
// fonts, so that brig ui works offline and loads nothing from elsewhere.
//
//go:embed web
var web embed.FS

// siteFiles returns the files of web: the embedded ones, or those in dir
// when dir is not empty (brig ui --dev-assets, for working on the
// templates).
func siteFiles(dir string) (fs.FS, error) {
	if dir != "" {
		return os.DirFS(dir), nil
	}
	return fs.Sub(web, "web")
}
