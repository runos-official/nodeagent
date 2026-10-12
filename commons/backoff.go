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

// Backoff is capped exponential backoff with jitter. It is not safe for
// concurrent use; each retry loop owns one.
type Backoff struct {
	base, max, stable time.Duration
	attempt           int
	equal             bool                // equal jitter: wait in [cap/2, cap]
	rand              func(n int64) int64 // returns [0, n)
}

// NewReconnectBackoff returns a full jitter Backoff with the reconnect bounds
// above: each wait is random in [0, cap].
func NewReconnectBackoff() *Backoff {
	return NewReconnectBackoffWith(rand.Int64N)
}

// NewReconnectBackoffWith is NewReconnectBackoff with the given random source.
// rand(n) must return a value in [0, n). Tests pin it.
func NewReconnectBackoffWith(rand func(n int64) int64) *Backoff {
	return &Backoff{
		base:   ReconnectBaseCap,
		max:    ReconnectMaxCap,
		stable: ReconnectStableTime,
		rand:   rand,
	}
}

// NewRetryBackoff returns a Backoff with the same caps but equal jitter: each
// wait is random in [cap/2, cap]. Use it for one node's own retry loop that
// gives up after a number of attempts, so the total wait has a floor and a
// run of small draws cannot end the loop early.
func NewRetryBackoff() *Backoff {
	return NewRetryBackoffWith(rand.Int64N)
}

// NewRetryBackoffWith is NewRetryBackoff with the given random source.
func NewRetryBackoffWith(rand func(n int64) int64) *Backoff {
	b := NewReconnectBackoffWith(rand)
	b.equal = true
	return b
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
	if b.equal {
		half := limit / 2
		return half + time.Duration(b.rand(int64(limit-half)+1))
	}
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
