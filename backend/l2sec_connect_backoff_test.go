package backend

import (
	"testing"
	"time"
)

// FPL315 phase 8: grpc-go's own reconnect inside one dial must also be full
// jitter. For retry n >= 1 grpc-go waits min(base*mult^n, max) *
// (1 + jitter*(2r-1)), so with jitter 1 each wait is random in
// [0, 2*min(base*mult^n, max)]. With base 0.5 s and max 15 s the cap is 30 s.
// Retry 0 waits the base delay, 0.5 s, without jitter.
func TestL2SecConnectBackoffIsFullJitter(t *testing.T) {
	b := l2secConnectBackoff
	if b.Jitter != 1 {
		t.Fatalf("jitter = %v, want 1 (full jitter)", b.Jitter)
	}
	if b.Multiplier != 2 {
		t.Errorf("multiplier = %v, want 2", b.Multiplier)
	}
	if b.BaseDelay != 500*time.Millisecond {
		t.Errorf("base delay = %s, want 0.5s", b.BaseDelay)
	}
	if top := 2 * b.MaxDelay; top != 30*time.Second {
		t.Errorf("capped wait range is [0, %s], want [0, 30s]", top)
	}
}
