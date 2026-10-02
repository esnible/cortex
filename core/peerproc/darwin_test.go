//go:build darwin

package peerproc

import (
	"encoding/binary"
	"errors"
	"net/netip"
	"strings"
	"testing"
)

// fakePCB is one TCP pcb as pcbBuffer writes it: an xinpcb_n then its xsocket_n.
// Zero lengths mean the real ones; a zero vflag is derived from laddr's family.
type fakePCB struct {
	laddr, faddr    netip.AddrPort
	pid             int32
	opts            uint32 // so_options
	vflag           byte
	inpLen, sockLen int
}

// pcbBufferWithCount builds a net.inet.tcp.pcblist_n buffer from the layout constants in
// darwin.go: an xinpgen header, each pcb's records (with an unrelated 32-byte
// rcvbuf record after each socket, as the kernel writes more kinds than this package
// reads), and a trailing xinpgen whose second word is count (the host's TCP socket count).
func pcbBufferWithCount(count uint32, pcbs ...fakePCB) []byte {
	le := binary.LittleEndian
	gen := make([]byte, 24)
	le.PutUint32(gen, 24)
	buf := append([]byte(nil), gen...)
	for _, p := range pcbs {
		il, sl := p.inpLen, p.sockLen
		if il == 0 {
			il = inpcbLen
		}
		if sl == 0 {
			sl = socketLen
		}
		inp := make([]byte, (il+7)&^7)
		le.PutUint32(inp, uint32(il))
		le.PutUint32(inp[4:], xsoInpcb)
		binary.BigEndian.PutUint16(inp[inpFport:], p.faddr.Port())
		binary.BigEndian.PutUint16(inp[inpLport:], p.laddr.Port())
		vflag := p.vflag
		if p.laddr.Addr().Is4() {
			if vflag == 0 {
				vflag = inpIPv4
			}
			f4, l4 := p.faddr.Addr().As4(), p.laddr.Addr().As4()
			copy(inp[inpFaddr+12:], f4[:])
			copy(inp[inpLaddr+12:], l4[:])
		} else {
			if vflag == 0 {
				vflag = inpIPv6
			}
			f6, l6 := p.faddr.Addr().As16(), p.laddr.Addr().As16()
			copy(inp[inpFaddr:], f6[:])
			copy(inp[inpLaddr:], l6[:])
		}
		inp[inpVflag] = vflag
		sock := make([]byte, (sl+7)&^7)
		le.PutUint32(sock, uint32(sl))
		le.PutUint32(sock[4:], xsoSocket)
		le.PutUint32(sock[soOptions:], p.opts)
		le.PutUint32(sock[soLastPID:], uint32(p.pid))
		rcv := make([]byte, 32)
		le.PutUint32(rcv, 32)
		le.PutUint32(rcv[4:], 0x002) // XSO_RCVBUF
		buf = append(buf, inp...)
		buf = append(buf, sock...)
		buf = append(buf, rcv...)
	}
	trailer := make([]byte, 24)
	le.PutUint32(trailer, 24)
	le.PutUint32(trailer[4:], count) // xig_count: socket count, not a record kind
	return append(buf, trailer...)
}

// pcbBuffer builds a pcblist_n with socket count 0 (the leading header's value).
func pcbBuffer(pcbs ...fakePCB) []byte {
	return pcbBufferWithCount(0, pcbs...)
}

func TestWalkPCBs_ReadsIPv4AndIPv6(t *testing.T) {
	want := []fakePCB{
		{laddr: netip.MustParseAddrPort("192.168.1.167:57207"), faddr: netip.MustParseAddrPort("1.1.1.1:443"), pid: 4242},
		{laddr: netip.MustParseAddrPort("[::1]:57211"), faddr: netip.MustParseAddrPort("[::1]:57210"), pid: 77},
		{laddr: netip.MustParseAddrPort("127.0.0.1:47600"), faddr: netip.AddrPortFrom(netip.IPv4Unspecified(), 0), pid: 25908},
	}
	var got []pcb
	if err := walkPCBs(pcbBuffer(want...), func(p pcb) bool { got = append(got, p); return false }); err != nil {
		t.Fatalf("walkPCBs: %v", err)
	}
	if len(got) != len(want) {
		t.Fatalf("%d pcbs, want %d: %+v", len(got), len(want), got)
	}
	for i := range want {
		if got[i].laddr != want[i].laddr || got[i].faddr != want[i].faddr || got[i].pid != want[i].pid {
			t.Errorf("pcb %d = %+v, want laddr %s faddr %s pid %d", i, got[i], want[i].laddr, want[i].faddr, want[i].pid)
		}
	}
}

