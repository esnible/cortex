//go:build linux

package peerproc

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"io"
	"net/netip"
	"os"
	"strconv"
	"strings"
	"time"
)

// clockTicks is USER_HZ, the unit of /proc/<pid>/stat's starttime. The kernel reports
// 100 on every architecture, whatever its internal HZ.
const clockTicks = 100

type linux struct {
	bootTime time.Time
}

func newPlatform() (Resolver, error) {
	bt, err := readBootTime("/proc/stat")
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrUnsupported, err)
	}
	return &linux{bootTime: bt}, nil
}

// tcpEntry is one line of /proc/net/tcp or /proc/net/tcp6.
type tcpEntry struct {
	local, remote netip.AddrPort
	listen        bool
	inode         uint64
}

// parseProcNetTCP reads /proc/net/tcp or /proc/net/tcp6, skipping the header and any
// line it cannot read. Addresses are unmapped, so an IPv4 connection reads the same
// from either file.
func parseProcNetTCP(r io.Reader) ([]tcpEntry, error) {
	var out []tcpEntry
	sc := bufio.NewScanner(r)
	for header := true; sc.Scan(); header = false {
		if header {
			continue
		}
		f := strings.Fields(sc.Text())
		if len(f) < 10 {
			continue
		}
		local, err1 := parseHexAddrPort(f[1])
		remote, err2 := parseHexAddrPort(f[2])
		inode, err3 := strconv.ParseUint(f[9], 10, 64)
		if err1 != nil || err2 != nil || err3 != nil {
			continue
		}
		out = append(out, tcpEntry{local: local, remote: remote, listen: f[3] == "0A", inode: inode})
	}
	return out, sc.Err()
}

// parseHexAddrPort reads "0100007F:1F90", or its 32-digit IPv6 form. The kernel prints
// each 32-bit word of the address as the hex of that word loaded in host byte order,
// and the port as plain hex.
func parseHexAddrPort(s string) (netip.AddrPort, error) {
	a, p, ok := strings.Cut(s, ":")
	if !ok {
		return netip.AddrPort{}, fmt.Errorf("no port in %q", s)
	}
	port, err := strconv.ParseUint(p, 16, 16)
	if err != nil {
		return netip.AddrPort{}, err
	}
	raw, err := hex.DecodeString(a)
	if err != nil {
		return netip.AddrPort{}, err
	}
	// Each word as printed is a value; storing that value in host order recovers the
	// address bytes as they sat in kernel memory, which is network order.
	for i := 0; i+4 <= len(raw); i += 4 {
		binary.NativeEndian.PutUint32(raw[i:], binary.BigEndian.Uint32(raw[i:]))
	}
	var addr netip.Addr
	switch len(raw) {
	case 4:
		addr = netip.AddrFrom4([4]byte(raw))
	case 16:
		addr = netip.AddrFrom16([16]byte(raw)).Unmap()
	default:
		return netip.AddrPort{}, fmt.Errorf("address %q is %d bytes", a, len(raw))
	}
	return netip.AddrPortFrom(addr, uint16(port)), nil
}

// entries reads both TCP tables. A host without IPv6 has no tcp6, which is not an error.
func (l *linux) entries() ([]tcpEntry, error) {
	var all []tcpEntry
	for _, path := range []string{"/proc/net/tcp", "/proc/net/tcp6"} {
		f, err := os.Open(path)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return nil, err
		}
		es, err := parseProcNetTCP(f)
		_ = f.Close()
		if err != nil {
			return nil, err
		}
		all = append(all, es...)
	}
	return all, nil
}

func (l *linux) ConnOwner(client, server netip.AddrPort, hints ...int32) (Proc, error) {
	client, server = unmapAddrPort(client), unmapAddrPort(server)
	es, err := l.entries()
	if err != nil {
		return Proc{}, err
	}
	for _, e := range es {
		if !e.listen && e.inode != 0 && e.local == client && e.remote == server {
			return l.inodeOwner(e.inode, hints)
		}
	}
	return Proc{}, ErrNotFound
}

