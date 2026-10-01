//go:build darwin

package peerproc

import (
	"encoding/binary"
	"net/netip"
	"strings"
	"testing"
)

// fakePCB is one TCP pcb as pcbBuffer writes it: an xinpcb_n then its xsocket_n.
// Zero lengths mean the real ones.
type fakePCB struct {
	laddr, faddr    netip.AddrPort
	pid             int32
	inpLen, sockLen int
}

// pcbBuffer builds a net.inet.tcp.pcblist_n buffer from the layout constants in
// darwin.go: an xinpgen header, each pcb's records (with an unrelated 32-byte
// rcvbuf record after each socket, as the kernel writes more kinds than this package
// reads), and a trailing xinpgen.
func pcbBuffer(pcbs ...fakePCB) []byte {
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
		if p.laddr.Addr().Is4() {
			inp[inpVflag] = inpIPv4
			f4, l4 := p.faddr.Addr().As4(), p.laddr.Addr().As4()
			copy(inp[inpFaddr+12:], f4[:])
			copy(inp[inpLaddr+12:], l4[:])
		} else {
			inp[inpVflag] = inpIPv6
			f6, l6 := p.faddr.Addr().As16(), p.laddr.Addr().As16()
			copy(inp[inpFaddr:], f6[:])
			copy(inp[inpLaddr:], l6[:])
		}
		sock := make([]byte, (sl+7)&^7)
		le.PutUint32(sock, uint32(sl))
		le.PutUint32(sock[4:], xsoSocket)
		le.PutUint32(sock[soLastPID:], uint32(p.pid))
		rcv := make([]byte, 32)
		le.PutUint32(rcv, 32)
		le.PutUint32(rcv[4:], 0x002) // XSO_RCVBUF
		buf = append(buf, inp...)
		buf = append(buf, sock...)
		buf = append(buf, rcv...)
	}
	return append(buf, gen...)
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
