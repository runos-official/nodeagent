package certrenew

import (
	"context"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"math/rand/v2"
	"os"
	"runtime/debug"
	"sync"
	"sync/atomic"
	"time"

	"github.com/runos-official/nodeagent/config"
	"github.com/runos-official/nodeagent/roslog"
	"github.com/runos-official/nodeagent/uc/certificate"
	"github.com/runos-official/nodeagent/uc/certpolicy"
	"github.com/runos-official/nodeagent/uc/certstore"
)

// Leaf is what the loop reads from the held certificate.
type Leaf struct {
	NotBefore time.Time
	NotAfter  time.Time
	// Serial is lower-case hexadecimal text, as the certificate store
	// writes it.
	Serial string
}

// Request is the input of one renewal.
type Request struct {
	// ExpectedSerial is the serial of the leaf that the loop evaluated.
	ExpectedSerial string
	// LostResponse is the lost-response predicate of this attempt.
	LostResponse func(error) bool
}

// Deps is everything the loop touches outside itself. A zero field gets
// the production default in New.
type Deps struct {
	// Renew is the one renewal function.
	Renew func(ctx context.Context, r Request) (certificate.Result, error)
	// ReadLeaf reads the held certificate.
	ReadLeaf func() (Leaf, error)
	// NodeID gives the jitter of the due point.
	NodeID string
	// Rand returns a number from 0 up to 1 for the policy schedule.
	Rand func() float64
	// StatePath is the state file.
	StatePath string
	// PID is the pid member of the state file.
	PID int
}

// Loop is the renewal loop of one agent process. The methods of a nil
// *Loop do nothing.
type Loop struct {
	d    Deps
	once sync.Once
	// wake carries connection changes to a loop that waits for a stream.
	wake chan struct{}
	// done closes when the loop goroutine ends.
	done chan struct{}

	mu          sync.Mutex
	connected   bool
	connectedAt time.Time

	// running counts loop goroutines (tests).
	running    atomic.Int32
	maxRunning atomic.Int32

	// The fields below belong to the loop goroutine.
	s    cycleState
	logs attemptLog
	last State
}

// cycleState is the schedule state of the held certificate.
type cycleState struct {
	serial   string
	attempts int
	failures int
	// retryAt is the planned attempt after a failure; zero means the due
	// point decides.
	retryAt time.Time
	readyAt time.Time
	// lost: an earlier answer for the held serial may be lost.
	lost          bool
	lastAttemptAt time.Time
	lastResult    string
	outcome       string
	lastKind      certpolicy.Kind
	errCode       string
	errMessage    string
	lateWarned    bool
	expiredLogged bool
	readFailing   bool
}

// New gives a loop with the production default for each zero field of d.
func New(d Deps) *Loop {
	if d.Renew == nil {
		d.Renew = renewWithCertificate
	}
	if d.ReadLeaf == nil {
		d.ReadLeaf = func() (Leaf, error) { return ReadLeafFile(config.GetPublicKeyPath()) }
	}
	if d.NodeID == "" {
		d.NodeID = config.GetNID()
	}
	if d.Rand == nil {
		d.Rand = rand.Float64
	}
	if d.StatePath == "" {
		d.StatePath = DefaultStatePath
	}
	if d.PID == 0 {
		d.PID = os.Getpid()
	}
	return &Loop{d: d, wake: make(chan struct{}, 1), done: make(chan struct{})}
}

// renewWithCertificate is the production Renew.
func renewWithCertificate(ctx context.Context, r Request) (certificate.Result, error) {
	return certificate.Renew(ctx, certificate.Options{
		Event:          "CertificateAutoRenewal",
		Message:        "Certificate automatically renewed",
		ExpectedSerial: r.ExpectedSerial,
		LostResponse:   r.LostResponse,
	})
}

// ReadLeafFile reads the validity and the serial of the certificate at
// path.
func ReadLeafFile(path string) (Leaf, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return Leaf{}, fmt.Errorf("read the held certificate: %w", err)
	}
	block, _ := pem.Decode(b)
	if block == nil {
		return Leaf{}, errors.New("read the held certificate: no PEM block")
	}
	c, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return Leaf{}, fmt.Errorf("parse the held certificate: %w", err)
	}
	return Leaf{NotBefore: c.NotBefore, NotAfter: c.NotAfter, Serial: fmt.Sprintf("%x", c.SerialNumber)}, nil
}

