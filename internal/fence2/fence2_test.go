// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

package fence2

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

var t0 = time.Date(2026, 10, 14, 9, 12, 3, 0, time.UTC)

func readAll(t *testing.T, path string, pos Position) ([]Entry, Position, bool) {
	t.Helper()
	var es []Entry
	next, lost, err := ReadSpool(path, pos, func(e Entry) { es = append(es, e) })
	if err != nil {
		t.Fatal(err)
	}
	return es, next, lost
}

func TestSpool(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit", "dev", SpoolFile)
	s := Spool{Path: path, Limit: 200}
	if es, pos, lost := readAll(t, path, Position{}); es != nil || pos != (Position{}) || lost {
		t.Fatalf("reading a missing spool: %v, %v, %v", es, pos, lost)
	}
	entry := func(port int) Entry {
		return Entry{T: t0.Add(time.Duration(port) * time.Second), Reason: ReasonLAN, Proto: "tcp", Addr: "192.168.1.20", Port: port, Hits: 1}
	}
	if err := s.Append(Entry{T: t0, State: Itemised}, entry(1)); err != nil {
		t.Fatal(err)
	}
	es, pos, _ := readAll(t, path, Position{})
	if len(es) != 2 || es[0].State != Itemised || es[1] != entry(1) {
		t.Fatalf("entries %+v", es)
	}

	// A line still being written is left for the next read.
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(`{"t":"2026-10-14T09:12:03Z","reason":"lan"`); err != nil {
		t.Fatal(err)
	}
	if es, next, _ := readAll(t, path, pos); es != nil || next != pos {
		t.Fatalf("a partial line was read: %+v, %v", es, next)
	}
	if _, err := f.WriteString(`,"hits":2}` + "\n"); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()
	es, pos, _ = readAll(t, path, pos)
	if len(es) != 1 || es[0].Hits != 2 {
		t.Fatalf("finished line: %+v", es)
	}

	// Past the limit, the spool moves aside; the reader finishes the
	// copy and goes on with the new spool.
	if err := s.Append(entry(2)); err != nil {
		t.Fatal(err)
	}
	if err := s.Append(entry(3)); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path + ".1"); err != nil {
		t.Fatalf("not rotated: %v", err)
	}
	es, pos, lost := readAll(t, path, pos)
	if len(es) != 2 || es[0].Port != 2 || es[1].Port != 3 || lost {
		t.Fatalf("across a rotation: %+v, lost %v", es, lost)
	}

	// Rotated twice: the copy holds what is left, and entries are lost.
	for port := 4; port < 12; port++ {
		if err := s.Append(entry(port)); err != nil {
			t.Fatal(err)
		}
	}
	es, _, lost = readAll(t, path, pos)
	if !lost || len(es) == 0 || es[len(es)-1].Port != 11 {
		t.Fatalf("after two rotations: %+v, lost %v", es, lost)
	}

	// A first read takes the copy too.
	es, _, lost = readAll(t, path, Position{})
	if lost || len(es) < 2 || es[len(es)-1].Port != 11 {
		t.Fatalf("first read: %+v, lost %v", es, lost)
	}
}

// nflogMessage builds an NFLOG packet message with the prefix and payload.
func nflogMessage(prefix string, payload []byte) []byte {
	attrs := append(netlinkAttr(nfulaPrefix, append([]byte(prefix), 0)), netlinkAttr(nfulaPayload, payload)...)
	msg := make([]byte, nlmsgHdrLen+nfgenHdrLen, nlmsgHdrLen+nfgenHdrLen+len(attrs))
	msg = append(msg, attrs...)
	binary.NativeEndian.PutUint32(msg, uint32(len(msg))) //nolint:gosec // test data
	binary.NativeEndian.PutUint16(msg[4:], nfnlSubsysULOG<<8|nfulnlMsgPacket)
	msg[nlmsgHdrLen] = unix.AF_INET
	binary.BigEndian.PutUint16(msg[nlmsgHdrLen+2:], Group)
	return msg
}

