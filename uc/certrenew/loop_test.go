package certrenew

import (
	"context"
	"crypto/ecdsa"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"io/fs"
	"os"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/runos-official/nodeagent/uc/certificate"
	"github.com/runos-official/nodeagent/uc/certpolicy"
	"github.com/runos-official/nodeagent/uc/certstore"
)

// Criterion 1.
func TestLoopRenewsTwiceWithoutRestart(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		captureLogs(t)
		start := time.Now()
		c := newCreds(t, start, start.Add(time.Hour))
		f := &fakeNodeward{t: t, c: c, life: time.Hour}
		l := newLoop(c, realRenew(c, f))
		startLoop(t, l)
		l.NotifyConnected()

		due := certpolicy.Evaluate(start, start.Add(time.Hour), testNodeID, start).DueAt
		parent := c.serial(t)
		time.Sleep(due.Sub(start) - time.Second)
		synctest.Wait()
		if f.count() != 0 {
			t.Fatalf("mint before the first due point (%d calls)", f.count())
		}

		time.Sleep(2 * time.Second)
		synctest.Wait()
		child := c.serial(t)
		if f.count() != 1 || child == parent {
			t.Fatalf("after the due point: calls = %d, serial changed = %v; want 1 mint and a new serial", f.count(), child != parent)
		}
		if got := f.at[0]; got.Before(due) || got.After(due.Add(time.Second)) {
			t.Fatalf("first mint at %s, want the due point %s", got, due)
		}

		leaf, _ := ReadLeafFile(c.paths.Cert)
		childDue := certpolicy.Evaluate(leaf.NotBefore, leaf.NotAfter, testNodeID, time.Now()).DueAt
		time.Sleep(time.Until(childDue) - time.Second)
		synctest.Wait()
		if f.count() != 1 {
			t.Fatalf("mint before the child's due point (%d calls)", f.count())
		}
		time.Sleep(2 * time.Second)
		synctest.Wait()
		if f.count() != 2 || c.serial(t) == child {
			t.Fatalf("after the child's due point: calls = %d; want a second mint and a new serial", f.count())
		}
		if l.maxRunning.Load() != 1 || l.running.Load() != 1 {
			t.Fatalf("loop goroutines: max %d, now %d; want one", l.maxRunning.Load(), l.running.Load())
		}
	})
}

// Criterion 2.
func TestReconnectKeepsOneLoopAndSchedule(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		captureLogs(t)
		start := time.Now()
		c := newCreds(t, start, start.Add(24*time.Hour))
		s := &stub{steps: []func(Request) (certificate.Result, error){fails(status.Error(codes.Unavailable, "x"))}}
		l := newLoop(c, s.renew)
		ctx, cancel := context.WithCancel(context.Background())
		defer func() { cancel(); l.Wait(time.Minute) }()
		l.Start(ctx)
		l.NotifyConnected()
		synctest.Wait()
		before := c.readState(t).NextAttemptAt
		if before == "" {
			t.Fatal("no nextAttemptAt after the first cycle")
		}
		for i := 0; i < 50; i++ {
			l.NotifyDisconnected()
			time.Sleep(time.Second)
			l.NotifyConnected()
			time.Sleep(time.Second)
			synctest.Wait()
		}
		for i := 0; i < 50; i++ {
			l.Start(ctx)
		}
		synctest.Wait()
		if got := c.readState(t).NextAttemptAt; got != before {
			t.Fatalf("nextAttemptAt changed from %s to %s", before, got)
		}
		if s.count() != 0 {
			t.Fatalf("%d requests before the due point", s.count())
		}
		if l.maxRunning.Load() != 1 {
			t.Fatalf("%d loop goroutines ran, want one", l.maxRunning.Load())
		}
	})
}

// dueCreds gives a leaf that is due at once (10 minutes of 1 hour left).
func dueCreds(t *testing.T) *creds {
	now := time.Now()
	return newCreds(t, now.Add(-50*time.Minute), now.Add(10*time.Minute))
}

