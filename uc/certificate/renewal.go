package certificate

import (
	"context"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/runos-official/nodeagent/backend"
	"github.com/runos-official/nodeagent/config"
	"github.com/runos-official/nodeagent/l2sec"
	"github.com/runos-official/nodeagent/roslog"
	"github.com/runos-official/nodeagent/uc/certstore"
	"github.com/runos-official/nodeagent/uc/csrcert"
)

// Status values of a renewal.
const (
	// StatusRenewed: this call minted a child and adopted it.
	StatusRenewed = "renewed"
	// StatusAlreadyRenewed: the live certificate was already new when this
	// call held the lock (another process renewed, recovery rolled a staged
	// child forward, or another writer replaced the files). No mint.
	StatusAlreadyRenewed = "already_renewed"
)

// Limits of one mint sequence.
const (
	// maxMintAttempts counts mint attempts. Each attempt has a fresh key and
	// CSR. The clock-skew correction inside one attempt is not an attempt.
	maxMintAttempts = 3
	// mintBudget is the time limit of the whole sequence.
	mintBudget = 120 * time.Second
	// mintCallTimeout is the most time one wire call gets.
	mintCallTimeout = 30 * time.Second
	// busyWait is the wait after a ResourceExhausted refusal of the retry.
	busyWait = 5 * time.Second
	// startLockWait is the renewal lock wait of the recovery at start.
	startLockWait = 10 * time.Second
	// lockProbe is the first, short lock try that tells the manual command
	// whether it must wait.
	lockProbe = time.Millisecond
)

// preMintPrefixes start the refusals that Nodeward sends before it mints.
// Such a refusal under a "may be lost" code is not a lost answer.
var preMintPrefixes = []string{
	"could not check the minting rules",
	"could not check the mint proof",
	"authentication check failed",
}

// Result is the outcome of one renewal.
type Result struct {
	Status string
	// Serial and NotAfter name the live certificate after the call.
	Serial   string
	NotAfter time.Time
	// NodeLogFailed is true when the node log entry after an adoption
	// failed. The child stays.
	NodeLogFailed bool
}

// mintClient is the Nodeward method that a renewal calls.
type mintClient interface {
	RegenerateCertificate(ctx context.Context, in *l2sec.RegenerateCertificateRequest, opts ...grpc.CallOption) (*l2sec.RegenerateCertificateResponse, error)
}

// dialError marks a failure to connect before any mint call.
type dialError struct{ err error }

func (e *dialError) Error() string { return "connect to Nodeward: " + e.err.Error() }
func (e *dialError) Unwrap() error { return e.err }

// storeError marks a failure of the certificate store (recovery, prepare,
// adoption), as opposed to a refused or failed request.
type storeError struct {
	op  string
	err error
}

func (e *storeError) Error() string { return e.op + ": " + e.err.Error() }
func (e *storeError) Unwrap() error { return e.err }

// Steps that renewOnce reports to deps.OnStep.
const (
	stepLockWait = "lock-wait"
	stepRequest  = "request"
	stepAdopt    = "adopt"
)

// deps is everything renewOnce touches outside the certificate directory.
// A test sets every field and needs no network.
type deps struct {
	Paths certstore.Paths
	// Dial opens one connection for the whole mint sequence. close ends it.
	Dial func(ctx context.Context) (c mintClient, close func(), err error)
	// NodeLog sends one node log entry after an adoption.
	NodeLog func(event, message string) error
	// Event and Message are the node log entry of this caller.
	Event   string
	Message string
	// Now is the clock; Sleep waits on it.
	Now   func() time.Time
	Sleep func(ctx context.Context, d time.Duration) error
	// LockWait is the renewal lock wait; zero means the store default.
	LockWait time.Duration
	// LostResponse says that the answer of a failed mint call may be lost.
	LostResponse func(error) bool
	// ExpectedSerial is the serial of the certificate that the caller
	// evaluated when it decided to renew. "" skips that check.
	ExpectedSerial string
	// OnStep, when set, hears each step (manual command progress).
	OnStep func(step string)
	// storeHook, when set, is the crash hook of the store. Tests only.
	storeHook func(point string) error
}

// defaultDeps gives the production dependencies for one caller.
func defaultDeps(event, message string) deps {
	return deps{
		Paths:        configuredPaths(),
		Dial:         dialNodeward,
		NodeLog:      func(event, message string) error { return backend.AddNodelog(3, event, message) },
		Event:        event,
		Message:      message,
		Now:          time.Now,
		Sleep:        sleepCtx,
		LostResponse: MayBeLost,
	}
}

// configuredPaths reads the credential paths. It creates no file.
func configuredPaths() certstore.Paths {
	cert, key, ca := config.MTLSPaths()
	return certstore.Paths{Cert: cert, Key: key, CA: ca}
}