// Start starts the loop on ctx, the context of the process. A second call
// does nothing. The loop ends when ctx ends.
func (l *Loop) Start(ctx context.Context) {
	if l == nil {
		return
	}
	l.once.Do(func() {
		go l.run(ctx)
	})
}

// NotifyConnected tells the loop that the instruction stream is up. It
// does not change the schedule.
func (l *Loop) NotifyConnected() {
	if l == nil {
		return
	}
	l.mu.Lock()
	l.connected = true
	l.connectedAt = time.Now()
	l.mu.Unlock()
	l.poke()
}

// NotifyDisconnected tells the loop that the instruction stream ended. It
// does not change the schedule.
func (l *Loop) NotifyDisconnected() {
	if l == nil {
		return
	}
	l.mu.Lock()
	l.connected = false
	l.mu.Unlock()
	l.poke()
}

// Wait waits until the loop ends after its context ended, at most timeout.
// An attempt in progress at the cancel runs to its end; Wait gives it
// timeout to finish. It reports whether the loop ended.
func (l *Loop) Wait(timeout time.Duration) bool {
	if l == nil {
		return true
	}
	t := time.NewTimer(timeout)
	defer t.Stop()
	select {
	case <-l.done:
		return true
	case <-t.C:
		return false
	}
}

func (l *Loop) poke() {
	select {
	case l.wake <- struct{}{}:
	default:
	}
}

func (l *Loop) connection() (bool, time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.connected, l.connectedAt
}

// run runs the cycle until ctx ends. A panic is logged at ERROR and the
// cycle runs again after 1 minute.
func (l *Loop) run(ctx context.Context) {
	n := l.running.Add(1)
	for {
		m := l.maxRunning.Load()
		if n <= m || l.maxRunning.CompareAndSwap(m, n) {
			break
		}
	}
	defer func() {
		l.running.Add(-1)
		close(l.done)
	}()
	for ctx.Err() == nil {
		if !l.guarded(ctx) {
			return
		}
		if !sleepCtx(ctx, panicRestart) {
			return
		}
	}
}

// guarded runs the cycle and reports whether it ended on a panic.
func (l *Loop) guarded(ctx context.Context) (panicked bool) {
	defer func() {
		if r := recover(); r != nil {
			panicked = true
			roslog.E(PanicMessage, fmt.Errorf("panic: %v", r), "stack", string(debug.Stack()))
		}
	}()
	l.cycle(ctx)
	return false
}

// cycle is the loop body. It returns when ctx ends.
func (l *Loop) cycle(ctx context.Context) {
	for ctx.Err() == nil {
		leaf, err := l.d.ReadLeaf()
		now := time.Now()
		if err != nil {
			l.onReadError(now, err)
			l.sleep(ctx, now.Add(readErrorRetry), "", false)
			continue
		}
		l.s.readFailing = false
		if leaf.Serial != l.s.serial {
			l.newLeaf(leaf)
		}
		timing := certpolicy.Evaluate(leaf.NotBefore, leaf.NotAfter, l.d.NodeID, now)
		connected, connectedAt := l.connection()
		dec := decide(input{
			Now: now, Timing: timing, RetryAt: l.s.retryAt, ReadyAt: l.s.readyAt,
			Connected: connected, ConnectedAt: connectedAt,
		})

		switch dec.Action {
		case actExpired:
			l.onExpired(leaf, timing, dec)
			l.sleep(ctx, dec.At, leaf.Serial, false)
		case actSleep:
			l.s.readyAt = time.Time{}
			l.lateWarning(timing, now)
			l.writeState(leaf, timing, dec.NextAttemptAt)
			at := dec.At
			if lateAt := leaf.NotAfter.Add(-timing.Life / 6); !l.s.lateWarned && lateAt.After(now) {
				at = earliest(at, lateAt)
			}
			l.sleep(ctx, at, leaf.Serial, false)
		case actWaitStream:
			l.s.readyAt = dec.ReadyAt
			l.lateWarning(timing, now)
			l.writeState(leaf, timing, dec.NextAttemptAt)
			l.sleep(ctx, dec.At, leaf.Serial, true)
		case actAttempt:
			if ctx.Err() != nil {
				return
			}
			l.lateWarning(timing, now)
			l.attempt(ctx, leaf, timing)
		}
	}
}

