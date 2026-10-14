// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

package fence2

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"net/netip"
	"strconv"
	"time"

	"golang.org/x/sys/unix"
)

// Packet is a dropped packet as the firewall logged it.
type Packet struct {
	At     time.Time
	Reason string
	// Proto is tcp, udp, icmp, icmpv6, or the IP protocol's number.
	Proto string
	Addr  netip.Addr
	// Port is the destination port of TCP, UDP and SCTP, else zero.
	Port int
}

// The parts of the netfilter netlink protocol that NFLOG uses, from
// linux/netfilter/nfnetlink.h and nfnetlink_log.h.
const (
	nfnlSubsysULOG   = 4
	nfulnlMsgPacket  = 0
	nfulnlMsgConfig  = 1
	nfulaCfgCmd      = 1
	nfulaCfgMode     = 2
	nfulnlCfgCmdBind = 1
	nfulnlCopyPacket = 2
	nfulaTimestamp   = 3
	nfulaPayload     = 9
	nfulaPrefix      = 10
	nlmsgHdrLen      = 16
	nfgenHdrLen      = 4
	nlaHdrLen        = 4
	// nlaTypeMask clears the nested and byte order flags of an
	// attribute's type.
	nlaTypeMask = 0x3fff
)

// nflog reads the packets that the firewall logs to an NFLOG group in the
// caller's network namespace.
type nflog struct {
	fd  int
	buf []byte
}

// openNFLOG binds the NFLOG group, which needs CAP_NET_ADMIN in the
// network namespace and the kernel's nfnetlink_log.
func openNFLOG(group uint16) (*nflog, error) {
	fd, err := unix.Socket(unix.AF_NETLINK, unix.SOCK_RAW|unix.SOCK_CLOEXEC, unix.NETLINK_NETFILTER)
	if err != nil {
		return nil, fmt.Errorf("opening a netfilter netlink socket: %w", err)
	}
	n := &nflog{fd: fd, buf: make([]byte, 1<<16)}
	if err := unix.Bind(fd, &unix.SockaddrNetlink{Family: unix.AF_NETLINK}); err != nil {
		n.close()
		return nil, fmt.Errorf("binding a netfilter netlink socket: %w", err)
	}
	// A bigger buffer holds a burst up to the rate limit; drops that do
	// not fit are counted all the same.
	_ = unix.SetsockoptInt(fd, unix.SOL_SOCKET, unix.SO_RCVBUF, 1<<20)
	mode := make([]byte, 6)
	binary.BigEndian.PutUint32(mode, Snaplen)
	mode[4] = nfulnlCopyPacket
	for _, attr := range [][]byte{
		netlinkAttr(nfulaCfgCmd, []byte{nfulnlCfgCmdBind}),
		netlinkAttr(nfulaCfgMode, mode),
	} {
		if err := n.config(group, attr); err != nil {
			n.close()
			return nil, fmt.Errorf("binding NFLOG group %d: %w", group, err)
		}
	}
	return n, nil
}

func (n *nflog) close() { _ = unix.Close(n.fd) }

// config sends a configuration message for the group and waits for its
// acknowledgement.
func (n *nflog) config(group uint16, attr []byte) error {
	msg := make([]byte, nlmsgHdrLen+nfgenHdrLen, nlmsgHdrLen+nfgenHdrLen+len(attr))
	msg = append(msg, attr...)
	binary.NativeEndian.PutUint32(msg[0:], uint32(len(msg))) //nolint:gosec // a few bytes
	binary.NativeEndian.PutUint16(msg[4:], nfnlSubsysULOG<<8|nfulnlMsgConfig)
	binary.NativeEndian.PutUint16(msg[6:], unix.NLM_F_REQUEST|unix.NLM_F_ACK)
	binary.NativeEndian.PutUint32(msg[8:], 1)
	msg[nlmsgHdrLen] = unix.AF_UNSPEC
	binary.BigEndian.PutUint16(msg[nlmsgHdrLen+2:], group)
	if err := unix.Sendto(n.fd, msg, 0, &unix.SockaddrNetlink{Family: unix.AF_NETLINK}); err != nil {
		return err
	}
	for {
		m, _, err := unix.Recvfrom(n.fd, n.buf, 0)
		if err != nil {
			return err
		}
		for b := n.buf[:m]; len(b) >= nlmsgHdrLen; {
			l := int(binary.NativeEndian.Uint32(b))
			if l < nlmsgHdrLen || l > len(b) {
				break
			}
			if binary.NativeEndian.Uint16(b[4:]) == unix.NLMSG_ERROR && l >= nlmsgHdrLen+4 {
				if errno := int32(binary.NativeEndian.Uint32(b[nlmsgHdrLen:])); errno != 0 { //nolint:gosec // an errno
					return unix.Errno(-errno) //nolint:gosec // a negative errno
				}
				return nil
			}
			b = b[align(l):]
		}
	}
}

// read waits up to timeout for packets, and returns those that arrived. A
// zero timeout only takes what is there. Packets the socket had no room
// for are left out, which the counters make up for.
func (n *nflog) read(timeout time.Duration, now func() time.Time) ([]Packet, error) {
	fds := []unix.PollFd{{Fd: int32(n.fd), Events: unix.POLLIN}} //nolint:gosec // a file descriptor
	if _, err := unix.Poll(fds, int(timeout.Milliseconds())); err != nil && !errors.Is(err, unix.EINTR) {
		return nil, err
	}
	var out []Packet
	for {
		m, _, err := unix.Recvfrom(n.fd, n.buf, unix.MSG_DONTWAIT)
		switch {
		case errors.Is(err, unix.EAGAIN), errors.Is(err, unix.EINTR):
			return out, nil
		case errors.Is(err, unix.ENOBUFS):
			continue
		case err != nil:
			return out, err
		}
		out = append(out, parseMessages(n.buf[:m], now())...)
	}
}