func TestWalkPCBs_StopsWhenTold(t *testing.T) {
	buf := pcbBuffer(
		fakePCB{laddr: netip.MustParseAddrPort("127.0.0.1:1"), faddr: netip.MustParseAddrPort("127.0.0.1:2"), pid: 1},
		fakePCB{laddr: netip.MustParseAddrPort("127.0.0.1:3"), faddr: netip.MustParseAddrPort("127.0.0.1:4"), pid: 2},
	)
	n := 0
	if err := walkPCBs(buf, func(pcb) bool { n++; return true }); err != nil || n != 1 {
		t.Errorf("visited %d pcbs (err %v), want 1", n, err)
	}
}

// A record of another length means the kernel's layout changed; reading it at the old
// offsets would name the wrong process, so the walk refuses.
func TestWalkPCBs_RefusesAnUnknownLayout(t *testing.T) {
	ap := netip.MustParseAddrPort("127.0.0.1:1")
	for name, p := range map[string]fakePCB{
		"xinpcb_n":  {laddr: ap, faddr: ap, pid: 1, inpLen: inpcbLen + 8},
		"xsocket_n": {laddr: ap, faddr: ap, pid: 1, sockLen: socketLen - 8},
	} {
		err := walkPCBs(pcbBuffer(p), func(pcb) bool { return false })
		if err == nil || !strings.Contains(err.Error(), name) {
			t.Errorf("%s with a changed length: err = %v, want one naming %s", name, err, name)
		}
	}
}

func TestWalkPCBs_RejectsATruncatedBuffer(t *testing.T) {
	if err := walkPCBs([]byte{1, 2}, func(pcb) bool { return false }); err == nil {
		t.Error("walkPCBs accepted a 2-byte buffer")
	}
}

// The table's trailing xinpgen carries the host's socket count where a record carries
// its kind. Counts that collide with the two kinds read here must not be taken for them.
func TestWalkPCBs_IgnoresTheTrailerWhateverTheSocketCount(t *testing.T) {
	p := fakePCB{laddr: netip.MustParseAddrPort("127.0.0.1:1"), faddr: netip.MustParseAddrPort("127.0.0.1:2"), pid: 9}
	for _, count := range []uint32{xsoSocket, xsoInpcb, 183} {
		n := 0
		err := walkPCBs(pcbBufferWithCount(count, p), func(pcb) bool { n++; return false })
		if err != nil || n != 1 {
			t.Errorf("trailer count %d: visited %d pcbs, err %v; want 1 and nil", count, n, err)
		}
	}
}

// listening is a listener on addr ("127.0.0.1:80", "[::]:80"), as the kernel records
// one: SO_ACCEPTCONN set and no foreign address. vflag 0 derives it from the address.
func listening(addr string, pid int32, vflag byte) fakePCB {
	ap := netip.MustParseAddrPort(addr)
	none := netip.IPv6Unspecified()
	if ap.Addr().Is4() {
		none = netip.IPv4Unspecified()
	}
	return fakePCB{laddr: ap, faddr: netip.AddrPortFrom(none, 0), pid: pid, opts: soAcceptConn | 0x4, vflag: vflag}
}

const dualStack = inpIPv4 | inpIPv6