// Criterion 3.
func TestFirstAttemptWaitsForConnection(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		captureLogs(t)
		c := dueCreds(t)
		s := &stub{steps: []func(Request) (certificate.Result, error){fails(status.Error(codes.Unavailable, "x"))}}
		l := newLoop(c, s.renew)
		startLoop(t, l)
		time.Sleep(time.Minute)
		synctest.Wait()
		if s.count() != 0 {
			t.Fatal("request before the connect signal")
		}
		l.NotifyConnected()
		connected := time.Now()
		time.Sleep(9 * time.Second)
		synctest.Wait()
		if s.count() != 0 {
			t.Fatal("request before the settle time")
		}
		time.Sleep(2 * time.Second)
		synctest.Wait()
		if s.count() != 1 {
			t.Fatalf("requests = %d after connect plus settle, want 1", s.count())
		}
		if d := s.times()[0].Sub(connected); d != 10*time.Second {
			t.Fatalf("request %s after the connect signal, want 10s", d)
		}
	})
}

// Criterion 3.
func TestAttemptsWithoutConnectionAfterTwoMinutes(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		captureLogs(t)
		c := dueCreds(t)
		s := &stub{steps: []func(Request) (certificate.Result, error){fails(status.Error(codes.Unavailable, "x"))}}
		l := newLoop(c, s.renew)
		start := time.Now()
		startLoop(t, l)
		time.Sleep(3 * time.Minute)
		synctest.Wait()
		if s.count() == 0 {
			t.Fatal("no request without a connection after 3 minutes")
		}
		if d := s.times()[0].Sub(start); d < 2*time.Minute || d > 2*time.Minute+30*time.Second {
			t.Fatalf("first request %s after start, want 2:00 to 2:30", d)
		}
	})
}

