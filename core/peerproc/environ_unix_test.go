//go:build darwin || linux

package peerproc

import (
	"errors"
	"os"
	"syscall"
	"testing"
)

// Another user's process cannot be read. Pid 1 is root's on a host, but a container's init
// can be the caller's own, so the test skips when it is.
func TestEnviron_RefusesAnotherUsersProcess(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can read every process")
	}
	if fi, err := os.Stat("/proc/1"); err == nil {
		if st, ok := fi.Sys().(*syscall.Stat_t); ok && int(st.Uid) == os.Geteuid() {
			t.Skip("pid 1 is this user's own (a container's init)")
		}
	}
	_, err := Environ(1)
	if err == nil {
		t.Fatal("Environ(1) succeeded for a non-root caller")
	}
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("Environ(1) = %v, want ErrNotFound", err)
	}
}
