// Copyright (c) 2020 Tailscale Inc & AUTHORS All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package filter

import (
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"

	"inet.af/netaddr"
	"tailscale.com/types/logger"
	"tailscale.com/wgengine/packet"
)

// Type aliases only in test code: (but ideally nowhere)
type ParsedPacket = packet.ParsedPacket

var Unknown = packet.Unknown
var ICMP = packet.ICMP
var TCP = packet.TCP
var UDP = packet.UDP
var Fragment = packet.Fragment

func mustip(s string) netaddr.IP {
	ip, err := netaddr.ParseIP(s)
	if err != nil {
		panic(err)
	}
	return ip
}

func ipp(s string) netaddr.IPPort {
	ret, err := netaddr.ParseIPPort(s)
	if err != nil {
		panic(err)
	}
	return ret
}

func pfx(s string) netaddr.IPPrefix {
	p, err := netaddr.ParseIPPrefix(s)
	if err != nil {
		panic(err)
	}
	return p
}

func nets(ss ...string) []netaddr.IPPrefix {
	var ret []netaddr.IPPrefix
	for _, s := range ss {
		ip, err := netaddr.ParseIP(s)
		if err != nil {
			panic(err)
		}
		ret = append(ret, netaddr.IPPrefix{IP: ip, Bits: 32})
	}
	return ret
}

func netpr(s string, bits uint8, start, end uint16) NetPortRange {
	ip, err := netaddr.ParseIP(s)
	if err != nil {
		panic(err)
	}
	return NetPortRange{netaddr.IPPrefix{IP: ip, Bits: bits}, PortRange{start, end}}
}

func netprs(ip string, bits uint8, start, end uint16) []NetPortRange {
	return []NetPortRange{netpr(ip, bits, start, end)}
}

var matches = Matches{
	{
		Srcs: nets("8.1.1.1", "8.2.2.2"),
		Dsts: []NetPortRange{
			netpr("1.2.3.4", 32, 22, 22),
			netpr("5.6.7.8", 32, 23, 24),
		},
	},
	{
		Srcs: nets("8.1.1.1", "8.2.2.2"),
		Dsts: netprs("5.6.7.8", 32, 27, 28),
	},
	{
		Srcs: nets("2.2.2.2"),
		Dsts: netprs("8.1.1.1", 32, 22, 22),
	},
	{
		Srcs: []netaddr.IPPrefix{NetAny},
		Dsts: netprs("100.122.98.50", 32, 0, 65535),
	},
	{
		Srcs: []netaddr.IPPrefix{NetAny},
		Dsts: netprs("0.0.0.0", 0, 443, 443),
	},
	{
		Srcs: nets("153.1.1.1", "153.1.1.2", "153.3.3.3"),
		Dsts: netprs("1.2.3.4", 32, 999, 999),
	},
}

func newFilter(logf logger.Logf) *Filter {
	// Expects traffic to 100.122.98.50, 1.2.3.4, 5.6.7.8,
	// 102.102.102.102, 119.119.119.119, 8.1.0.0/16
	localNets := nets("100.122.98.50", "1.2.3.4", "5.6.7.8", "102.102.102.102", "119.119.119.119")
	localNets = append(localNets, pfx("8.1.0.0/16"))

	return New(matches, localNets, nil, logf)
}

func TestMarshal(t *testing.T) {
	for _, ent := range []Matches{Matches{matches[0]}, matches} {
		b, err := json.Marshal(ent)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}

		mm2 := Matches{}
		if err := json.Unmarshal(b, &mm2); err != nil {
			t.Fatalf("unmarshal: %v (%v)", err, string(b))
		}
	}
}

