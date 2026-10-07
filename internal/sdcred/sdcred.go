// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

// Package sdcred describes systemd system credentials that brig passes to a
// VM at boot. See systemd.system-credentials(7).
package sdcred

import (
	"encoding/base64"
	"fmt"
	"regexp"
)

// Credential is a systemd system credential.
type Credential struct {
	// Name is the credential name, e.g. "tmpfiles.extra".
	Name string
	// Data is the credential's raw content.
	Data []byte
	// Secret credentials must reach the VM as fw_cfg files: SMBIOS OEM
	// strings end up in the QEMU command line and libvirt's logs.
	Secret bool
}

var nameRE = regexp.MustCompile(`^[A-Za-z0-9_.@-]{1,255}$`)

// Validate checks that the name is a valid credential name.
func (c Credential) Validate() error {
	if !nameRE.MatchString(c.Name) {
		return fmt.Errorf("invalid credential name %q", c.Name)
	}
	return nil
}

// OEMString encodes the credential as an SMBIOS type 11 OEM string, which
// systemd imports as a credential on boot.
func (c Credential) OEMString() string {
	return "io.systemd.credential.binary:" + c.Name + "=" + base64.StdEncoding.EncodeToString(c.Data)
}

// FwCfgName is the QEMU fw_cfg entry name under which systemd looks for the
// credential when it is passed as a file.
func (c Credential) FwCfgName() string {
	return "opt/io.systemd.credentials/" + c.Name
}