// newLeaf starts a new cycle for a changed held certificate: the retry
// state and the lost-response flag go.
func (l *Loop) newLeaf(leaf Leaf) {
	l.s = cycleState{
		serial:        leaf.Serial,
		attempts:      l.s.attempts,
		lastAttemptAt: l.s.lastAttemptAt,
		lastResult:    ResultOK,
	}
	if l.s.attempts > 0 && l.last.LastResult == ResultOK {
		l.s.outcome = l.last.Outcome
	}
	l.logs.reset()
}

func (l *Loop) onReadError(now time.Time, err error) {
	if !l.s.readFailing {
		roslog.W(ReadErrorMessage, err)
	}
	l.s.readFailing = true
	st := l.baseState()
	st.HeldSerial, st.NotAfter, st.RenewalDueAt = l.last.HeldSerial, l.last.NotAfter, l.last.RenewalDueAt
	st.LastResult = ResultRefused
	st.Outcome = ""
	st.LastErrorCode = "CERTIFICATE_UNREADABLE"
	st.LastErrorMessage = err.Error()
	st.NextAttemptAt = stamp(now.Add(readErrorRetry))
	l.store(st)
}

func (l *Loop) onExpired(leaf Leaf, timing certpolicy.Timing, dec decision) {
	if !l.s.expiredLogged {
		l.s.expiredLogged = true
		roslog.E(ExpiredMessage, nil,
			"not_after", stamp(leaf.NotAfter),
			"held_serial", leaf.Serial,
			"next_step", certpolicy.ExpiredRemedy(leaf.NotAfter))
	}
	l.s.lastResult = ResultExpired
	l.writeState(leaf, timing, dec.NextAttemptAt)
}

// lateWarning writes the one WARN line when less than a sixth of the
// certificate life is left.
func (l *Loop) lateWarning(timing certpolicy.Timing, now time.Time) {
	if l.s.lateWarned || timing.Life <= 0 || timing.Remaining >= timing.Life/6 {
		return
	}
	l.s.lateWarned = true
	kind := l.s.lastKind
	if kind == 0 {
		kind = certpolicy.Transient
	}
	roslog.W(LateMessage, nil,
		"time_left", timing.Remaining.Round(time.Second).String(),
		"not_after", stamp(timing.NotAfter),
		"held_serial", l.s.serial,
		"consecutive_failures", l.s.failures,
		"next_step", certpolicy.NextStep(kind, l.s.retryAt))
}

// attempt runs one renewal and plans the next step.
func (l *Loop) attempt(ctx context.Context, leaf Leaf, timing certpolicy.Timing) {
	l.s.attempts++
	l.s.lastAttemptAt = time.Now()
	l.s.readyAt = time.Time{}

	// The predicate hears every failed mint call of this attempt. A
	// possibly lost answer marks the held serial.
	lost := func(err error) bool {
		o := certpolicy.Classify(err, certpolicy.Prior{ResponseMayBeLost: l.s.lost})
		if o.ResponseMayBeLost {
			l.s.lost = true
		}
		return o.ResponseMayBeLost
	}
	// A shutdown does not cut an attempt: the attempt has its own limit.
	actx, cancel := context.WithTimeout(context.WithoutCancel(ctx), attemptLimit)
	res, err := l.d.Renew(actx, Request{ExpectedSerial: leaf.Serial, LostResponse: lost})
	cancel()
	now := time.Now()
	line := attemptLine{Attempt: l.s.attempts, HeldSerial: leaf.Serial}

	switch {
	case err == nil:
		l.s.failures = 0
		l.s.retryAt = time.Time{}
		l.s.lost = false
		l.s.lastResult, l.s.outcome, l.s.lastKind = ResultOK, res.Status, 0
		l.s.errCode, l.s.errMessage = "", ""
		line.Result, line.Outcome, line.NewSerial = ResultOK, res.Status, res.Serial
		next, left := l.nextAfterSuccess(now)
		line.NextAttempt, line.TimeLeft = next, left
		if res.Serial == leaf.Serial {
			// The live certificate did not change: do not attempt again
			// at once.
			l.s.retryAt = now.Add(lockBusyRetry)
			line.NextAttempt = l.s.retryAt
		}
		l.logs.write(line)
		st := l.stateFor(leaf, timing, line.NextAttempt)
		l.store(st)
		return

	case errors.Is(err, certstore.ErrRenewalInProgress):
		l.s.retryAt = now.Add(lockBusyRetry)
		l.s.lastResult, l.s.outcome = ResultWaiting, ""
		l.s.errCode, l.s.errMessage = ReasonLockBusy, err.Error()
		line.Result, line.Reason = ResultWaiting, ReasonLockBusy
	default:
		o := certpolicy.Classify(err, certpolicy.Prior{ResponseMayBeLost: l.s.lost})
		result := o.Kind.ReportResult()
		if result != ResultWaiting {
			l.s.failures++
		}
		t := certpolicy.Evaluate(leaf.NotBefore, leaf.NotAfter, l.d.NodeID, now)
		plan := certpolicy.Next(o, t, l.s.failures, now, l.d.Rand)
		l.s.retryAt = plan.At
		l.s.lastResult, l.s.outcome, l.s.lastKind = result, o.Kind.String(), o.Kind
		l.s.errCode, l.s.errMessage = o.Code, o.Message
		line.Result, line.Outcome, line.Reason = result, o.Kind.String(), o.Code
		line.Failure, line.Step = true, l.s.failures-1
		if o.Unclassified {
			line.Unclassified, line.GRPCCode, line.Status = true, o.GRPCCode.String(), o.Message
		}
	}
	line.NextAttempt = l.s.retryAt
	line.TimeLeft = leaf.NotAfter.Sub(now)
	if line.TimeLeft < 0 {
		line.TimeLeft = 0
	}
	l.logs.write(line)
	l.writeState(leaf, timing, l.s.retryAt)
}