func TestParseMessages(t *testing.T) {
	ipv4 := make([]byte, 24)
	ipv4[0], ipv4[9] = 0x45, protoTCP
	copy(ipv4[16:], []byte{192, 168, 1, 20})
	binary.BigEndian.PutUint16(ipv4[22:], 445)
	frag := append([]byte(nil), ipv4...)
	binary.BigEndian.PutUint16(frag[6:], 100)
	ipv6 := make([]byte, 52)
	ipv6[0], ipv6[6] = 0x60, 60 // destination options, then UDP
	copy(ipv6[24:], netip.MustParseAddr("fd00::7").AsSlice())
	ipv6[40], ipv6[41] = protoUDP, 0
	binary.BigEndian.PutUint16(ipv6[48+2:], 53)
	icmp := make([]byte, 28)
	icmp[0], icmp[9] = 0x45, protoICMP
	copy(icmp[16:], []byte{8, 8, 8, 8})

	b := nflogMessage(ReasonLAN, ipv4)
	b = append(b, nflogMessage(ReasonDNS, ipv6)...)
	b = append(b, nflogMessage(ReasonInternet, icmp)...)
	b = append(b, nflogMessage(ReasonLAN, frag)...)
	b = append(b, nflogMessage("", ipv4)...)       // no prefix: not brig's
	b = append(b, nflogMessage(ReasonLAN, nil)...) // no payload
	got := parseMessages(b, t0)
	want := []Packet{
		{At: t0, Reason: ReasonLAN, Proto: "tcp", Addr: netip.MustParseAddr("192.168.1.20"), Port: 445},
		{At: t0, Reason: ReasonDNS, Proto: "udp", Addr: netip.MustParseAddr("fd00::7"), Port: 53},
		{At: t0, Reason: ReasonInternet, Proto: "icmp", Addr: netip.MustParseAddr("8.8.8.8")},
		{At: t0, Reason: ReasonLAN, Proto: "tcp", Addr: netip.MustParseAddr("192.168.1.20")},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("parseMessages() =\n%+v\nwant\n%+v", got, want)
	}
	if got := parseMessages(b[:len(b)-3], t0); len(got) != 4 {
		t.Errorf("a cut message: %d packets", len(got))
	}
}

func TestAggregator(t *testing.T) {
	a := &aggregator{whyUncounted: WhyOverRate, windows: map[dropKey]*Entry{}, itemised: map[string]uint64{}, reported: map[string]uint64{}}
	nas := netip.MustParseAddr("192.168.1.20")
	at := t0.Add
	a.add([]Packet{
		{At: at(time.Second), Reason: ReasonLAN, Proto: "tcp", Addr: nas, Port: 445},
		{At: at(0), Reason: ReasonLAN, Proto: "tcp", Addr: nas, Port: 445},
		{At: at(2 * time.Second), Reason: ReasonLAN, Proto: "tcp", Addr: nas, Port: 445},
		{At: at(3 * time.Second), Reason: ReasonHost, Proto: "tcp", Addr: nas, Port: 22},
	})
	got := a.flush(map[string]uint64{ReasonLAN: 3, ReasonHost: 1, ReasonUDPFlows: 500}, t0, at(Window))
	want := []Entry{
		{T: at(0), Last: at(2 * time.Second), Reason: ReasonLAN, Proto: "tcp", Addr: "192.168.1.20", Port: 445, Hits: 3},
		{T: at(3 * time.Second), Reason: ReasonHost, Proto: "tcp", Addr: "192.168.1.20", Port: 22, Hits: 1},
		{T: t0, Last: at(Window), Reason: ReasonUDPFlows, Hits: 500, Why: WhyOverRate},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("flush() =\n%+v\nwant\n%+v", got, want)
	}
	// A drop counted in one window and read in the next is itemised, not
	// counted twice.
	a.add([]Packet{{At: at(Window), Reason: ReasonLAN, Proto: "tcp", Addr: nas, Port: 445}})
	got = a.flush(map[string]uint64{ReasonLAN: 4, ReasonHost: 1, ReasonUDPFlows: 600}, at(Window), at(2*Window))
	if len(got) != 2 || got[0].Hits != 1 || got[1].Reason != ReasonUDPFlows || got[1].Hits != 100 {
		t.Fatalf("second flush() = %+v", got)
	}
	if got := a.flush(nil, at(2*Window), at(3*Window)); len(got) != 0 {
		t.Fatalf("flush() without counters = %+v", got)
	}
}

