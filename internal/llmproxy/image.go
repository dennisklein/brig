// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

package llmproxy

import (
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
)

// LiteLLMVersion is the version of LiteLLM that the sandbox image installs,
// pinned with every dependency's hashes in image/requirements.txt. Bump it
// with that file:
//
//	uv pip compile --universal --python-version 3.13 --generate-hashes \
//	  --no-header -o requirements.txt requirements.in
//
// where requirements.in holds litellm[proxy]==VERSION and prisma.
const LiteLLMVersion = "1.106.2"

// ImageRepository is the repository of the LiteLLM sandbox image in the
// host's and the service VM's Podman.
const ImageRepository = "localhost/brig-llm-proxy"

//go:embed image
var imageFS embed.FS

// imageFiles are the build context's files and their modes.
var imageFiles = map[string]fs.FileMode{
	"Containerfile":    0o644,
	"llm-proxy-load":   0o755,
	"llm-proxy-start":  0o755,
	"requirements.txt": 0o644,
}

// ImageRef returns the reference of the image that this brig builds: the
// LiteLLM version and a digest of the build context, so that a change to
// the context makes a new tag.
func ImageRef() string {
	h := sha256.New()
	for _, name := range slices.Sorted(maps.Keys(imageFiles)) {
		data, err := imageFS.ReadFile("image/" + name)
		if err != nil {
			panic(err)
		}
		h.Write([]byte(name + "\x00"))
		h.Write(data)
		h.Write([]byte{0})
	}
	return ImageRepository + ":" + LiteLLMVersion + "-" + hex.EncodeToString(h.Sum(nil))[:12]
}

// WriteImageContext writes the image's build context into dir, which must
// exist, for podman build.
func WriteImageContext(dir string) error {
	for name, mode := range imageFiles {
		data, err := imageFS.ReadFile("image/" + name)
		if err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(dir, name), data, mode); err != nil {
			return err
		}
	}
	return nil
}
