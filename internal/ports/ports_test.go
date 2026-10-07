// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

package ports

import (
	"net"
	"testing"
)

func TestFree(t *testing.T) {
	port, err := Free()
	if err != nil {
		t.Fatal(err)
	}
	if port < 1 || port > 65535 {
		t.Fatalf("Free() = %d", port)
	}
	if !Available(port) {
		t.Errorf("port %d from Free() is not available", port)
	}
}

func TestAvailable(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	if Available(port) {
		t.Errorf("Available(%d) while it is in use", port)
	}
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	if !Available(port) {
		t.Errorf("Available(%d) = false after closing its listener", port)
	}
	for _, bad := range []int{-1, 0, 65536} {
		if Available(bad) {
			t.Errorf("Available(%d) = true", bad)
		}
	}
}
