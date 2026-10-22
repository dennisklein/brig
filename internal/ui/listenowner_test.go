// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

package ui

import (
	"net/netip"
	"strings"
	"testing"
)

// procNetTCP is /proc/net/tcp with a listener of user 1000 on
// 127.0.0.1:7878, a listener without an inode on 127.0.0.1:7879, and an
// established connection on 127.0.0.1:7880.
const procNetTCP = `  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode
   0: 0100007F:1EC6 00000000:0000 0A 00000000:00000000 00:00000000 00000000  1000        0 101 1 0000000000000000 100 0 0 10 0
   1: 0100007F:1EC7 00000000:0000 0A 00000000:00000000 00:00000000 00000000  1001        0 0 1 0000000000000000 100 0 0 10 0
   2: 0100007F:1EC8 0100007F:D431 01 00000000:00000000 00:00000000 00000000  1002        0 103 1 0000000000000000 20 4 30 10 -1
`

func TestListenerOwner(t *testing.T) {
	uid, err := listenerOwner(strings.NewReader(procNetTCP), netip.MustParseAddrPort("127.0.0.1:7878"))
	if err != nil || uid != 1000 {
		t.Errorf("listener: uid %d, %v; want 1000", uid, err)
	}
	for addr, why := range map[string]string{
		"127.0.0.1:7877": "that is not there",
		"127.0.0.1:7879": "without an inode",
		"127.0.0.1:7880": "that is no listener",
	} {
		if _, err := listenerOwner(strings.NewReader(procNetTCP), netip.MustParseAddrPort(addr)); err == nil {
			t.Errorf("found a socket %s", why)
		}
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
