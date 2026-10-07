// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

package sdcred

import "testing"

func TestEncoding(t *testing.T) {
	c := Credential{Name: "tmpfiles.extra", Data: []byte("d /x 0700 - - -\n")}
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	if got, want := c.OEMString(), "io.systemd.credential.binary:tmpfiles.extra=ZCAveCAwNzAwIC0gLSAtCg=="; got != want {
		t.Errorf("OEMString() = %q, want %q", got, want)
	}
	if got, want := c.FwCfgName(), "opt/io.systemd.credentials/tmpfiles.extra"; got != want {
		t.Errorf("FwCfgName() = %q, want %q", got, want)
	}
}

func TestValidateRejectsBadNames(t *testing.T) {
	for _, name := range []string{"", "a b", "a/b", "a=b"} {
		if err := (Credential{Name: name}).Validate(); err == nil {
			t.Errorf("Validate(%q) succeeded", name)
		}
	}
}