// nextAfterSuccess reads the new leaf for the attempt line.
func (l *Loop) nextAfterSuccess(now time.Time) (time.Time, time.Duration) {
	child, err := l.d.ReadLeaf()
	if err != nil {
		return time.Time{}, 0
	}
	t := certpolicy.Evaluate(child.NotBefore, child.NotAfter, l.d.NodeID, now)
	return t.DueAt, t.Remaining
}

func (l *Loop) baseState() State {
	return State{
		Version:             stateVersion,
		PID:                 l.d.PID,
		LastAttemptAt:       stamp(l.s.lastAttemptAt),
		ConsecutiveFailures: l.s.failures,
		Attempts:            l.s.attempts,
	}
}

// stateFor builds the state of the held leaf.
func (l *Loop) stateFor(leaf Leaf, timing certpolicy.Timing, next time.Time) State {
	st := l.baseState()
	st.HeldSerial = leaf.Serial
	st.NotAfter = stamp(leaf.NotAfter)
	st.RenewalDueAt = stamp(timing.DueAt)
	st.LastResult = l.s.lastResult
	if st.LastResult == "" {
		st.LastResult = ResultOK
	}
	st.Outcome = l.s.outcome
	st.LastErrorCode = l.s.errCode
	st.LastErrorMessage = l.s.errMessage
	st.NextAttemptAt = stamp(next)
	return st
}

func (l *Loop) writeState(leaf Leaf, timing certpolicy.Timing, next time.Time) {
	l.store(l.stateFor(leaf, timing, next))
}

// store writes st when it differs from the last written state. A failed
// write is one WARN line and does not stop the loop.
func (l *Loop) store(st State) {
	cmp := st
	cmp.UpdatedAt = l.last.UpdatedAt
	if l.last.Version != 0 && cmp == l.last {
		return
	}
	st.UpdatedAt = stamp(time.Now())
	if err := writeState(l.d.StatePath, st); err != nil {
		roslog.W(StateWriteMessage, err, "path", l.d.StatePath)
		return
	}
	l.last = st
}

// sleep waits until at, until ctx ends, or until the held serial differs
// from serial (checked every 30 s; "" skips the check). With
// onConnection it also returns on a connection change.
func (l *Loop) sleep(ctx context.Context, at time.Time, serial string, onConnection bool) {
	var wake <-chan struct{}
	if onConnection {
		wake = l.wake
	} else {
		// Drop a stale connection change.
		select {
		case <-l.wake:
		default:
		}
	}
	for {
		d := time.Until(at)
		if d <= 0 {
			return
		}
		t := time.NewTimer(min(d, serialCheck))
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-wake:
			t.Stop()
			return
		case <-t.C:
		}
		if serial != "" {
			if leaf, err := l.d.ReadLeaf(); err == nil && leaf.Serial != serial {
				return
			}
		}
	}
}

// sleepCtx sleeps d and reports whether ctx is still live.
func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