// Criterion 4.
func TestScheduleFollowsPolicy(t *testing.T) {
	lost := status.Error(codes.Unavailable, "connection reset by peer")
	already := status.Error(codes.FailedPrecondition, "certificate was already regenerated")
	type tc struct {
		kind certpolicy.Kind
		err  func(now time.Time) error
		// lostFirst makes the attempt report a lost answer first.
		lostFirst bool
	}
	cases := []tc{
		{certpolicy.WaitUntil, func(now time.Time) error {
			return status.Error(codes.FailedPrecondition, "certificate is not inside its renewal window; renewal opens at "+
				now.Add(5*time.Minute).UTC().Format(time.RFC3339))
		}, false},
		{certpolicy.Transient, func(time.Time) error { return lost }, false},
		{certpolicy.LostResponse, func(time.Time) error { return already }, true},
		{certpolicy.TerminalForThisCertificate, func(time.Time) error { return already }, false},
		{certpolicy.NeedsAgentUpgrade, func(time.Time) error {
			return status.Error(codes.FailedPrecondition, "legacy key minting through nodehub is turned off")
		}, false},
		{certpolicy.CertificateExpired, func(time.Time) error { return status.Error(codes.Unauthenticated, "certificate has expired") }, false},
		{certpolicy.IdentityRepair, func(time.Time) error { return status.Error(codes.NotFound, "node not found") }, false},
		{certpolicy.BadRequest, func(time.Time) error {
			return status.Error(codes.InvalidArgument, "mint proof is missing or malformed")
		}, false},
	}
	for _, k := range certpolicy.AllKinds() {
		found := false
		for _, c := range cases {
			found = found || c.kind == k
		}
		if !found {
			t.Fatalf("no case for kind %s", k)
		}
	}
	for _, tc := range cases {
		t.Run(tc.kind.String(), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				captureLogs(t)
				now := time.Now()
				// 30-day life, 5 days left: due, far from expiry.
				c := newCreds(t, now.Add(-25*24*time.Hour), now.Add(5*24*time.Hour))
				var sent error
				s := &stub{steps: []func(Request) (certificate.Result, error){func(r Request) (certificate.Result, error) {
					if tc.lostFirst {
						r.LostResponse(lost)
					}
					sent = tc.err(time.Now())
					return certificate.Result{}, sent
				}}}
				l := newLoop(c, s.renew)
				startLoop(t, l)
				l.NotifyConnected()
				time.Sleep(settle + time.Second)
				synctest.Wait()
				if s.count() != 1 {
					t.Fatalf("requests = %d, want 1", s.count())
				}
				at := s.times()[0]
				o := certpolicy.Classify(sent, certpolicy.Prior{ResponseMayBeLost: tc.lostFirst})
				if o.Kind != tc.kind {
					t.Fatalf("the case gives kind %s, want %s", o.Kind, tc.kind)
				}
				leaf, _ := ReadLeafFile(c.paths.Cert)
				want := certpolicy.Next(o, certpolicy.Evaluate(leaf.NotBefore, leaf.NotAfter, testNodeID, at), 1, at,
					func() float64 { return 0.5 }).At
				st := c.readState(t)
				if st.NextAttemptAt != stamp(want) {
					t.Fatalf("nextAttemptAt = %s, want the policy time %s", st.NextAttemptAt, stamp(want))
				}
				if st.Outcome != tc.kind.String() || st.LastResult != tc.kind.ReportResult() {
					t.Fatalf("state outcome %s / %s, want %s / %s", st.Outcome, st.LastResult, tc.kind, tc.kind.ReportResult())
				}
				if !tc.kind.Retries() {
					time.Sleep(5*time.Hour + 59*time.Minute)
					synctest.Wait()
					if s.count() != 1 {
						t.Fatalf("kind %s sent %d requests in 5 h 59 min, want 1", tc.kind, s.count())
					}
					// A changed serial starts a new cycle at once.
					now := time.Now()
					c.replaceLeaf(t, now.Add(-25*24*time.Hour), now.Add(5*24*time.Hour))
					time.Sleep(serialCheck + time.Second)
					synctest.Wait()
					if s.count() != 2 {
						t.Fatalf("a changed serial gave %d requests, want a new attempt", s.count())
					}
					if d := s.times()[1].Sub(now); d > serialCheck {
						t.Fatalf("new attempt %s after the serial change, want at most %s", d, serialCheck)
					}
				}
			})
		})
	}
}

// Criterion 5.
func TestWarnOncePerBackoffStep(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		logs := captureLogs(t)
		now := time.Now()
		c := newCreds(t, now.Add(-25*24*time.Hour), now.Add(5*24*time.Hour))
		reset := status.Error(codes.Unavailable, "connection reset by peer")
		busy := status.Error(codes.ResourceExhausted, "gateway busy")
		s := &stub{}
		s.steps = []func(Request) (certificate.Result, error){func(Request) (certificate.Result, error) {
			if s.count() > 100 {
				return certificate.Result{}, busy
			}
			return certificate.Result{}, reset
		}}
		l := newLoop(c, s.renew)
		startLoop(t, l)
		l.NotifyConnected()
		for s.count() < 100 {
			time.Sleep(time.Minute)
		}
		synctest.Wait()
		lines := logs.lines(t, AttemptMessage)
		if len(lines) != 100 {
			t.Fatalf("attempt lines = %d, want 100", len(lines))
		}
		if w := countLevel(lines, "WARN"); w > 7 || w < 1 {
			t.Fatalf("WARN lines = %d for one reason, want 1 to 7", w)
		}
		warns := countLevel(lines, "WARN")
		for s.count() < 101 {
			time.Sleep(time.Minute)
		}
		synctest.Wait()
		lines = logs.lines(t, AttemptMessage)
		if got := countLevel(lines, "WARN"); got != warns+1 {
			t.Fatalf("a new reason gave %d new WARN lines, want 1", got-warns)
		}
		for _, l := range lines {
			for _, k := range []string{"attempt", "result", "outcome", "reason", "time_left", "next_attempt", "held_serial"} {
				if _, ok := l[k]; !ok {
					t.Fatalf("attempt line has no %s: %v", k, l)
				}
			}
		}
	})
}