func TestFilter(t *testing.T) {
	acl := newFilter(t.Logf)
	// check packet filtering based on the table

	type InOut struct {
		want Response
		p    ParsedPacket
	}
	tests := []InOut{
		// Basic
		{Accept, parsed(TCP, mustip("8.1.1.1"), mustip("1.2.3.4"), 999, 22)},
		{Accept, parsed(UDP, mustip("8.1.1.1"), mustip("1.2.3.4"), 999, 22)},
		{Accept, parsed(ICMP, mustip("8.1.1.1"), mustip("1.2.3.4"), 0, 0)},
		{Drop, parsed(TCP, mustip("8.1.1.1"), mustip("1.2.3.4"), 0, 0)},
		{Accept, parsed(TCP, mustip("8.1.1.1"), mustip("1.2.3.4"), 0, 22)},
		{Drop, parsed(TCP, mustip("8.1.1.1"), mustip("1.2.3.4"), 0, 21)},
		{Accept, parsed(TCP, mustip("17.34.51.68"), mustip("8.1.34.51"), 0, 443)},
		{Drop, parsed(TCP, mustip("17.34.51.68"), mustip("8.1.34.51"), 0, 444)},
		{Accept, parsed(TCP, mustip("17.34.51.68"), mustip("100.122.98.50"), 0, 999)},
		{Accept, parsed(TCP, mustip("17.34.51.68"), mustip("100.122.98.50"), 0, 0)},

		// localNets prefilter - accepted by policy filter, but
		// unexpected dst IP.
		{Drop, parsed(TCP, mustip("8.1.1.1"), mustip("16.32.48.64"), 0, 443)},

		// Stateful UDP. Note each packet is run through the input
		// filter, then the output filter (which sets conntrack
		// state).
		// Initially empty cache
		{Drop, parsed(UDP, mustip("119.119.119.119"), mustip("102.102.102.102"), 4242, 4343)},
		// Return packet from previous attempt is allowed
		{Accept, parsed(UDP, mustip("102.102.102.102"), mustip("119.119.119.119"), 4343, 4242)},
		// Because of the return above, initial attempt is allowed now
		{Accept, parsed(UDP, mustip("119.119.119.119"), mustip("102.102.102.102"), 4242, 4343)},
	}
	for i, test := range tests {
		if got, _ := acl.runIn(&test.p); test.want != got {
			t.Errorf("#%d got=%v want=%v packet:%v\n", i, got, test.want, test.p)
		}
		// Update UDP state
		_, _ = acl.runOut(&test.p)
	}
}

func TestNoAllocs(t *testing.T) {
	acl := newFilter(t.Logf)

	tcpPacket := rawpacket(TCP, mustip("8.1.1.1"), mustip("1.2.3.4"), 999, 22, 0)
	udpPacket := rawpacket(UDP, mustip("8.1.1.1"), mustip("1.2.3.4"), 999, 22, 0)

	tests := []struct {
		name   string
		in     bool
		want   int
		packet []byte
	}{
		{"tcp_in", true, 2, tcpPacket},
		{"tcp_out", false, 2, tcpPacket},
		{"udp_in", true, 2, udpPacket},
		// One alloc is inevitable (an lru cache update)
		{"udp_out", false, 3, udpPacket},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := int(testing.AllocsPerRun(1000, func() {
				q := &ParsedPacket{}
				q.Decode(test.packet)
				if test.in {
					acl.RunIn(q, 0)
				} else {
					acl.RunOut(q, 0)
				}
			}))

			if got > test.want {
				t.Errorf("got %d allocs per run; want at most %d", got, test.want)
			}
		})
	}
}

func TestParseIP(t *testing.T) {
	tests := []struct {
		host    string
		bits    int
		want    netaddr.IPPrefix
		wantErr string
	}{
		{"8.8.8.8", 24, pfx("8.8.8.8/24"), ""},
		{"8.8.8.8", 33, netaddr.IPPrefix{}, `invalid CIDR size 33 for host "8.8.8.8"`},
		{"8.8.8.8", -1, netaddr.IPPrefix{}, `invalid CIDR size -1 for host "8.8.8.8"`},
		{"0.0.0.0", 24, netaddr.IPPrefix{}, `ports="0.0.0.0": to allow all IP addresses, use *:port, not 0.0.0.0:port`},
		{"*", 24, NetAny, ""},
		{"fe80::1", 128, NetNone, `ports="fe80::1": invalid IPv4 address`},
	}
	for _, tt := range tests {
		got, err := parseIP(tt.host, tt.bits)
		if err != nil {
			if err.Error() == tt.wantErr {
				continue
			}
			t.Errorf("parseIP(%q, %v) error: %v; want error %q", tt.host, tt.bits, err, tt.wantErr)
		}
		if got != tt.want {
			t.Errorf("parseIP(%q, %v) = %#v; want %#v", tt.host, tt.bits, got, tt.want)
			continue
		}
	}
}

