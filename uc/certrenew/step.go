package certrenew

import (
	"time"

	"github.com/runos-official/nodeagent/uc/certpolicy"
)

// Timing numbers of the loop.
const (
	// settle is the wait after the stream connects, before a mint, so that
	// Nodeward holds the live route of the node.
	settle = 10 * time.Second
	// noStreamLimit: an attempt that is ready runs after this wait even
	// with no connected stream.
	noStreamLimit = 2 * time.Minute
	// serialCheck is the period of the held serial check during a sleep.
	serialCheck = 30 * time.Second
	// lockBusyRetry is the wait after "renewal already in progress".
	lockBusyRetry = time.Minute
	// readErrorRetry is the wait after the held certificate did not read.
	readErrorRetry = time.Minute
	// expiredWake is the wait of an expired certificate.
	expiredWake = 6 * time.Hour
	// attemptLimit is the time limit of one attempt.
	attemptLimit = 150 * time.Second
	// panicRestart is the wait before the loop runs again after a panic.
	panicRestart = time.Minute
	// ShutdownWait is the time that the agent waits for an attempt in
	// progress at shutdown.
	ShutdownWait = 20 * time.Second
)

// action is what the loop does next.
type action int

const (
	// actSleep: not due, or a retry time not reached. Sleep until At.
	actSleep action = iota + 1
	// actExpired: the held certificate expired. Send no request; wake at
	// At or on a serial change.
	actExpired
	// actWaitStream: the attempt is ready and waits for a connected stream
	// and the settle time. Wake at At or on a connection change.
	actWaitStream
	// actAttempt: attempt now.
	actAttempt
)

// input is what decide reads.
type input struct {
	Now    time.Time
	Timing certpolicy.Timing
	// RetryAt is the planned time after a failed attempt; zero means the
	// due point decides.
	RetryAt time.Time
	// ReadyAt is when the attempt became ready; zero means not yet.
	ReadyAt     time.Time
	Connected   bool
	ConnectedAt time.Time
}

// decision is the next action of the loop.
type decision struct {
	Action action
	// At is the wake time of a sleep.
	At time.Time
	// ReadyAt is the ready time to keep (actWaitStream only).
	ReadyAt time.Time
	// NextAttemptAt is the planned attempt time, for the state file.
	NextAttemptAt time.Time
}

// decide is the pure decision of one loop step. No sleep passes NotAfter.
func decide(in input) decision {
	t := in.Timing
	if t.Expired || !in.Now.Before(t.NotAfter) {
		at := in.Now.Add(expiredWake)
		return decision{Action: actExpired, At: at, NextAttemptAt: at}
	}
	target := t.DueAt
	if !in.RetryAt.IsZero() {
		target = in.RetryAt
	}
	if in.Now.Before(target) {
		return decision{Action: actSleep, At: earliest(target, t.NotAfter), NextAttemptAt: target}
	}

	ready := in.ReadyAt
	if ready.IsZero() {
		ready = in.Now
	}
	settled := in.ConnectedAt.Add(settle)
	limit := ready.Add(noStreamLimit)
	if (in.Connected && !in.Now.Before(settled)) || !in.Now.Before(limit) {
		return decision{Action: actAttempt, NextAttemptAt: target}
	}
	at := limit
	if in.Connected {
		at = earliest(at, settled)
	}
	return decision{Action: actWaitStream, At: earliest(at, t.NotAfter), ReadyAt: ready, NextAttemptAt: target}
}

func earliest(a, b time.Time) time.Time {
	if b.Before(a) {
		return b
	}
	return a
}