// Criterion 6.
func TestRsaParentRenewsToEcdsa(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		captureLogs(t)
		k, err := rsa.GenerateKey(rand.Reader, 2048)
		if err != nil {
			t.Fatal(err)
		}
		keyPEM := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(k)})
		now := time.Now()
		c := writeCreds(t, keyPEM, &k.PublicKey, now.Add(-50*time.Minute), now.Add(10*time.Minute))
		f := &fakeNodeward{t: t, c: c, life: time.Hour}
		l := newLoop(c, realRenew(c, f))
		startLoop(t, l)
		l.NotifyConnected()
		time.Sleep(settle + time.Second)
		synctest.Wait()
		if f.count() != 1 {
			t.Fatalf("calls = %d, want 1", f.count())
		}
		if f.calls[0].CsrPem == "" || f.calls[0].Proof == nil {
			t.Fatal("the request carried no CSR or no proof")
		}
		pair, err := tls.LoadX509KeyPair(c.paths.Cert, c.paths.Key)
		if err != nil {
			t.Fatalf("the new key does not match the new certificate: %v", err)
		}
		if _, ok := pair.PrivateKey.(*ecdsa.PrivateKey); !ok {
			t.Fatalf("the new key is %T, want ECDSA", pair.PrivateKey)
		}
		if st := c.readState(t); st.LastResult != ResultOK || st.Outcome != certificate.StatusRenewed {
			t.Fatalf("state %s / %s, want ok / renewed", st.LastResult, st.Outcome)
		}
	})
}

// Criterion 7.
func TestStateFileMembers(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		captureLogs(t)
		c := dueCreds(t)
		f := &fakeNodeward{t: t, c: c, life: time.Hour}
		s := &stub{steps: []func(Request) (certificate.Result, error){
			fails(status.Error(codes.Unavailable, "connection reset by peer")),
			func(r Request) (certificate.Result, error) { return realRenew(c, f)(context.Background(), r) },
		}}
		l := newLoop(c, s.renew)
		startLoop(t, l)
		synctest.Wait()

		// The first cycle writes the file before any attempt.
		st := c.readState(t)
		if st.LastResult != ResultOK || st.Attempts != 0 || st.HeldSerial == "" {
			t.Fatalf("first-cycle state = %+v", st)
		}
		checkFile(t, c.state, []string{"version", "pid", "updatedAt", "heldSerial", "notAfter", "renewalDueAt",
			"lastResult", "consecutiveFailures", "nextAttemptAt", "attempts"})

		l.NotifyConnected()
		time.Sleep(settle + time.Second)
		synctest.Wait()
		st = c.readState(t)
		if st.LastResult != ResultTransientFailure || st.ConsecutiveFailures != 1 || st.Attempts != 1 ||
			st.LastErrorCode != "Unavailable" || st.LastErrorMessage != "connection reset by peer" || st.Outcome != "Transient" {
			t.Fatalf("state after a failure = %+v", st)
		}
		checkFile(t, c.state, []string{"version", "pid", "updatedAt", "heldSerial", "notAfter", "renewalDueAt",
			"lastAttemptAt", "lastResult", "outcome", "lastErrorCode", "lastErrorMessage", "consecutiveFailures",
			"nextAttemptAt", "attempts"})

		time.Sleep(2 * time.Minute)
		synctest.Wait()
		st = c.readState(t)
		if st.LastResult != ResultOK || st.ConsecutiveFailures != 0 || st.Attempts != 2 || st.HeldSerial != c.serial(t) ||
			st.LastErrorCode != "" || st.PID != 4242 || st.Version != 1 {
			t.Fatalf("state after a success = %+v", st)
		}
		checkFile(t, c.state, []string{"version", "pid", "updatedAt", "heldSerial", "notAfter", "renewalDueAt",
			"lastAttemptAt", "lastResult", "consecutiveFailures", "nextAttemptAt", "attempts"})
	})
}

