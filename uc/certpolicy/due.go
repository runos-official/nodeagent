package certpolicy

import (
	"crypto/sha256"
	"encoding/binary"
	"time"
)

// jitterLabel separates the jitter hash from any other use of the node id.
// Changing it moves the due point of every node.
const jitterLabel = "runos-agent-cert-renewal-jitter-v1"

// Timing is the renewal timing of one held certificate at one moment.
type Timing struct {
	NotBefore time.Time
	NotAfter  time.Time
	// Life is NotAfter - NotBefore.
	Life time.Duration
	// Jitter is fixed per node id, in the closed range 0 to Life/12.
	Jitter time.Duration
	// DueAt is NotAfter - (Life/3 - Jitter): between Life/4 and Life/3
	// before NotAfter.
	DueAt time.Time
	// Due is now >= DueAt.
	Due bool
	// Expired is now >= NotAfter.
	Expired bool
	// Remaining is NotAfter - now, never negative.
	Remaining time.Duration
}

// Evaluate gives the renewal timing of a certificate with the given
// validity for the node nodeID at the time now.
//
// A certificate with a life of zero or less is malformed: it is due at
// once, with no jitter. An empty node id is valid and gives one fixed
// jitter. The rule has no certificate version input, so a v1 and a v2
// certificate with equal times get the same result.
func Evaluate(notBefore, notAfter time.Time, nodeID string, now time.Time) Timing {
	t := Timing{
		NotBefore: notBefore,
		NotAfter:  notAfter,
		Life:      notAfter.Sub(notBefore),
		Expired:   !now.Before(notAfter),
	}
	if !t.Expired {
		t.Remaining = notAfter.Sub(now)
	}
	if t.Life <= 0 {
		t.DueAt = notAfter
		t.Due = true
		return t
	}
	t.Jitter = jitter(t.Life, nodeID)
	t.DueAt = notAfter.Add(-(t.Life/3 - t.Jitter))
	t.Due = !now.Before(t.DueAt)
	return t
}

// jitter spreads the due points of a fleet. It takes the first 8 bytes of
// SHA-256 over the label, a zero byte and the node id, modulo Life/12 plus
// one nanosecond. SHA-256 is a spread function here, not a secret.
func jitter(life time.Duration, nodeID string) time.Duration {
	h := sha256.New()
	h.Write([]byte(jitterLabel))
	h.Write([]byte{0})
	h.Write([]byte(nodeID))
	sum := h.Sum(nil)
	n := binary.BigEndian.Uint64(sum[:8])
	return time.Duration(n % uint64(life/12+1))
}