// parseMessages reads the packets of the NFLOG messages in b, received at
// the time at.
func parseMessages(b []byte, at time.Time) []Packet {
	var out []Packet
	for len(b) >= nlmsgHdrLen {
		l := int(binary.NativeEndian.Uint32(b))
		if l < nlmsgHdrLen || l > len(b) {
			break
		}
		if binary.NativeEndian.Uint16(b[4:]) == nfnlSubsysULOG<<8|nfulnlMsgPacket && l >= nlmsgHdrLen+nfgenHdrLen {
			if p, ok := parsePacket(b[nlmsgHdrLen+nfgenHdrLen:l], at); ok {
				out = append(out, p)
			}
		}
		b = b[min(align(l), len(b)):]
	}
	return out
}

// parsePacket reads the attributes of an NFLOG packet message.
func parsePacket(b []byte, at time.Time) (Packet, bool) {
	p := Packet{At: at}
	var payload []byte
	for len(b) >= nlaHdrLen {
		l := int(binary.NativeEndian.Uint16(b))
		if l < nlaHdrLen || l > len(b) {
			break
		}
		v := b[nlaHdrLen:l]
		switch binary.NativeEndian.Uint16(b[2:]) & nlaTypeMask {
		case nfulaPrefix:
			if i := bytes.IndexByte(v, 0); i >= 0 {
				v = v[:i]
			}
			p.Reason = string(v)
		case nfulaPayload:
			payload = v
		case nfulaTimestamp:
			if len(v) == 16 {
				sec, usec := binary.BigEndian.Uint64(v), binary.BigEndian.Uint64(v[8:])
				if sec > 0 {
					p.At = time.Unix(int64(sec), int64(usec)*1000) //nolint:gosec // a time
				}
			}
		}
		b = b[min(align(l), len(b)):]
	}
	if p.Reason == "" || !parseIP(payload, &p) {
		return Packet{}, false
	}
	return p, true
}

// The IP protocols that brig names.
const (
	protoICMP   = 1
	protoTCP    = 6
	protoUDP    = 17
	protoICMPv6 = 58
	protoSCTP   = 132
)

// parseIP reads the protocol, destination address and port of an IPv4 or
// IPv6 packet, of which b may hold the start only.
func parseIP(b []byte, p *Packet) bool {
	if len(b) < 1 {
		return false
	}
	var proto, l4 int
	switch b[0] >> 4 {
	case 4:
		if len(b) < 20 {
			return false
		}
		proto, l4 = int(b[9]), int(b[0]&0xf)*4
		p.Addr = netip.AddrFrom4([4]byte(b[16:20]))
		if binary.BigEndian.Uint16(b[6:])&0x1fff != 0 {
			// A later fragment has no transport header.
			l4 = len(b)
		}
	case 6:
		if len(b) < 40 {
			return false
		}
		proto, l4 = int(b[6]), 40
		p.Addr = netip.AddrFrom16([16]byte(b[24:40]))
		proto, l4 = skipIPv6Headers(b, proto, l4)
	default:
		return false
	}
	switch proto {
	case protoICMP:
		p.Proto = "icmp"
	case protoTCP:
		p.Proto = "tcp"
	case protoUDP:
		p.Proto = "udp"
	case protoICMPv6:
		p.Proto = "icmpv6"
	case protoSCTP:
		p.Proto = "sctp"
	default:
		p.Proto = strconv.Itoa(proto)
	}
	if (proto == protoTCP || proto == protoUDP || proto == protoSCTP) && l4+4 <= len(b) {
		p.Port = int(binary.BigEndian.Uint16(b[l4+2:]))
	}
	return true
}

// skipIPv6Headers skips the IPv6 extension headers that may come before the
// transport header, and returns its protocol and offset.
func skipIPv6Headers(b []byte, proto, off int) (int, int) {
	for {
		switch proto {
		case 0, 43, 60: // hop-by-hop, routing, destination options
			if off+2 > len(b) {
				return proto, len(b)
			}
			proto, off = int(b[off]), off+(int(b[off+1])+1)*8
		case 44: // fragment
			if off+8 > len(b) {
				return proto, len(b)
			}
			if binary.BigEndian.Uint16(b[off+2:])&0xfff8 != 0 {
				// A later fragment has no transport header.
				return int(b[off]), len(b)
			}
			proto, off = int(b[off]), off+8
		default:
			return proto, off
		}
	}
}

// netlinkAttr encodes a netlink attribute, padded to 4 bytes.
func netlinkAttr(typ uint16, v []byte) []byte {
	b := make([]byte, align(nlaHdrLen+len(v)))
	binary.NativeEndian.PutUint16(b, uint16(nlaHdrLen+len(v))) //nolint:gosec // a few bytes
	binary.NativeEndian.PutUint16(b[2:], typ)
	copy(b[nlaHdrLen:], v)
	return b
}

func align(n int) int { return (n + 3) &^ 3 }