// checkFile checks mode 0644, valid JSON, no PEM text and the members.
func checkFile(t *testing.T, path string, members []string) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != fs.FileMode(0o644) {
		t.Fatalf("state file mode %v, want 0644", info.Mode().Perm())
	}
	b, _ := os.ReadFile(path)
	if strings.Contains(string(b), "-----BEGIN") || strings.Contains(string(b), "PRIVATE") {
		t.Fatal("state file holds PEM text")
	}
	var m map[string]any
	if err := jsonUnmarshal(b, &m); err != nil {
		t.Fatalf("state file is not JSON: %v", err)
	}
	for _, k := range members {
		if _, ok := m[k]; !ok {
			t.Fatalf("state file has no %s: %s", k, b)
		}
	}
}

// Criterion 7.
func TestExpiredSendsNoRequest(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		logs := captureLogs(t)
		now := time.Now()
		c := newCreds(t, now.Add(-2*time.Hour), now.Add(-time.Minute))
		s := &stub{steps: []func(Request) (certificate.Result, error){fails(status.Error(codes.Unavailable, "x"))}}
		l := newLoop(c, s.renew)
		startLoop(t, l)
		l.NotifyConnected()
		time.Sleep(13 * time.Hour)
		synctest.Wait()
		if s.count() != 0 {
			t.Fatalf("an expired certificate sent %d requests", s.count())
		}
		if st := c.readState(t); st.LastResult != ResultExpired || st.Attempts != 0 {
			t.Fatalf("state = %+v, want expired", st)
		}
		errs := logs.lines(t, ExpiredMessage)
		if len(errs) != 1 || errs[0]["level"] != "ERROR" {
			t.Fatalf("expired lines = %v, want one ERROR line", errs)
		}
		if !strings.Contains(fmt.Sprint(errs[0]["not_after"]), "T") || strings.Contains(logs.text(), "runos certificate renew") {
			t.Fatalf("expired line = %v", errs[0])
		}
	})
}

// Criterion 7.
func TestLockBusyIsNotAFailure(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		captureLogs(t)
		c := dueCreds(t)
		s := &stub{steps: []func(Request) (certificate.Result, error){fails(certstore.ErrRenewalInProgress)}}
		l := newLoop(c, s.renew)
		startLoop(t, l)
		l.NotifyConnected()
		time.Sleep(settle + time.Second)
		synctest.Wait()
		st := c.readState(t)
		first := s.times()[0]
		if st.LastResult != ResultWaiting || st.ConsecutiveFailures != 0 || st.LastErrorCode != ReasonLockBusy ||
			st.NextAttemptAt != stamp(first.Add(lockBusyRetry)) {
			t.Fatalf("state after a busy lock = %+v", st)
		}
		time.Sleep(lockBusyRetry)
		synctest.Wait()
		if s.count() != 2 || s.times()[1].Sub(first) != lockBusyRetry {
			t.Fatalf("retry after a busy lock: %v", s.times())
		}
		if st := c.readState(t); st.ConsecutiveFailures != 0 {
			t.Fatalf("a busy lock counted as a failure: %+v", st)
		}
	})
}

