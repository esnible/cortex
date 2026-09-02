package forwardproxy

import (
	"strings"
	"sync"
	"testing"

	"github.com/rossoctl/cortex/authbridge/authlib/tlsbridge"
)

// TestNoteTunnel_WarnsOnlyWhenNothingIsEverDecrypted covers the failure that
// looks like a plugin bug: the bridge is on, the client does not trust its CA,
// so every HTTPS request opens an opaque tunnel and every body-reading plugin
// correctly does nothing. Nothing errors — the only symptom is silence.
func TestNoteTunnel_WarnsOnlyWhenNothingIsEverDecrypted(t *testing.T) {
	tests := []struct {
		name      string
		bridge    *tlsbridge.Engine
		tunnels   int
		bridged   uint64
		wantWarns int
	}{
		{
			name:      "bridge disabled: never warn, tunnels are the expected behaviour",
			bridge:    nil,
			tunnels:   50,
			wantWarns: 0,
		},
		{
			name:      "below threshold: a few tunnels are normal (passthrough hosts, startup races)",
			bridge:    &tlsbridge.Engine{},
			tunnels:   tunnelWarnThreshold - 1,
			wantWarns: 0,
		},
		{
			name:      "tunnels but something was decrypted: bridge is working",
			bridge:    &tlsbridge.Engine{},
			tunnels:   50,
			bridged:   1,
			wantWarns: 0,
		},
		{
			name:      "many tunnels, nothing decrypted: warn",
			bridge:    &tlsbridge.Engine{},
			tunnels:   tunnelWarnThreshold,
			wantWarns: 1,
		},
		{
			name:      "and only once, however much traffic follows",
			bridge:    &tlsbridge.Engine{},
			tunnels:   200,
			wantWarns: 1,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := &Server{TLSBridge: tc.bridge}
			s.bridgedRequests.Store(tc.bridged)
			var warns int
			// bridgeWarnOnce is the mechanism under test; count how many times
			// the guarded block would run by observing the sync.Once directly.
			for i := 0; i < tc.tunnels; i++ {
				before := s.warnFired()
				s.noteTunnel()
				if !before && s.warnFired() {
					warns++
				}
			}
			if warns != tc.wantWarns {
				t.Errorf("warned %d times, want %d", warns, tc.wantWarns)
			}
		})
	}
}

// TestCaFileHint_NamesTheAbsolutePath: a relative path in the fix hint is only
// correct for someone standing in the directory --demo was launched from, which
// is precisely how the trust anchor gets mismatched in the first place.
func TestCaFileHint_NamesTheAbsolutePath(t *testing.T) {
	s := &Server{TLSBridge: &tlsbridge.Engine{CAFile: "/abs/cortex-ca/ca.crt"}}
	if got := s.caFileHint(); got != "/abs/cortex-ca/ca.crt" {
		t.Errorf("caFileHint() = %q", got)
	}
	// Degrade to a placeholder rather than an empty string, so the log line
	// still reads as an instruction.
	bare := &Server{TLSBridge: &tlsbridge.Engine{}}
	if got := bare.caFileHint(); !strings.Contains(got, "ca.crt") {
		t.Errorf("caFileHint() = %q, want something naming ca.crt", got)
	}
}

// TestNoteTunnel_ConcurrentIsRaceFree: tunnels open on many goroutines.
func TestNoteTunnel_ConcurrentIsRaceFree(t *testing.T) {
	s := &Server{TLSBridge: &tlsbridge.Engine{}}
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 64; j++ {
				s.noteTunnel()
			}
		}()
	}
	wg.Wait()
	if got := s.tunnelsOpened.Load(); got != 16*64 {
		t.Errorf("tunnelsOpened = %d, want %d", got, 16*64)
	}
}
