// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

package ui

import (
	"errors"
	"net"
	"net/netip"
	"os"
	"strings"
	"testing"
	"time"
)

// procNetTCP is /proc/net/tcp with a listener on 127.0.0.1:7878, a client
// of user 1000 connected to it from port 54321, and the server's end.
const procNetTCP = `  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode
   0: 0100007F:1EC6 00000000:0000 0A 00000000:00000000 00:00000000 00000000  1000        0 101 1 0000000000000000 100 0 0 10 0
   3: 0100007F:D432 0100007F:1EC6 06 00000000:00000000 03:00000000 00000000     0        0 0 3 0000000000000000
   4: 0100007F:D433 0100007F:1EC6 01 00000000:00000000 00:00000000 00000000     0        0 0 1 0000000000000000 20 4 30 10 -1
   1: 0100007F:D431 0100007F:1EC6 01 00000000:00000000 00:00000000 00000000  1001        0 102 1 0000000000000000 20 4 30 10 -1
   2: 0100007F:1EC6 0100007F:D431 01 00000000:00000000 00:00000000 00000000  1000        0 103 1 0000000000000000 20 4 30 10 -1
`

func TestSocketOwner(t *testing.T) {
	client := netip.MustParseAddrPort("127.0.0.1:54321")
	server := netip.MustParseAddrPort("127.0.0.1:7878")
	uid, err := socketOwner(strings.NewReader(procNetTCP), tcpEstablished, client, server)
	if err != nil || uid != 1001 {
		t.Errorf("client socket: uid %d, %v; want 1001", uid, err)
	}
	if _, err := socketOwner(strings.NewReader(procNetTCP), tcpEstablished, netip.MustParseAddrPort("127.0.0.1:1"), server); err == nil {
		t.Error("found a socket that is not there")
	}
	for port, why := range map[string]string{"54322": "in TIME_WAIT", "54323": "without an inode"} {
		if _, err := socketOwner(strings.NewReader(procNetTCP), tcpEstablished, netip.MustParseAddrPort("127.0.0.1:"+port), server); err == nil {
			t.Errorf("found a socket %s", why)
		}
	}
}

func TestSocketOwnerOfAListener(t *testing.T) {
	server := netip.MustParseAddrPort("127.0.0.1:7878")
	unspecified := netip.AddrPortFrom(netip.IPv4Unspecified(), 0)
	uid, err := socketOwner(strings.NewReader(procNetTCP), tcpListen, server, unspecified)
	if err != nil || uid != 1000 {
		t.Errorf("listener: uid %d, %v; want 1000", uid, err)
	}
	if _, err := socketOwner(strings.NewReader(procNetTCP), tcpListen, netip.MustParseAddrPort("127.0.0.1:7879"), unspecified); err == nil {
		t.Error("found a listener that is not there")
	}
}

func TestParseProcAddr(t *testing.T) {
	got, err := parseProcAddr("0100007F:1F90")
	if err != nil || got != netip.MustParseAddrPort("127.0.0.1:8080") {
		t.Errorf("got %v, %v", got, err)
	}
	for _, bad := range []string{"", "0100007F", "01007F:1F90", "zz00007F:1F90", "0100007F:fffff"} {
		if _, err := parseProcAddr(bad); err == nil {
			t.Errorf("%q parsed", bad)
		}
	}
}

// TestPeerListenerOwnUser connects to a real listener: the kernel's table
// must name this process's user as the client's owner.
func TestPeerListenerOwnUser(t *testing.T) {
	if _, err := os.Stat("/proc/net/tcp"); err != nil {
		t.Skip("no /proc/net/tcp")
	}
	inner, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var refused []error
	l := &peerListener{Listener: inner, uid: os.Getuid(), owner: procNetTCPOwner, refused: func(err error) { refused = append(refused, err) }}
	defer func() { _ = l.Close() }()
	go func() {
		c, err := net.Dial("tcp4", inner.Addr().String())
		if err == nil {
			time.Sleep(100 * time.Millisecond)
			_ = c.Close()
		}
	}()
	c, err := l.Accept()
	if err != nil {
		t.Fatal(err)
	}
	_ = c.Close()
	if len(refused) != 0 {
		t.Errorf("refused: %v", refused)
	}
}

func TestPeerListenerRefusesOtherUsers(t *testing.T) {
	inner, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	owners := []int{1001, 0, 1000}
	var refused []error
	l := &peerListener{
		Listener: inner, uid: 1000,
		owner: func(netip.AddrPort, netip.AddrPort) (int, error) {
			uid := owners[0]
			owners = owners[1:]
			if uid == 0 {
				return 0, errors.New("no such socket")
			}
			return uid, nil
		},
		refused: func(err error) { refused = append(refused, err) },
	}
	defer func() { _ = l.Close() }()
	closed := make(chan bool, 3)
	for range 3 {
		go func() {
			c, err := net.Dial("tcp4", inner.Addr().String())
			if err != nil {
				closed <- false
				return
			}
			defer func() { _ = c.Close() }()
			_ = c.SetReadDeadline(time.Now().Add(2 * time.Second))
			_, err = c.Read(make([]byte, 1))
			closed <- err != nil && !errors.Is(err, os.ErrDeadlineExceeded)
		}()
	}
	c, err := l.Accept()
	if err != nil {
		t.Fatal(err)
	}
	_ = c.Close()
	if len(refused) != 2 {
		t.Errorf("refused %d connections, want 2: %v", len(refused), refused)
	}
	for range 3 {
		if !<-closed {
			t.Error("a connection was neither accepted and closed nor refused")
		}
	}
}
