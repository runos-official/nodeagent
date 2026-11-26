package certrenew

import (
	"time"

	"github.com/runos-official/nodeagent/roslog"
)

// Log messages of the loop. Each line holds no PEM data and no proof.
const (
	// AttemptMessage is the message of the line of one attempt.
	AttemptMessage = "Certificate renewal attempt"
	// ExpiredMessage is the one ERROR line when the held certificate
	// expires.
	ExpiredMessage = "mTLS certificate expired; the agent does not request a renewal"
	// LateMessage is the one extra WARN line when less than a sixth of the
	// certificate life is left.
	LateMessage = "mTLS certificate renewal is late: less than a sixth of the certificate life is left"
	// PanicMessage is the ERROR line of a panic in the loop.
	PanicMessage = "Certificate renewal loop stopped on a panic; it runs again in 1 minute"
	// StateWriteMessage is the WARN line of a failed state file write.
	StateWriteMessage = "Could not write the certificate renewal state file; the renewal loop continues"
	// ReadErrorMessage is the WARN line of a held certificate that does
	// not read.
	ReadErrorMessage = "Could not read the held mTLS certificate; the renewal loop tries again in 1 minute"
)

// ReasonLockBusy is the reason of an attempt that found another renewal
// in progress (the renewal lock wait ended). It is not a failure.
const ReasonLockBusy = "RENEWAL_IN_PROGRESS"

// maxBackoffStep is the last backoff step number. The policy backoff
// doubles from 1 minute and stops at the 1-hour ceiling after 6 doublings,
// so one reason gives at most 7 WARN lines.
const maxBackoffStep = 6

// attemptLine is the content of one attempt line.
type attemptLine struct {
	Attempt     int
	Result      string
	Outcome     string
	Reason      string
	TimeLeft    time.Duration
	NextAttempt time.Time
	HeldSerial  string
	// NewSerial is the live serial after a success.
	NewSerial string
	// Unclassified adds the gRPC code and the status text.
	Unclassified bool
	GRPCCode     string
	Status       string
	// Failure marks an attempt that the WARN rule counts; Step is its
	// backoff step number.
	Failure bool
	Step    int
}

// attemptLog keeps the WARN rule state: the (reason, step) pair of the
// last WARN line.
type attemptLog struct {
	warned     bool
	warnReason string
	warnStep   int
}

// write logs one attempt line. A success is INFO. A failure is WARN when
// its (reason, backoff step) pair differs from the pair of the last WARN
// line, else INFO.
func (a *attemptLog) write(l attemptLine) {
	args := []any{
		"attempt", l.Attempt,
		"result", l.Result,
		"outcome", l.Outcome,
		"reason", l.Reason,
		"time_left", l.TimeLeft.Round(time.Second).String(),
		"next_attempt", stamp(l.NextAttempt),
		"held_serial", l.HeldSerial,
	}
	if l.NewSerial != "" {
		args = append(args, "new_serial", l.NewSerial)
	}
	if l.Unclassified {
		args = append(args, "grpc_code", l.GRPCCode, "status", l.Status)
	}
	step := min(max(l.Step, 0), maxBackoffStep)
	if l.Failure && (!a.warned || a.warnReason != l.Reason || a.warnStep != step) {
		a.warned, a.warnReason, a.warnStep = true, l.Reason, step
		roslog.W(AttemptMessage, nil, args...)
		return
	}
	roslog.I(AttemptMessage, args...)
}

// reset forgets the last WARN pair (a new held certificate).
func (a *attemptLog) reset() { *a = attemptLog{} }
