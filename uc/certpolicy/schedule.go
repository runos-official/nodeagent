package certpolicy

import "time"

// Schedule numbers.
const (
	backoffStart   = time.Minute
	backoffCeiling = time.Hour
	waitSpreadMax  = 5 * time.Minute
	idleWait       = 6 * time.Hour
	// maxShift caps the backoff exponent: 1 minute << 6 is over 1 hour.
	maxShift = 6
)

// Plan is the time of the next renewal attempt.
type Plan struct {
	At time.Time
	// OnFileChange asks the caller to try at once when the certificate
	// files change. The caller watches the files; this package does not.
	OnFileChange bool
}

// Next gives the time of the next attempt after outcome o.
// consecutiveFailures counts the failed attempts in a row with the held
// certificate; a value below 1 counts as 1. rnd returns a number from 0 up
// to 1; nil counts as 0.
//
//   - Transient: step = min(1 minute x 2^(failures-1), 1 hour,
//     remaining/10); the delay is in [step/2, step], so the attempt is
//     always before NotAfter.
//   - WaitUntil: RetryAt (or now when RetryAt is past) plus a spread of up
//     to min(5 minutes, Life/12). A spread that would reach NotAfter is
//     dropped.
//   - Every kind that does not retry, and every kind once the certificate
//     expired: now + 6 hours, and at once on a certificate file change.
func Next(o Outcome, t Timing, consecutiveFailures int, now time.Time, rnd func() float64) Plan {
	if t.Expired || !now.Before(t.NotAfter) || !o.Kind.Retries() {
		return Plan{At: now.Add(idleWait), OnFileChange: true}
	}
	r := unit(rnd)
	if o.Kind == WaitUntil {
		base := o.RetryAt
		if base.Before(now) {
			base = now
		}
		spread := waitSpreadMax
		if t.Life > 0 && t.Life/12 < spread {
			spread = t.Life / 12
		}
		at := base.Add(time.Duration(float64(spread) * r))
		if !at.Before(t.NotAfter) {
			at = base
		}
		return Plan{At: at}
	}

	remaining := t.NotAfter.Sub(now)
	ceiling := backoffCeiling
	if remaining/10 < ceiling {
		ceiling = remaining / 10
	}
	shift := consecutiveFailures - 1
	if shift < 0 {
		shift = 0
	}
	if shift > maxShift {
		shift = maxShift
	}
	step := backoffStart << shift
	if step > ceiling {
		step = ceiling
	}
	half := step / 2
	delay := half + time.Duration(float64(step-half)*r)
	if delay > step {
		delay = step
	}
	return Plan{At: now.Add(delay)}
}

// unit returns rnd() clamped to [0, 1).
func unit(rnd func() float64) float64 {
	if rnd == nil {
		return 0
	}
	v := rnd()
	if !(v >= 0) { // also NaN
		return 0
	}
	if v >= 1 {
		return 0.999999999
	}
	return v
}
