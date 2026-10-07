// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

package packaging

import (
	"bytes"
	"strings"
	"testing"
)

func TestRepo(t *testing.T) {
	repoFile, key, err := Repo()
	if err != nil {
		if strings.Contains(err.Error(), "no repository signing key") {
			t.Skip("signing key not created yet")
		}
		t.Fatal(err)
	}
	if !bytes.Contains(repoFile, []byte("[brig]")) || !bytes.Contains(repoFile, []byte("repo_gpgcheck=1")) {
		t.Errorf("unexpected brig.repo:\n%s", repoFile)
	}
	if !bytes.HasPrefix(key, []byte("-----BEGIN PGP PUBLIC KEY BLOCK-----")) {
		t.Errorf("RPM-GPG-KEY-brig is not an armored public key")
	}
}
