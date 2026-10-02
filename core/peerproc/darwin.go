//go:build darwin

package peerproc

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"net/netip"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"
)

// The layout of net.inet.tcp.pcblist_n, from xnu's bsd/netinet/in_pcb.h and
// bsd/sys/socketvar.h. The structs are #pragma pack(4) and each record is padded to 8
// bytes. For every TCP pcb the kernel writes an xinpcb_n, then that pcb's xsocket_n,
// then buffer, statistics and tcpcb records this package skips.
//
// Confirmed against macOS 26.6.2 on 2026-10-01. A record of either kind with another
// length means the layout has changed, and walkPCBs refuses rather than reading the
// wrong bytes as a PID.
const (
	xsoSocket = 0x001
	xsoInpcb  = 0x010

	inpcbLen  = 104
	socketLen = 104

	inpFport = 16 // u_short, network order
	inpLport = 18 // u_short, network order
	inpVflag = 44
	inpFaddr = 48 // a 16-byte union; an IPv4 address is its last 4 bytes
	inpLaddr = 64

	soOptions = 20 // u_int32: the socket's SO_* options
	soState   = 26 // short: the socket's SS_* state
	soLastPID = 68 // pid_t: the last process to use the socket

	// soAcceptConn is SO_ACCEPTCONN, which listen(2) sets. A socket bound to a port but
	// never listened on also has foreign port 0, so this flag is what tells a listener
	// apart. On the live kernel so_options reads 0x6 on a listener, 0x8 on a client
	// socket, 0xc on an accepted one and 0 on a socket that is only bound.
	soAcceptConn = 0x2

	// ssNoFDRef is SS_NOFDREF: no file descriptor refers to the socket. A connection its
	// process has closed stays in the table a while with so_last_pid unchanged. On the live
	// kernel so_state reads 0x102 on an open client socket, 0x13b once its process has
	// closed it and 0x2131 once the server end has closed too.
	ssNoFDRef = 0x1

	// inp_vflag. A listener on [::] without IPV6_V6ONLY — Go's default for "tcp" with no
	// host — carries both.
	inpIPv4 = 0x1
	inpIPv6 = 0x2
)

type darwin struct{}

func newPlatform() (Resolver, error) { return darwin{}, nil }

// pcb is what this package reads from one xinpcb_n and its xsocket_n.
type pcb struct {
	laddr, faddr netip.AddrPort
	pid          int32 // 0 when no process holds the socket, or none was recorded
	vflag        byte
	wildcard     bool // the local address is all zero: 0.0.0.0 or [::]
	listening    bool // SO_ACCEPTCONN
}

// walkPCBs calls fn for each TCP pcb in a pcblist_n buffer until fn returns true. It
// fails on a buffer too short to hold its header, and on a record of a kind it reads
// whose length is not the one this package was written for.
func walkPCBs(buf []byte, fn func(pcb) bool) error {
	if len(buf) < 4 {
		return fmt.Errorf("pcblist_n: %d bytes", len(buf))
	}
	le := binary.LittleEndian
	var cur *pcb
	hdr := int(le.Uint32(buf[0:4]))
	off := hdr // past the leading xinpgen
	for off+8 <= len(buf) {
		l, kind := int(le.Uint32(buf[off:])), le.Uint32(buf[off+4:])
		if l < 8 || off+l > len(buf) {
			break // the end of the buffer
		}
		// The kernel closes the table with a second xinpgen, the same size as the first,
		// whose second word is the host's TCP socket count rather than a record kind. Read
		// as a kind it would pass for an xsocket_n or an xinpcb_n whenever that count is
		// exactly 1 or 16, and the length check below would then fail every lookup.
		if l == hdr && off+l == len(buf) {
			break
		}
		rec := buf[off : off+l]
		switch kind {
		case xsoInpcb:
			if l != inpcbLen {
				return fmt.Errorf("pcblist_n: xinpcb_n is %d bytes, want %d", l, inpcbLen)
			}
			p := parseInpcb(rec)
			cur = &p
		case xsoSocket:
			if l != socketLen {
				return fmt.Errorf("pcblist_n: xsocket_n is %d bytes, want %d", l, socketLen)
			}
			if cur != nil {
				if le.Uint16(rec[soState:])&ssNoFDRef == 0 {
					cur.pid = int32(le.Uint32(rec[soLastPID:]))
				}
				cur.listening = le.Uint32(rec[soOptions:])&soAcceptConn != 0
				if fn(*cur) {
					return nil
				}
				cur = nil
			}
		}
		off += (l + 7) &^ 7
	}
	return nil
}