// Criterion 7.
func TestLostResponseFlagReachesClassifier(t *testing.T) {
	lost := status.Error(codes.Unavailable, "connection reset by peer")
	already := status.Error(codes.FailedPrecondition, "certificate was already regenerated")
	t.Run("across attempts", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			captureLogs(t)
			c := dueCreds(t)
			s := &stub{steps: []func(Request) (certificate.Result, error){
				func(r Request) (certificate.Result, error) {
					if !r.LostResponse(lost) {
						t.Error("the predicate does not count a reset as a lost answer")
					}
					return certificate.Result{}, lost
				},
				fails(already),
			}}
			l := newLoop(c, s.renew)
			startLoop(t, l)
			l.NotifyConnected()
			time.Sleep(10 * time.Minute)
			synctest.Wait()
			if s.count() != 2 {
				t.Fatalf("requests = %d, want 2", s.count())
			}
			if st := c.readState(t); st.Outcome != certpolicy.LostResponse.String() {
				t.Fatalf("outcome = %s, want LostResponse", st.Outcome)
			}
		})
	})
	t.Run("serial change clears the flag", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			captureLogs(t)
			c := dueCreds(t)
			s := &stub{steps: []func(Request) (certificate.Result, error){
				func(r Request) (certificate.Result, error) { r.LostResponse(lost); return certificate.Result{}, lost },
				fails(already),
			}}
			l := newLoop(c, s.renew)
			startLoop(t, l)
			l.NotifyConnected()
			time.Sleep(settle + time.Second)
			synctest.Wait()
			now := time.Now()
			c.replaceLeaf(t, now.Add(-50*time.Minute), now.Add(10*time.Minute))
			time.Sleep(serialCheck + time.Second)
			synctest.Wait()
			if s.count() != 2 {
				t.Fatalf("requests = %d, want 2", s.count())
			}
			if st := c.readState(t); st.Outcome != certpolicy.TerminalForThisCertificate.String() {
				t.Fatalf("outcome = %s, want TerminalForThisCertificate", st.Outcome)
			}
		})
	})
	t.Run("inside one attempt", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			captureLogs(t)
			c := dueCreds(t)
			s := &stub{steps: []func(Request) (certificate.Result, error){
				func(r Request) (certificate.Result, error) {
					r.LostResponse(lost)
					return certificate.Result{}, already
				},
			}}
			l := newLoop(c, s.renew)
			startLoop(t, l)
			l.NotifyConnected()
			time.Sleep(settle + time.Second)
			synctest.Wait()
			if st := c.readState(t); st.Outcome != certpolicy.LostResponse.String() || st.LastResult != ResultRefused {
				t.Fatalf("state = %+v, want LostResponse", st)
			}
		})
	})
}

// Criterion 8.
func TestShutdownWaitsForAttempt(t *testing.T) {
	t.Run("an attempt ends", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			captureLogs(t)
			c := dueCreds(t)
			entered := make(chan struct{})
			var ctxErr error
			s := &stub{steps: []func(Request) (certificate.Result, error){func(Request) (certificate.Result, error) {
				close(entered)
				time.Sleep(5 * time.Second)
				return certificate.Result{Status: certificate.StatusRenewed, Serial: "child"}, nil
			}}}
			var seen context.Context
			l := newLoop(c, func(ctx context.Context, r Request) (certificate.Result, error) {
				seen = ctx
				res, err := s.renew(ctx, r)
				ctxErr = ctx.Err()
				return res, err
			})
			ctx, cancel := context.WithCancel(context.Background())
			l.Start(ctx)
			l.NotifyConnected()
			<-entered
			cancel()
			begin := time.Now()
			if !l.Wait(ShutdownWait) {
				t.Fatal("Wait did not see the loop end")
			}
			if d := time.Since(begin); d != 5*time.Second {
				t.Fatalf("Wait returned after %s, want the 5 s of the attempt", d)
			}
			if ctxErr != nil || seen == nil {
				t.Fatalf("the cancel reached the attempt: %v", ctxErr)
			}
			time.Sleep(time.Hour)
			if s.count() != 1 {
				t.Fatalf("attempts after the shutdown: %d", s.count())
			}
		})
	})
	t.Run("Wait stops at its limit", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			captureLogs(t)
			c := dueCreds(t)
			entered := make(chan struct{})
			l := newLoop(c, func(ctx context.Context, _ Request) (certificate.Result, error) {
				close(entered)
				<-ctx.Done()
				return certificate.Result{}, ctx.Err()
			})
			ctx, cancel := context.WithCancel(context.Background())
			l.Start(ctx)
			l.NotifyConnected()
			<-entered
			cancel()
			begin := time.Now()
			if l.Wait(ShutdownWait) {
				t.Fatal("Wait saw the loop end during an attempt")
			}
			if d := time.Since(begin); d != ShutdownWait {
				t.Fatalf("Wait returned after %s, want %s", d, ShutdownWait)
			}
			// The attempt ends at its own limit.
			if !l.Wait(attemptLimit) {
				t.Fatal("the attempt did not end at its limit")
			}
		})
	})
}