func TestParseCounters(t *testing.T) {
	out := `{"nftables": [{"metainfo": {"version": "1.0.9", "json_schema_version": 1}}, {"counter": {"family": "inet", "name": "host-port", "table": "brig", "handle": 4, "packets": 7, "bytes": 420}}, {"counter": {"family": "inet", "name": "lan", "table": "brig", "handle": 5, "packets": 1, "bytes": 29}}, {"counter": {"family": "inet", "name": "other", "table": "brig", "packets": 9}}]}`
	got, err := parseCounters([]byte(out))
	if err != nil || !reflect.DeepEqual(got, map[string]uint64{ReasonHostPort: 7, ReasonLAN: 1}) {
		t.Fatalf("parseCounters() = %v, %v", got, err)
	}
	if _, err := parseCounters([]byte("Error")); err == nil {
		t.Error("parseCounters accepted no JSON")
	}
}

// fakeSource gives the packets of a channel.
type fakeSource struct {
	pkts   chan []Packet
	mu     sync.Mutex
	closed bool
}

func (f *fakeSource) read(timeout time.Duration, _ func() time.Time) ([]Packet, error) {
	select {
	case p := <-f.pkts:
		return p, nil
	case <-time.After(timeout):
		return nil, nil
	}
}

func (f *fakeSource) close() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closed = true
}

func TestListenerRun(t *testing.T) {
	path := filepath.Join(t.TempDir(), SpoolFile)
	src := &fakeSource{pkts: make(chan []Packet, 4)}
	var mu sync.Mutex
	counters := map[string]uint64{}
	l := &Listener{
		Spool: Spool{Path: path}, Mode: Itemised, Window: 50 * time.Millisecond,
		Counters: func() (map[string]uint64, error) {
			mu.Lock()
			defer mu.Unlock()
			return map[string]uint64{ReasonLAN: counters[ReasonLAN]}, nil
		},
		open: func() (packetSource, error) { return src, nil },
	}
	ctx, cancel := context.WithCancel(context.Background())
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error)
	go func() { done <- l.Run(ctx, w) }()
	buf := make([]byte, 64)
	n, _ := r.Read(buf)
	if got := string(buf[:n]); got != "itemised\n" {
		t.Errorf("ready said %q", got)
	}
	mu.Lock()
	counters[ReasonLAN] = 250
	mu.Unlock()
	src.pkts <- []Packet{{At: time.Now(), Reason: ReasonLAN, Proto: "udp", Addr: netip.MustParseAddr("10.0.0.5"), Port: 445}}
	time.Sleep(200 * time.Millisecond)
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	es, _, _ := readAll(t, path, Position{})
	var hits, uncounted int
	for _, e := range es[1:] {
		if e.Why == WhyOverRate {
			uncounted += e.Hits
		} else {
			hits += e.Hits
		}
	}
	if es[0].State != Itemised || hits != 1 || uncounted != 249 {
		t.Errorf("spool: %+v", es)
	}
	if !src.closed {
		t.Error("the source was not closed")
	}

	// The NFLOG group cannot be bound: the listener counts only.
	path = filepath.Join(t.TempDir(), SpoolFile)
	l = &Listener{
		Spool: Spool{Path: path}, Mode: Itemised, Window: time.Hour,
		Counters: func() (map[string]uint64, error) { return map[string]uint64{ReasonDNS: 3}, nil },
		open:     func() (packetSource, error) { return nil, errors.New("protocol not supported") },
	}
	ctx, cancel = context.WithCancel(context.Background())
	cancel()
	if err := l.Run(ctx, nil); err != nil {
		t.Fatal(err)
	}
	es, _, _ = readAll(t, path, Position{})
	if len(es) != 2 || es[0].State != CountsOnly || !strings.Contains(es[0].Why, "protocol not supported") ||
		es[1].Hits != 3 || es[1].Why != WhyCountsOnly {
		t.Errorf("counting only: %+v", es)
	}

	// Off: the state only.
	path = filepath.Join(t.TempDir(), SpoolFile)
	l = &Listener{Spool: Spool{Path: path}, Mode: Off, Why: "no counters"}
	if err := l.Run(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	if es, _, _ = readAll(t, path, Position{}); len(es) != 1 || es[0].State != Off || es[0].Why != "no counters" {
		t.Errorf("off: %+v", es)
	}
}