func BenchmarkFilter(b *testing.B) {
	acl := newFilter(b.Logf)

	tcpPacket := rawpacket(TCP, mustip("8.1.1.1"), mustip("1.2.3.4"), 999, 22, 0)
	udpPacket := rawpacket(UDP, mustip("8.1.1.1"), mustip("1.2.3.4"), 999, 22, 0)
	icmpPacket := rawpacket(ICMP, mustip("8.1.1.1"), mustip("1.2.3.4"), 0, 0, 0)

	tcpSynPacket := rawpacket(TCP, mustip("8.1.1.1"), mustip("1.2.3.4"), 999, 22, 0)
	// TCP filtering is trivial (Accept) for non-SYN packets.
	tcpSynPacket[33] = packet.TCPSyn

	benches := []struct {
		name   string
		in     bool
		packet []byte
	}{
		// Non-SYN TCP and ICMP have similar code paths in and out.
		{"icmp", true, icmpPacket},
		{"tcp", true, tcpPacket},
		{"tcp_syn_in", true, tcpSynPacket},
		{"tcp_syn_out", false, tcpSynPacket},
		{"udp_in", true, udpPacket},
		{"udp_out", false, udpPacket},
	}

	for _, bench := range benches {
		b.Run(bench.name, func(b *testing.B) {
			for i := 0; i < b.N; i++ {
				q := &ParsedPacket{}
				q.Decode(bench.packet)
				// This branch seems to have no measurable impact on performance.
				if bench.in {
					acl.RunIn(q, 0)
				} else {
					acl.RunOut(q, 0)
				}
			}
		})
	}
}

func TestPreFilter(t *testing.T) {
	packets := []struct {
		desc string
		want Response
		b    []byte
	}{
		{"empty", Accept, []byte{}},
		{"short", Drop, []byte("short")},
		{"junk", Drop, rawdefault(Unknown, 10)},
		{"fragment", Accept, rawdefault(Fragment, 40)},
		{"tcp", noVerdict, rawdefault(TCP, 200)},
		{"udp", noVerdict, rawdefault(UDP, 200)},
		{"icmp", noVerdict, rawdefault(ICMP, 200)},
	}
	f := NewAllowNone(t.Logf)
	for _, testPacket := range packets {
		p := &ParsedPacket{}
		p.Decode(testPacket.b)
		got := f.pre(p, LogDrops|LogAccepts, in)
		if got != testPacket.want {
			t.Errorf("%q got=%v want=%v packet:\n%s", testPacket.desc, got, testPacket.want, packet.Hexdump(testPacket.b))
		}
	}
}

func parsed(proto packet.IPProto, src, dst netaddr.IP, sport, dport uint16) ParsedPacket {
	return ParsedPacket{
		IPProto:  proto,
		Src:      netaddr.IPPort{IP: src, Port: sport},
		Dst:      netaddr.IPPort{IP: dst, Port: dport},
		TCPFlags: packet.TCPSyn,
	}
}

// rawpacket generates a packet with given source and destination ports and IPs
// and resizes the header to trimLength if it is nonzero.
func rawpacket(proto packet.IPProto, src, dst netaddr.IP, sport, dport uint16, trimLength int) []byte {
	var headerLength int

	switch proto {
	case ICMP:
		headerLength = 24
	case TCP:
		headerLength = 40
	case UDP:
		headerLength = 28
	default:
		headerLength = 24
	}
	if trimLength > headerLength {
		headerLength = trimLength
	}
	if trimLength == 0 {
		trimLength = headerLength
	}

	bin := binary.BigEndian
	hdr := make([]byte, headerLength)
	hdr[0] = 0x45
	bin.PutUint16(hdr[2:4], uint16(trimLength))
	hdr[8] = 64
	s, d := src.As4(), dst.As4()
	copy(hdr[12:16], s[:])
	copy(hdr[16:20], d[:])
	// ports
	bin.PutUint16(hdr[20:22], sport)
	bin.PutUint16(hdr[22:24], dport)

	switch proto {
	case ICMP:
		hdr[9] = 1
	case TCP:
		hdr[9] = 6
	case UDP:
		hdr[9] = 17
	case Fragment:
		hdr[9] = 6
		// flags + fragOff
		bin.PutUint16(hdr[6:8], (1<<13)|1234)
	case Unknown:
	default:
		panic("unknown protocol")
	}

	// Trim the header if requested
	hdr = hdr[:trimLength]

	return hdr
}