// Criterion 8.
func TestPanicRestartsLoop(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		logs := captureLogs(t)
		c := dueCreds(t)
		reads := 0
		var mu = make(chan struct{}, 1)
		mu <- struct{}{}
		l := newLoop(c, (&stub{steps: []func(Request) (certificate.Result, error){fails(status.Error(codes.Unavailable, "x"))}}).renew)
		l.d.ReadLeaf = func() (Leaf, error) {
			<-mu
			reads++
			n := reads
			mu <- struct{}{}
			if n == 1 {
				panic("test panic")
			}
			return ReadLeafFile(c.paths.Cert)
		}
		startLoop(t, l)
		synctest.Wait()
		lines := logs.lines(t, PanicMessage)
		if len(lines) != 1 || lines[0]["level"] != "ERROR" {
			t.Fatalf("panic lines = %v, want one ERROR line", lines)
		}
		time.Sleep(panicRestart - time.Second)
		synctest.Wait()
		<-mu
		if reads != 1 {
			t.Fatalf("the loop ran again %d times before 1 minute", reads-1)
		}
		mu <- struct{}{}
		time.Sleep(2 * time.Second)
		synctest.Wait()
		<-mu
		if reads < 2 {
			t.Fatal("the loop did not run again after 1 minute")
		}
		mu <- struct{}{}
		if st := c.readState(t); st.HeldSerial == "" {
			t.Fatal("the restarted loop wrote no state")
		}
	})
}

// TestExpiredCallbackReplacesTheLine: with OnExpired set, the loop writes no
// ERROR line of its own, tells the callback at expiry and at each held
// serial check, and still writes lastResult expired with no request.
func TestExpiredCallbackReplacesTheLine(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		logs := captureLogs(t)
		now := time.Now()
		c := newCreds(t, now.Add(-2*time.Hour), now.Add(time.Minute))
		s := &stub{steps: []func(Request) (certificate.Result, error){fails(status.Error(codes.Unavailable, "x"))}}
		var mu sync.Mutex
		var calls []time.Time
		l := New(Deps{
			Renew:     s.renew,
			ReadLeaf:  func() (Leaf, error) { return ReadLeafFile(c.paths.Cert) },
			NodeID:    testNodeID,
			Rand:      func() float64 { return 0 },
			StatePath: c.state,
			PID:       4242,
			OnExpired: func(Leaf) {
				mu.Lock()
				calls = append(calls, time.Now())
				mu.Unlock()
			},
		})
		startLoop(t, l)
		// No stream: the attempt waits for one, and the leaf expires first
		// when no request goes out.
		time.Sleep(time.Minute + time.Second)
		synctest.Wait()
		mu.Lock()
		first := len(calls)
		mu.Unlock()
		if first != 1 {
			t.Fatalf("callback calls at expiry = %d, want 1", first)
		}
		time.Sleep(10 * time.Minute)
		synctest.Wait()
		mu.Lock()
		got := len(calls)
		mu.Unlock()
		if got < 20 {
			t.Fatalf("callback calls after 10 minutes = %d, want one per 30 s check", got)
		}
		if n := len(logs.lines(t, ExpiredMessage)); n != 0 {
			t.Fatalf("the loop wrote %d expired lines of its own", n)
		}
		if st := c.readState(t); st.LastResult != ResultExpired {
			t.Fatalf("state = %+v, want expired", st)
		}
		if s.count() != 0 {
			t.Fatalf("an expired certificate sent %d requests", s.count())
		}
	})
}
