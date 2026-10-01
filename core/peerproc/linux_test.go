//go:build linux

package peerproc

import (
	"encoding/binary"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The fixtures print addresses the way a little-endian kernel does.
func skipUnlessLittleEndian(t *testing.T) {
	t.Helper()
	if binary.NativeEndian.Uint16([]byte{1, 0}) != 1 {
		t.Skip("fixtures are in little-endian /proc format")
	}
}

const procNetTCP = `  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode
   0: 0100007F:B98C 0100007F:1F90 01 00000000:00000000 00:00000000 00000000  1000        0 123456 1 0000000000000000 20 4 30 10 -1
   1: 00000000:1F90 00000000:0000 0A 00000000:00000000 00:00000000 00000000  1000        0 654321 1 0000000000000000 100 0 0 10 0
   2: garbage line
`

const procNetTCP6 = `  sl  local_address                         remote_address                        st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode
   0: 00000000000000000000000001000000:1F91 00000000000000000000000001000000:B98D 01 00000000:00000000 00:00000000 00000000  1000        0 777 1 0000000000000000 20 4 30 10 -1
   1: 0000000000000000FFFF00000100007F:B98E 0000000000000000FFFF00000100007F:1F92 01 00000000:00000000 00:00000000 00000000  1000        0 888 1 0000000000000000 20 4 30 10 -1
`

func TestParseProcNetTCP_IPv4(t *testing.T) {
	skipUnlessLittleEndian(t)
	es, err := parseProcNetTCP(strings.NewReader(procNetTCP))
	if err != nil {
		t.Fatal(err)
	}
	want := []tcpEntry{
		{local: netip.MustParseAddrPort("127.0.0.1:47500"), remote: netip.MustParseAddrPort("127.0.0.1:8080"), inode: 123456},
		{local: netip.MustParseAddrPort("0.0.0.0:8080"), remote: netip.MustParseAddrPort("0.0.0.0:0"), listen: true, inode: 654321},
	}
	if len(es) != len(want) {
		t.Fatalf("%d entries, want %d (the garbage line skipped): %+v", len(es), len(want), es)
	}
	for i := range want {
		if es[i] != want[i] {
			t.Errorf("entry %d = %+v, want %+v", i, es[i], want[i])
		}
	}
}

// tcp6 holds both real IPv6 sockets and IPv4-mapped ones; the mapped one must read
// exactly as the same connection would from /proc/net/tcp.
func TestParseProcNetTCP_IPv6AndMapped(t *testing.T) {
	skipUnlessLittleEndian(t)
	es, err := parseProcNetTCP(strings.NewReader(procNetTCP6))
	if err != nil {
		t.Fatal(err)
	}
	want := []tcpEntry{
		{local: netip.MustParseAddrPort("[::1]:8081"), remote: netip.MustParseAddrPort("[::1]:47501"), inode: 777},
		{local: netip.MustParseAddrPort("127.0.0.1:47502"), remote: netip.MustParseAddrPort("127.0.0.1:8082"), inode: 888},
	}
	if len(es) != len(want) {
		t.Fatalf("%d entries, want %d: %+v", len(es), len(want), es)
	}
	for i := range want {
		if es[i] != want[i] {
			t.Errorf("entry %d = %+v, want %+v", i, es[i], want[i])
		}
	}
}

// The command name is parenthesised and may itself hold spaces and parentheses.
func TestParseStat_CountsFieldsFromTheLastParen(t *testing.T) {
	fields := make([]string, 50)
	for i := range fields {
		fields[i] = "0"
	}
	fields[0], fields[1], fields[19] = "S", "17", "99887"
	line := "4242 (my (odd) proc) " + strings.Join(fields, " ") + "\n"

	ppid, start, err := parseStat([]byte(line))
	if err != nil {
		t.Fatal(err)
	}
	if ppid != 17 || start != 99887 {
		t.Errorf("ppid %d start %d, want 17 and 99887", ppid, start)
	}
}

func TestParseStat_RejectsAShortLine(t *testing.T) {
	if _, _, err := parseStat([]byte("1 (x) S 0")); err == nil {
		t.Error("parseStat accepted a line with too few fields")
	}
}

func TestReadBootTime(t *testing.T) {
	path := filepath.Join(t.TempDir(), "stat")
	if err := os.WriteFile(path, []byte("cpu  1 2 3\nintr 5\nbtime 1700000000\nprocesses 9\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	bt, err := readBootTime(path)
	if err != nil || !bt.Equal(time.Unix(1700000000, 0)) {
		t.Errorf("readBootTime = %v, %v; want 1700000000", bt, err)
	}
}

// A wildcard bind answers for every address on its port. Linux-only: on macOS a test
// binary listening on every interface can raise the firewall prompt.
func TestListenerOwner_MatchesAWildcardBind(t *testing.T) {
	r := newResolver(t)
	ln := listen(t, "tcp4", "0.0.0.0:0")
	port := uint16(ln.Addr().(*net.TCPAddr).Port)

	p, err := r.ListenerOwner(netip.AddrPortFrom(netip.MustParseAddr("127.0.0.1"), port))
	if err != nil || p.PID != int32(os.Getpid()) {
		t.Errorf("ListenerOwner(127.0.0.1:%d) over a 0.0.0.0 bind = %+v, %v", port, p, err)
	}
}
