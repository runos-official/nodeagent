package commons

import (
	"math/rand/v2"
	"time"
)

// Reconnect backoff for the L2SEC stream and the unary retry loops that run
// after a disconnect (FPL315 phase 8). Full jitter: each wait is random in
// [0, cap]. The cap starts at 1 s and doubles up to 30 s. A connection that
// stays up 60 s resets the cap. So a nodeward or nodehub restart spreads the
// reconnects of all agents over time instead of one second.
const (
	ReconnectBaseCap    = 1 * time.Second
	ReconnectMaxCap     = 30 * time.Second
	ReconnectStableTime = 60 * time.Second
)

// Backoff is capped exponential backoff with full jitter. It is not safe for
// concurrent use; each retry loop owns one.
type Backoff struct {
	base, max, stable time.Duration
	attempt           int
	rand              func(n int64) int64 // returns [0, n); swapped in tests
}

// NewReconnectBackoff returns a Backoff with the reconnect bounds above.
func NewReconnectBackoff() *Backoff {
	return &Backoff{
		base:   ReconnectBaseCap,
		max:    ReconnectMaxCap,
		stable: ReconnectStableTime,
		rand:   rand.Int64N,
	}
}

// Next returns the next wait and doubles the cap for the wait after it.
func (b *Backoff) Next() time.Duration {
	limit := b.max
	if b.attempt < 32 { // a larger shift would overflow
		if c := b.base << b.attempt; c > 0 && c < b.max {
			limit = c
		}
	}
	b.attempt++
	return time.Duration(b.rand(int64(limit) + 1))
}

// Reset puts the cap back to its first value.
func (b *Backoff) Reset() {
	b.attempt = 0
}

// AfterConnection returns the wait before the next reconnect. upFor is how
// long the last connection stayed up (0 when it never came up). A connection
// that stayed up for the stable time resets the cap first.
func (b *Backoff) AfterConnection(upFor time.Duration) time.Duration {
	if upFor >= b.stable {
		b.Reset()
	}
	return b.Next()
}