// pickListenerCases list pcbs in buffer order, which is the kernel's: newest first.
// want 0 is ErrNotFound.
var pickListenerCases = []struct {
	name  string
	pcbs  []fakePCB
	query string
	want  int32
}{
	{"an exact bind before a wildcard",
		[]fakePCB{listening("127.0.0.1:80", 2, 0), listening("0.0.0.0:80", 1, 0)}, "127.0.0.1:80", 2},
	{"an exact bind after a wildcard",
		[]fakePCB{listening("0.0.0.0:80", 1, 0), listening("127.0.0.1:80", 2, 0)}, "127.0.0.1:80", 2},
	{"an exact IPv6 bind beside an IPv4 wildcard",
		[]fakePCB{listening("0.0.0.0:80", 1, 0), listening("[::1]:80", 2, 0)}, "[::1]:80", 2},
	{"an IPv4 wildcard does not answer IPv6",
		[]fakePCB{listening("0.0.0.0:80", 1, 0)}, "[::1]:80", 0},
	{"a v6-only wildcard does not answer IPv4",
		[]fakePCB{listening("[::]:80", 1, inpIPv6)}, "127.0.0.1:80", 0},
	{"a v6-only wildcard answers IPv6",
		[]fakePCB{listening("[::]:80", 1, inpIPv6)}, "[::1]:80", 1},
	{"a dual-stack wildcard answers IPv4",
		[]fakePCB{listening("[::]:80", 3, dualStack)}, "127.0.0.1:80", 3},
	{"a dual-stack wildcard answers IPv6",
		[]fakePCB{listening("[::]:80", 3, dualStack)}, "[::1]:80", 3},
	{"an IPv4-mapped query is an IPv4 one",
		[]fakePCB{listening("127.0.0.1:80", 2, 0)}, "[::ffff:127.0.0.1]:80", 2},
	// The kernel prefers a 0.0.0.0 listener over a dual-stack [::] one for IPv4 whichever
	// is newer; macOS lets the two share a port without SO_REUSEPORT.
	{"0.0.0.0 before a newer dual-stack [::], for IPv4",
		[]fakePCB{listening("[::]:80", 3, dualStack), listening("0.0.0.0:80", 1, 0)}, "127.0.0.1:80", 1},
	{"0.0.0.0 before an older dual-stack [::], for IPv4",
		[]fakePCB{listening("0.0.0.0:80", 1, 0), listening("[::]:80", 3, dualStack)}, "127.0.0.1:80", 1},
	{"a dual-stack [::] beside 0.0.0.0, for IPv6",
		[]fakePCB{listening("[::]:80", 3, dualStack), listening("0.0.0.0:80", 1, 0)}, "[::1]:80", 3},
	// SO_REUSEPORT: the newest exact bind, the oldest wildcard.
	{"the newest of two exact binds",
		[]fakePCB{listening("127.0.0.1:80", 8, 0), listening("127.0.0.1:80", 7, 0)}, "127.0.0.1:80", 8},
	{"the oldest of two wildcards",
		[]fakePCB{listening("0.0.0.0:80", 6, 0), listening("0.0.0.0:80", 5, 0)}, "127.0.0.1:80", 5},
	{"a socket only bound is no listener, though its foreign port is 0",
		[]fakePCB{{laddr: netip.MustParseAddrPort("127.0.0.1:80"), faddr: netip.MustParseAddrPort("0.0.0.0:0"), pid: 4}},
		"127.0.0.1:80", 0},
	{"a socket only bound does not hide the wildcard listener",
		[]fakePCB{{laddr: netip.MustParseAddrPort("127.0.0.1:80"), faddr: netip.MustParseAddrPort("0.0.0.0:0"), pid: 4},
			listening("0.0.0.0:80", 1, 0)}, "127.0.0.1:80", 1},
	{"a connected socket is no listener",
		[]fakePCB{{laddr: netip.MustParseAddrPort("127.0.0.1:80"), faddr: netip.MustParseAddrPort("127.0.0.1:5000"), pid: 4, opts: 0xc}},
		"127.0.0.1:80", 0},
	{"a listener whose pid is 0 is passed over",
		[]fakePCB{listening("127.0.0.1:80", 0, 0), listening("0.0.0.0:80", 1, 0)}, "127.0.0.1:80", 1},
	{"a listener whose pid is 0 is not found",
		[]fakePCB{listening("127.0.0.1:80", 0, 0)}, "127.0.0.1:80", 0},
	{"nothing on the port",
		[]fakePCB{listening("127.0.0.1:81", 2, 0), listening("0.0.0.0:81", 1, 0)}, "127.0.0.1:80", 0},
}

func TestPickListener(t *testing.T) {
	for _, tc := range pickListenerCases {
		t.Run(tc.name, func(t *testing.T) {
			pid, err := pickListener(pcbBuffer(tc.pcbs...), netip.MustParseAddrPort(tc.query))
			switch {
			case tc.want == 0 && !errors.Is(err, ErrNotFound):
				t.Errorf("pickListener(%s) = %d, %v; want ErrNotFound", tc.query, pid, err)
			case tc.want != 0 && (err != nil || pid != tc.want):
				t.Errorf("pickListener(%s) = %d, %v; want %d", tc.query, pid, err, tc.want)
			}
		})
	}
}

func TestPickListener_PassesOnALayoutError(t *testing.T) {
	buf := pcbBuffer(fakePCB{laddr: netip.MustParseAddrPort("127.0.0.1:80"), faddr: netip.MustParseAddrPort("0.0.0.0:0"), pid: 1, inpLen: inpcbLen + 8})
	if _, err := pickListener(buf, netip.MustParseAddrPort("127.0.0.1:80")); err == nil || errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v, want walkPCBs's layout error", err)
	}
}