// rawdefault calls rawpacket with default ports and IPs.
func rawdefault(proto packet.IPProto, trimLength int) []byte {
	ip := mustip("8.8.8.8")
	port := uint16(53)
	return rawpacket(proto, ip, ip, port, port, trimLength)
}

func parseHexPkt(t *testing.T, h string) *packet.ParsedPacket {
	t.Helper()
	b, err := hex.DecodeString(strings.ReplaceAll(h, " ", ""))
	if err != nil {
		t.Fatalf("failed to read hex %q: %v", h, err)
	}
	p := new(packet.ParsedPacket)
	p.Decode(b)
	return p
}

func TestOmitDropLogging(t *testing.T) {
	tests := []struct {
		name string
		pkt  *packet.ParsedPacket
		dir  direction
		want bool
	}{
		{
			name: "v4_tcp_out",
			pkt:  &packet.ParsedPacket{IPVersion: 4, IPProto: packet.TCP},
			dir:  out,
			want: false,
		},
		{
			name: "v6_icmp_out", // as seen on Linux
			pkt:  parseHexPkt(t, "60 00 00 00 00 00 3a 00   fe800000000000000000000000000000 ff020000000000000000000000000002"),
			dir:  out,
			want: true,
		},
		{
			name: "v6_to_MLDv2_capable_routers", // as seen on Windows
			pkt:  parseHexPkt(t, "60 00 00 00 00 24 00 01 00 00 00 00 00 00 00 00 00 00 00 00 00 00 00 00 ff 02 00 00 00 00 00 00 00 00 00 00 00 00 00 16 3a 00 05 02 00 00 01 00 8f 00 6e 80 00 00 00 01 04 00 00 00 ff 02 00 00 00 00 00 00 00 00 00 00 00 00 00 0c"),
			dir:  out,
			want: true,
		},
		{
			name: "v4_igmp_out", // on Windows, from https://github.com/tailscale/tailscale/issues/618
			pkt:  parseHexPkt(t, "46 00 00 30 37 3a 00 00 01 02 10 0e a9 fe 53 6b e0 00 00 16 94 04 00 00 22 00 14 05 00 00 00 02 04 00 00 00 e0 00 00 fb 04 00 00 00 e0 00 00 fc"),
			dir:  out,
			want: true,
		},
		{
			name: "v6_udp_multicast",
			pkt:  parseHexPkt(t, "60 00 00 00 00 00 11 00  fe800000000000007dc6bc04499262a3 ff120000000000000000000000008384"),
			dir:  out,
			want: true,
		},
		{
			name: "v4_multicast_out_low",
			pkt:  &packet.ParsedPacket{IPVersion: 4, Dst: ipp("224.0.0.0:0")},
			dir:  out,
			want: true,
		},
		{
			name: "v4_multicast_out_high",
			pkt:  &packet.ParsedPacket{IPVersion: 4, Dst: ipp("239.255.255.255:0")},
			dir:  out,
			want: true,
		},
		{
			name: "v4_link_local_unicast",
			pkt:  &packet.ParsedPacket{IPVersion: 4, Dst: ipp("169.254.1.2:0")},
			dir:  out,
			want: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := omitDropLogging(tt.pkt, tt.dir)
			if got != tt.want {
				t.Errorf("got %v; want %v\npacket: %#v\n%s", got, tt.want, tt.pkt, packet.Hexdump(tt.pkt.Buffer()))
			}
		})
	}
}
