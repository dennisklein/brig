// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

// Package packaging embeds the definition of brig's dnf repository, which
// serves OpenShell built from source, so brig can install from it into VM
// images.
package packaging

import (
	"embed"
	"errors"
	"io/fs"
)

//go:embed brig-release
var release embed.FS

// Repo returns the dnf repository definition (brig.repo) and the OpenPGP
// public key that signs the repository's packages and metadata.
func Repo() (repoFile, key []byte, err error) {
	repoFile, err = release.ReadFile("brig-release/brig.repo")
	if err != nil {
		return nil, nil, err
	}
	key, err = release.ReadFile("brig-release/RPM-GPG-KEY-brig")
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil, errors.New("this brig build has no repository signing key (packaging/brig-release/RPM-GPG-KEY-brig); create it with scripts/signing-key.sh")
	}
	if err != nil {
		return nil, nil, err
	}
	return repoFile, key, nil
}