func dialNodeward(context.Context) (mintClient, func(), error) {
	c, _, cancel, conn, err := backend.NodewardL2Sec()
	if err != nil {
		return nil, nil, err
	}
	return c, func() {
		cancel()
		conn.Close()
	}, nil
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// MayBeLost is the default lost-response predicate. The answer of a failed
// mint call may be lost when the gRPC code is Unavailable,
// DeadlineExceeded, Canceled, Internal or Unknown, and the text does not
// start with a refusal that Nodeward sends before a mint. An error that is
// not a gRPC status (a local failure) is never a lost answer.
func MayBeLost(err error) bool {
	st, ok := status.FromError(err)
	if !ok || err == nil {
		return false
	}
	switch st.Code() {
	case codes.Unavailable, codes.DeadlineExceeded, codes.Canceled, codes.Internal, codes.Unknown:
	default:
		return false
	}
	for _, p := range preMintPrefixes {
		if strings.HasPrefix(st.Message(), p) {
			return false
		}
	}
	return true
}

// renewOnce is the one renewal function. It holds the renewal lock,
// recovers the store, mints with a fresh key and adopts the child through
// the store. It never restores the parent after Nodeward returned a child
// that passes the local checks.
func renewOnce(ctx context.Context, d deps) (Result, error) {
	s := certstore.Open(d.Paths)
	if d.storeHook != nil {
		s.SetCrashHook(d.storeHook)
	}
	dir := filepath.Dir(d.Paths.Cert)

	// 1. The held serial and the journal sequence before the lock.
	serialBefore, _ := s.HeldSerial()
	recBefore, _, _ := s.Adopted()

	// 2. The renewal lock.
	release, err := takeLock(ctx, dir, d)
	if err != nil && (errors.Is(err, certstore.ErrRenewalInProgress) || ctx.Err() != nil) {
		return Result{}, err
	}
	if err != nil {
		return Result{}, &storeError{"take the renewal lock", err}
	}
	var once sync.Once
	unlock := func() { once.Do(release) }
	defer unlock()
	// A caller that gave up while this call waited gets no mint.
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}

	// 3. Recover.
	recErr := s.Recover()
	switch {
	case recErr == nil, errors.Is(recErr, certstore.ErrAdoptionSuperseded):
	case errors.Is(recErr, certstore.ErrRecoveredToParent):
		roslog.W("Certificate store restored the held certificate; renewing from it", recErr)
	default:
		return Result{}, &storeError{"recover the certificate store", recErr}
	}

	// 4. The held serial and the journal sequence again.
	serialAfter, err := s.HeldSerial()
	if err != nil {
		return Result{}, &storeError{"read the held certificate", err}
	}
	recAfter, _, err := s.Adopted()
	if err != nil {
		return Result{}, &storeError{"read the certificate store journal", err}
	}

	// 5. The live certificate is already new: no mint.
	if why := alreadyRenewed(serialBefore, serialAfter, recBefore.Sequence, recAfter.Sequence, recErr, d.ExpectedSerial); why != "" {
		res, err := liveResult(d.Paths, StatusAlreadyRenewed)
		if err != nil {
			return Result{}, err
		}
		roslog.I("Certificate already renewed; not requesting another", "reason", why, "serial", res.Serial,
			"not_after", res.NotAfter.Format(time.RFC3339))
		return res, nil
	}

	// 6. Prepare.
	if err := s.Prepare(); err != nil {
		return Result{}, &storeError{"prepare the certificate store", err}
	}

	// 7. Mint.
	step(d, stepRequest)
	m, err := mint(ctx, d)
	if err != nil {
		// Remove the copy of the parent that Prepare made.
		discardPrepared(s)
		return Result{}, err
	}

	// 8. Adopt.
	step(d, stepAdopt)
	if err := s.Adopt(m); err != nil {
		if errors.Is(err, certstore.ErrChildRejected) {
			discardPrepared(s)
			return Result{}, err
		}
		// A failure after the commit point leaves the child staged; Recover
		// rolls it forward. A child that passed its checks is never thrown
		// away.
		roslog.W("Certificate adoption stopped; recovering the store", err)
		if rerr := s.Recover(); rerr != nil {
			return Result{}, &storeError{"adopt the renewed certificate", errors.Join(err, rerr)}
		}
		if rec, _, rerr := s.Adopted(); rerr != nil || rec.Sequence <= recAfter.Sequence {
			return Result{}, &storeError{"adopt the renewed certificate", err}
		}
	}

	// 9. Release the lock.
	unlock()
	res, err := liveResult(d.Paths, StatusRenewed)
	if err != nil {
		return Result{}, err
	}

	// 10. The node log entry. A failure leaves the child.
	if err := d.NodeLog(d.Event, d.Message); err != nil {
		roslog.W("Could not send the certificate renewal to the node log; the renewed certificate stays", err,
			"event", d.Event)
		res.NodeLogFailed = true
	}
	roslog.I("Certificate renewed", "serial", res.Serial, "not_after", res.NotAfter.Format(time.RFC3339))
	return res, nil
}

// takeLock takes the renewal lock. When OnStep is set, it first tries once
// with a short wait, so the caller can report the wait.
func takeLock(ctx context.Context, dir string, d deps) (func(), error) {
	if d.OnStep != nil {
		release, err := certstore.AcquireRenewalLock(ctx, dir, lockProbe)
		if !errors.Is(err, certstore.ErrRenewalInProgress) {
			return release, err
		}
		d.OnStep(stepLockWait)
	}
	return certstore.AcquireRenewalLock(ctx, dir, d.LockWait)
}