// parseInpcb reads an xinpcb_n's addresses. A socket marked IPv4 — including an IPv6
// socket connected to an IPv4-mapped address, which the kernel re-marks, and a
// dual-stack [::] listener, marked both — keeps its addresses in the last 4 bytes of
// each union.
func parseInpcb(rec []byte) pcb {
	fport := binary.BigEndian.Uint16(rec[inpFport:])
	lport := binary.BigEndian.Uint16(rec[inpLport:])
	vflag := rec[inpVflag]
	var fa, la netip.Addr
	if vflag&inpIPv4 != 0 {
		fa = netip.AddrFrom4([4]byte(rec[inpFaddr+12 : inpFaddr+16]))
		la = netip.AddrFrom4([4]byte(rec[inpLaddr+12 : inpLaddr+16]))
	} else {
		fa = netip.AddrFrom16([16]byte(rec[inpFaddr : inpFaddr+16])).Unmap()
		la = netip.AddrFrom16([16]byte(rec[inpLaddr : inpLaddr+16])).Unmap()
	}
	return pcb{
		laddr:    netip.AddrPortFrom(la, lport),
		faddr:    netip.AddrPortFrom(fa, fport),
		vflag:    vflag,
		wildcard: [16]byte(rec[inpLaddr:inpLaddr+16]) == [16]byte{},
	}
}

// pickListener is the pid of the listener a connection to addr reaches, chosen from a
// pcblist_n buffer in the order the kernel's own lookup uses:
//
//  1. a listener bound to exactly addr;
//  2. else a wildcard listener of addr's family — for an IPv4 addr a 0.0.0.0 listener
//     before a dual-stack [::] one, whichever is newer.
//
// Sockets that are not listening, and any whose pid is unknown, are passed over. Where
// SO_REUSEPORT lets several listeners share a rank the kernel takes the newest exact
// bind but the oldest wildcard, and so does this: the table lists sockets newest first,
// so the first exact match is final while each wildcard match replaces the one before.
// ErrNotFound when no listener matches.
func pickListener(buf []byte, addr netip.AddrPort) (int32, error) {
	addr = unmapAddrPort(addr)
	v4 := addr.Addr().Is4()
	var exact, wild, dualStack int32
	err := walkPCBs(buf, func(p pcb) bool {
		if !p.listening || p.pid <= 0 || p.laddr.Port() != addr.Port() {
			return false
		}
		switch {
		case !p.wildcard:
			if p.laddr.Addr() == addr.Addr() {
				exact = p.pid
				return true
			}
		case v4 && p.vflag&inpIPv4 != 0 && p.vflag&inpIPv6 != 0:
			dualStack = p.pid
		case v4 && p.vflag&inpIPv4 != 0, !v4 && p.vflag&inpIPv6 != 0:
			wild = p.pid
		}
		return false
	})
	if err != nil {
		return 0, err
	}
	for _, pid := range []int32{exact, wild, dualStack} {
		if pid != 0 {
			return pid, nil
		}
	}
	return 0, ErrNotFound
}

// connPID is the pid holding the client end of the connection between client and server,
// from a pcblist_n buffer. ErrNotFound when no process holds it.
func connPID(buf []byte, client, server netip.AddrPort) (int32, error) {
	client, server = unmapAddrPort(client), unmapAddrPort(server)
	var pid int32
	err := walkPCBs(buf, func(p pcb) bool {
		if p.pid > 0 && p.laddr == client && p.faddr == server {
			pid = p.pid
			return true
		}
		return false
	})
	if err != nil {
		return 0, err
	}
	if pid == 0 {
		return 0, ErrNotFound
	}
	return pid, nil
}

