package commons

import (
	"testing"
	"time"
)

// FPL315 phase 8: a nodeward or nodehub restart must not make every agent
// reconnect in the same second. Each wait is random in [0, cap], the cap
// doubles from 1 s to 30 s, and a connection that stayed up 60 s resets it.

func TestReconnectBackoffCapsDoubleToThirtySeconds(t *testing.T) {
	b := NewReconnectBackoff()
	b.rand = func(n int64) int64 { return n - 1 } // always the top of the range
	want := []time.Duration{1, 2, 4, 8, 16, 30, 30, 30}
	for i, w := range want {
		if got := b.Next(); got != w*time.Second {
			t.Fatalf("wait %d = %s, want %s", i+1, got, w*time.Second)
		}
	}
}

func TestReconnectBackoffIsFullJitter(t *testing.T) {
	b := NewReconnectBackoff()
	b.rand = func(int64) int64 { return 0 }
	for i := 0; i < 10; i++ {
		if got := b.Next(); got != 0 {
			t.Fatalf("wait %d = %s, want 0 at the bottom of the range", i+1, got)
		}
	}

	// With the real source, every wait stays in [0, cap] and the waits spread.
	b = NewReconnectBackoff()
	first := map[time.Duration]bool{}
	for i := 0; i < 200; i++ {
		b.Reset()
		w := b.Next()
		if w < 0 || w > time.Second {
			t.Fatalf("first wait %s is outside [0, 1s]", w)
		}
		first[w] = true
	}
	if len(first) < 50 {
		t.Fatalf("200 first waits gave only %d distinct values; agents would reconnect together", len(first))
	}
}

func TestReconnectBackoffResetsAfterAStableConnection(t *testing.T) {
	b := NewReconnectBackoff()
	b.rand = func(n int64) int64 { return n - 1 }
	for i := 0; i < 6; i++ {
		b.Next()
	}
	if got := b.AfterConnection(59 * time.Second); got != 30*time.Second {
		t.Fatalf("after a 59 s connection: wait %s, want the 30 s cap kept", got)
	}
	if got := b.AfterConnection(60 * time.Second); got != time.Second {
		t.Fatalf("after a 60 s connection: wait %s, want the 1 s first cap", got)
	}
	if got := b.AfterConnection(0); got != 2*time.Second {
		t.Fatalf("a failed dial after the reset: wait %s, want the 2 s cap", got)
	}
}

func TestNewReconnectBackoffWithUsesTheGivenSource(t *testing.T) {
	b := NewReconnectBackoffWith(func(n int64) int64 { return n - 1 })
	want := []time.Duration{1, 2, 4, 8, 16, 30, 30}
	for i, w := range want {
		if got := b.Next(); got != w*time.Second {
			t.Fatalf("wait %d = %s, want %s", i+1, got, w*time.Second)
		}
	}
}

// One node's own retry loop (the install command fetch) needs a floor: equal
// jitter waits random in [cap/2, cap], so the total wait before giving up
// cannot collapse toward 0.
func TestRetryBackoffIsEqualJitter(t *testing.T) {
	low := NewRetryBackoffWith(func(int64) int64 { return 0 })
	high := NewRetryBackoffWith(func(n int64) int64 { return n - 1 })
	caps := []time.Duration{1, 2, 4, 8, 16, 30, 30}
	for i, c := range caps {
		c *= time.Second
		if got := low.Next(); got != c/2 {
			t.Fatalf("lowest wait %d = %s, want %s", i+1, got, c/2)
		}
		if got := high.Next(); got != c {
			t.Fatalf("highest wait %d = %s, want %s", i+1, got, c)
		}
	}

	b := NewRetryBackoff()
	for i := 0; i < 200; i++ {
		b.Reset()
		if w := b.Next(); w < 500*time.Millisecond || w > time.Second {
			t.Fatalf("first wait %s is outside [0.5s, 1s]", w)
		}
	}
}