func step(d deps, name string) {
	if d.OnStep != nil {
		d.OnStep(name)
	}
}

// alreadyRenewed returns why the live certificate is already new, or "".
func alreadyRenewed(before, after string, seqBefore, seqAfter uint64, recErr error, expected string) string {
	switch {
	case errors.Is(recErr, certstore.ErrAdoptionSuperseded):
		return "superseded"
	case seqAfter > seqBefore:
		return "recovered"
	case before != "" && after != before:
		return "serial_changed"
	case expected != "" && after != expected:
		return "not_expected_serial"
	}
	return ""
}

// discardPrepared runs Recover after a renewal that adopted nothing, so no
// copy of the parent key stays on disk.
func discardPrepared(s *certstore.Store) {
	if err := s.Recover(); err != nil {
		roslog.W("Could not clean up the certificate store after a failed renewal", err)
	}
}

// liveResult reads the serial and NotAfter of the live certificate.
func liveResult(p certstore.Paths, st string) (Result, error) {
	b, err := os.ReadFile(p.Cert)
	if err != nil {
		return Result{}, fmt.Errorf("read the held certificate: %w", err)
	}
	block, _ := pem.Decode(b)
	if block == nil {
		return Result{}, errors.New("read the held certificate: no PEM block")
	}
	c, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return Result{}, fmt.Errorf("parse the held certificate: %w", err)
	}
	return Result{Status: st, Serial: fmt.Sprintf("%x", c.SerialNumber), NotAfter: c.NotAfter}, nil
}

// mint runs the mint sequence on one connection: an attempt with a fresh
// key; one more attempt at once when the answer may be lost; one more 5 s
// after a ResourceExhausted refusal of that retry. At most 3 attempts,
// inside 120 s. Any other refusal returns unchanged.
func mint(ctx context.Context, d deps) (certstore.Material, error) {
	deadline := d.Now().Add(mintBudget)
	c, closeConn, err := d.Dial(ctx)
	if err != nil {
		return certstore.Material{}, &dialError{err: err}
	}
	defer closeConn()

	callTimeout := func() time.Duration { return min(mintCallTimeout, deadline.Sub(d.Now())) }
	var lastErr error
	for attempt := 1; attempt <= maxMintAttempts; attempt++ {
		if callTimeout() <= 0 {
			break
		}
		roslog.I("Requesting a renewed certificate from Nodeward", "attempt", attempt)
		got, err := requestCertificate(ctx, c, d.Paths, d.Now, callTimeout)
		if err == nil {
			return got.material()
		}
		lastErr = err
		lost := errors.Is(err, csrcert.ErrUnusableResponse) || d.LostResponse(err)
		switch {
		case attempt == 1 && lost:
			roslog.W("The renewal answer may be lost; asking again at once with a fresh key", err, "attempt", attempt)
		case attempt == 2 && status.Code(err) == codes.ResourceExhausted:
			if deadline.Sub(d.Now()) <= busyWait {
				return certstore.Material{}, err
			}
			roslog.W("Nodeward is still busy with the earlier request; asking again in 5 s with a fresh key", err,
				"attempt", attempt)
			if serr := d.Sleep(ctx, busyWait); serr != nil {
				return certstore.Material{}, err
			}
		default:
			return certstore.Material{}, err
		}
	}
	if lastErr == nil {
		lastErr = context.DeadlineExceeded
	}
	return certstore.Material{}, lastErr
}

// RecoverAtStart brings the certificate store to rest before the agent
// checks its registration, so a crash in the middle of an adoption never
// ends a start with a key that does not match the certificate. On an
// unregistered node, or when the certificate directory does not exist, it
// changes nothing and writes no log line. No result stops the start.
func RecoverAtStart() {
	recoverAtStart(context.Background(), configuredPaths(), startLockWait)
}

func recoverAtStart(ctx context.Context, p certstore.Paths, wait time.Duration) {
	dir := filepath.Dir(p.Cert)
	if info, err := os.Stat(dir); err != nil || !info.IsDir() {
		return
	}
	// Registered, or a store with state: a non-empty certificate or a
	// journal. Otherwise there is nothing to recover and no lock to make.
	cert, err := os.Stat(p.Cert)
	_, jerr := os.Stat(filepath.Join(dir, certstore.JournalName))
	if (err != nil || cert.Size() == 0) && jerr != nil {
		return
	}
	release, err := certstore.AcquireRenewalLock(ctx, dir, wait)
	if err != nil {
		roslog.W("Certificate store recovery at start skipped; the start continues", err)
		return
	}
	defer release()
	err = certstore.Open(p).Recover()
	switch {
	case err == nil:
	case errors.Is(err, certstore.ErrRecoveredToParent), errors.Is(err, certstore.ErrAdoptionSuperseded):
		// The store logged one warning line.
	default:
		roslog.E("Certificate store recovery at start failed; the start continues", err)
	}
}