// TestNFLOG reads drops that a real firewall logs, in a throwaway user and
// network namespace when the host allows them, as pasta's is.
func TestNFLOG(t *testing.T) {
	if os.Getenv("FENCE2_TEST_IN_NAMESPACE") != "" {
		nflogInNamespace(t)
		return
	}
	nft, err := exec.LookPath("nft")
	if err != nil {
		t.Skip("nft not installed")
	}
	if exec.Command("unshare", "-Urn", "true").Run() != nil {
		t.Skip("unprivileged user and network namespaces unavailable")
	}
	cmd := exec.Command("unshare", "-Urn", os.Args[0], "-test.run=^TestNFLOG$", "-test.v") //nolint:gosec // G702: the test binary itself
	cmd.Env = append(os.Environ(), "FENCE2_TEST_IN_NAMESPACE=1", "FENCE2_TEST_NFT="+nft)
	out, err := cmd.CombinedOutput()
	if strings.Contains(string(out), "SKIP:") {
		t.Skipf("in the namespace: %s", out)
	}
	if err != nil {
		t.Fatalf("in the namespace: %v\n%s", err, out)
	}
}

func nflogInNamespace(t *testing.T) {
	// Bring the loopback interface up, as pasta does.
	fd, err := unix.Socket(unix.AF_INET, unix.SOCK_DGRAM, 0)
	if err != nil {
		t.Fatal(err)
	}
	ifr, err := unix.NewIfreq("lo")
	if err != nil {
		t.Fatal(err)
	}
	if err := unix.IoctlIfreq(fd, unix.SIOCGIFFLAGS, ifr); err != nil {
		t.Fatal(err)
	}
	ifr.SetUint16(ifr.Uint16() | unix.IFF_UP)
	if err := unix.IoctlIfreq(fd, unix.SIOCSIFFLAGS, ifr); err != nil {
		t.Fatal(err)
	}
	_ = unix.Close(fd)

	nft := os.Getenv("FENCE2_TEST_NFT")
	rules := fmt.Sprintf(`table inet brig {
	counter lan {
	}
	chain output {
		type filter hook output priority filter; policy accept;
		ip daddr 127.0.0.2 jump %s
	}
	chain %[1]s {
		counter name "lan"
		limit rate %d/second burst %d packets log group %d snaplen %d prefix "lan"
		drop
	}
}
`, Chain(ReasonLAN), LogRate, LogBurst, Group, Snaplen)
	load := exec.Command(nft, "-f", "-") //nolint:gosec // G702: nft as the outer test found it
	load.Stdin = strings.NewReader(rules)
	if out, err := load.CombinedOutput(); err != nil {
		t.Skipf("nft cannot load logging rules here: %v: %s", err, out)
	}
	src, err := openNFLOG(Group)
	if err != nil {
		t.Skipf("cannot bind NFLOG here: %v", err)
	}
	defer src.close()

	conn, err := net.Dial("udp", "127.0.0.2:445")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	const sent = 300
	for range sent {
		if _, err := conn.Write([]byte("x")); !errors.Is(err, unix.EPERM) {
			t.Fatalf("sending to a dropped address: %v, want EPERM", err)
		}
	}
	var pkts []Packet
	for len(pkts) < LogBurst {
		got, err := src.read(time.Second, time.Now)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) == 0 {
			break
		}
		pkts = append(pkts, got...)
	}
	if len(pkts) == 0 || len(pkts) > LogBurst+LogRate {
		t.Fatalf("read %d packets of %d sent", len(pkts), sent)
	}
	if p := pkts[0]; p.Reason != ReasonLAN || p.Proto != "udp" || p.Addr != netip.MustParseAddr("127.0.0.2") || p.Port != 445 {
		t.Errorf("packet %+v", p)
	}
	counters, err := ReadCounters(nft)
	if err != nil || counters[ReasonLAN] != sent {
		t.Fatalf("ReadCounters() = %v, %v; want %d drops", counters, err, sent)
	}
	a := &aggregator{whyUncounted: WhyOverRate, windows: map[dropKey]*Entry{}, itemised: map[string]uint64{}, reported: map[string]uint64{}}
	a.add(pkts)
	es := a.flush(counters, t0, t0.Add(Window))
	if len(es) != 2 || es[0].Hits != len(pkts) || es[1].Hits != sent-len(pkts) || es[1].Why != WhyOverRate {
		t.Errorf("flush() = %+v", es)
	}
}