func (darwin) ConnOwner(client, server netip.AddrPort, _ ...int32) (Proc, error) {
	buf, err := unix.SysctlRaw("net.inet.tcp.pcblist_n")
	if err != nil {
		return Proc{}, err
	}
	pid, err := connPID(buf, client, server)
	if err != nil {
		return Proc{}, err
	}
	return procInfo(pid)
}

func (darwin) ListenerOwner(addr netip.AddrPort, _ ...int32) (Proc, error) {
	buf, err := unix.SysctlRaw("net.inet.tcp.pcblist_n")
	if err != nil {
		return Proc{}, err
	}
	pid, err := pickListener(buf, addr)
	if err != nil {
		return Proc{}, err
	}
	return procInfo(pid)
}

func (darwin) Ancestry(pid int32, max int) ([]Proc, error) { return ancestry(procInfo, pid, max) }

// procInfo reads one process from kinfo_proc, which any process may read for any other.
func procInfo(pid int32) (Proc, error) {
	kp, err := unix.SysctlKinfoProc("kern.proc.pid", int(pid))
	if err != nil || kp.Proc.P_pid != pid {
		return Proc{}, ErrNotFound
	}
	tv := kp.Proc.P_starttime
	return Proc{
		PID:   pid,
		PPID:  kp.Eproc.Ppid,
		Start: time.Unix(tv.Sec, int64(tv.Usec)*1000),
		Exe:   exePath(pid),
	}, nil
}

// The proc_info call and flavor that read the path of a process's executable.
const (
	procInfoCallPIDInfo    = 2    // PROC_INFO_CALL_PIDINFO
	procPIDPathInfo        = 11   // PROC_PIDPATHINFO
	procPIDPathInfoMaxSize = 4096 // PROC_PIDPATHINFO_MAXSIZE
)

// exePath is the path of the file the process is executing, as the kernel records it.
// "" when the kernel does not give one, as for a process that has exited.
func exePath(pid int32) string {
	buf := make([]byte, procPIDPathInfoMaxSize)
	_, _, errno := unix.Syscall6(unix.SYS_PROC_INFO, procInfoCallPIDInfo, uintptr(pid), procPIDPathInfo, 0,
		uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)))
	if errno != 0 {
		return ""
	}
	if i := bytes.IndexByte(buf, 0); i >= 0 {
		buf = buf[:i]
	}
	return string(buf)
}

func environ(pid int32) ([]string, error) {
	buf, err := unix.SysctlRaw("kern.procargs2", int(pid))
	if err != nil {
		return nil, fmt.Errorf("peerproc: kern.procargs2 for pid %d: %w", pid, err)
	}
	return parseProcArgs2(buf)
}

// parseProcArgs2 reads the environment out of a kern.procargs2 buffer. The layout is argc
// as a 32-bit integer, the executable path, NUL padding, argc NUL-terminated arguments,
// then the NUL-terminated environment, which ends at an empty string. Apple's own strings
// follow that empty one and are not the environment.
func parseProcArgs2(buf []byte) ([]string, error) {
	if len(buf) < 4 {
		return nil, fmt.Errorf("peerproc: kern.procargs2 is %d bytes", len(buf))
	}
	argc := int(binary.LittleEndian.Uint32(buf[:4]))
	rest := buf[4:]
	i := bytes.IndexByte(rest, 0)
	if i < 0 {
		return nil, errors.New("peerproc: kern.procargs2 has no executable path")
	}
	rest = rest[i:]
	for len(rest) > 0 && rest[0] == 0 {
		rest = rest[1:]
	}
	for n := 0; n < argc; n++ {
		i := bytes.IndexByte(rest, 0)
		if i < 0 {
			return nil, errors.New("peerproc: kern.procargs2 ends inside its arguments")
		}
		rest = rest[i+1:]
	}
	var env []string
	for {
		i := bytes.IndexByte(rest, 0)
		if i <= 0 {
			return env, nil
		}
		env = append(env, string(rest[:i]))
		rest = rest[i+1:]
	}
}