func (l *linux) ListenerOwner(addr netip.AddrPort) (Proc, error) {
	addr = unmapAddrPort(addr)
	es, err := l.entries()
	if err != nil {
		return Proc{}, err
	}
	for _, e := range es {
		if e.listen && e.inode != 0 && e.local.Port() == addr.Port() &&
			(e.local.Addr() == addr.Addr() || e.local.Addr().IsUnspecified()) {
			return l.inodeOwner(e.inode, nil)
		}
	}
	return Proc{}, ErrNotFound
}

func (l *linux) Ancestry(pid int32, max int) ([]Proc, error) { return ancestry(l.procInfo, pid, max) }

// inodeOwner finds the process holding socket inode: the hints first, then every
// process. A socket shared across a fork is held by both processes, and the first one
// found wins. The lookup must run while the connection is open: a process that has
// exited is a zombie whose fd directory cannot be read.
func (l *linux) inodeOwner(inode uint64, hints []int32) (Proc, error) {
	target := "socket:[" + strconv.FormatUint(inode, 10) + "]"
	tried := make(map[int32]bool, len(hints))
	for _, pid := range hints {
		if pid <= 0 || tried[pid] {
			continue
		}
		tried[pid] = true
		if holds(pid, target) {
			return l.procInfo(pid)
		}
	}
	dirs, err := os.ReadDir("/proc")
	if err != nil {
		return Proc{}, err
	}
	for _, d := range dirs {
		n, err := strconv.ParseInt(d.Name(), 10, 32)
		if err != nil || tried[int32(n)] {
			continue
		}
		if holds(int32(n), target) {
			return l.procInfo(int32(n))
		}
	}
	return Proc{}, ErrNotFound
}

// holds reports whether pid has an open fd on target. A process the caller may not
// inspect holds nothing.
func holds(pid int32, target string) bool {
	dir := "/proc/" + strconv.Itoa(int(pid)) + "/fd"
	fds, err := os.ReadDir(dir)
	if err != nil {
		return false
	}
	for _, fd := range fds {
		if link, err := os.Readlink(dir + "/" + fd.Name()); err == nil && link == target {
			return true
		}
	}
	return false
}

func (l *linux) procInfo(pid int32) (Proc, error) {
	base := "/proc/" + strconv.Itoa(int(pid))
	b, err := os.ReadFile(base + "/stat")
	if err != nil {
		return Proc{}, ErrNotFound
	}
	ppid, ticks, err := parseStat(b)
	if err != nil {
		return Proc{}, fmt.Errorf("pid %d: %w", pid, err)
	}
	exe, _ := os.Readlink(base + "/exe") // "" when the caller may not read it
	return Proc{
		PID:   pid,
		PPID:  ppid,
		Start: l.bootTime.Add(time.Duration(ticks) * time.Second / clockTicks),
		Exe:   exe,
	}, nil
}

// parseStat reads the parent pid and the start time, in clock ticks after boot, from a
// /proc/<pid>/stat line. The command name is parenthesised and may itself contain spaces
// and parentheses, so fields are counted from the last ')': after it comes field 3.
func parseStat(b []byte) (ppid int32, startTicks uint64, err error) {
	i := bytes.LastIndexByte(b, ')')
	if i < 0 {
		return 0, 0, fmt.Errorf("stat: no ')'")
	}
	f := strings.Fields(string(b[i+1:]))
	if len(f) < 20 {
		return 0, 0, fmt.Errorf("stat: %d fields after the command name", len(f))
	}
	p, err := strconv.ParseInt(f[1], 10, 32) // field 4, ppid
	if err != nil {
		return 0, 0, err
	}
	st, err := strconv.ParseUint(f[19], 10, 64) // field 22, starttime
	if err != nil {
		return 0, 0, err
	}
	return int32(p), st, nil
}

// readBootTime reads btime, the boot time in Unix seconds, from /proc/stat.
func readBootTime(path string) (time.Time, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return time.Time{}, err
	}
	for _, line := range strings.Split(string(b), "\n") {
		if v, ok := strings.CutPrefix(line, "btime "); ok {
			n, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64)
			if err != nil {
				return time.Time{}, err
			}
			return time.Unix(n, 0), nil
		}
	}
	return time.Time{}, fmt.Errorf("%s: no btime line", path)
}
