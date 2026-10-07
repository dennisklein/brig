// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

// Package ports finds TCP ports on the host's IPv4 loopback address, where
// brig forwards each VM's SSH and gateway ports.
package ports

import (
	"fmt"
	"net"
	"strconv"
)

const host = "127.0.0.1"

// Free asks the kernel for a TCP port on 127.0.0.1 that is free right now.
// Another process may take it before the caller binds it.
func Free() (int, error) {
	l, err := net.Listen("tcp", net.JoinHostPort(host, "0"))
	if err != nil {
		return 0, fmt.Errorf("find a free port: %w", err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	if err := l.Close(); err != nil {
		return 0, fmt.Errorf("find a free port: %w", err)
	}
	return port, nil
}

// Available reports whether a TCP listener can bind 127.0.0.1:port right
// now.
func Available(port int) bool {
	if port < 1 || port > 65535 {
		return false
	}
	l, err := net.Listen("tcp", net.JoinHostPort(host, strconv.Itoa(port)))
	if err != nil {
		return false
	}
	_ = l.Close()
	return true
}
