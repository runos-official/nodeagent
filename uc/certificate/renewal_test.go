package certificate

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/runos-official/nodeagent/l2sec"
	"github.com/runos-official/nodeagent/roslog"
	"github.com/runos-official/nodeagent/uc/certstore"
	"github.com/runos-official/nodeagent/uc/csrcert/csrtest"
)

// env records what renewOnce did outside the certificate directory.
type env struct {
	mu     sync.Mutex
	offset time.Duration
	sleeps []time.Duration
	logs   []string
	logErr error
	dials  int
}

func (e *env) now() time.Time {
	e.mu.Lock()
	defer e.mu.Unlock()
	return time.Now().Add(e.offset)
}

func (e *env) advance(d time.Duration) {
	e.mu.Lock()
	e.offset += d
	e.mu.Unlock()
}

// newDeps gives test dependencies: the fake Nodeward c, a clock that Sleep
// advances without sleeping, and a recorded node log.
func newDeps(p certstore.Paths, c mintClient) (deps, *env) {
	e := &env{}
	return deps{
		Paths: p,
		Dial: func(context.Context) (mintClient, func(), error) {
			e.mu.Lock()
			e.dials++
			e.mu.Unlock()
			return c, func() {}, nil
		},
		NodeLog: func(event, _ string) error {
			e.mu.Lock()
			defer e.mu.Unlock()
			e.logs = append(e.logs, event)
			return e.logErr
		},
		Event:   "CertificateRenewal",
		Message: "Certificate renewed successfully",
		Now:     e.now,
		Sleep: func(_ context.Context, d time.Duration) error {
			e.mu.Lock()
			e.sleeps = append(e.sleeps, d)
			e.offset += d
			e.mu.Unlock()
			return nil
		},
		LockWait:     2 * time.Second,
		LostResponse: MayBeLost,
	}, e
}

// noMint is a fake Nodeward that fails the test on any call.
func noMint(t *testing.T) *fakeL2 {
	return &fakeL2{respond: func(*l2sec.RegenerateCertificateRequest) (*l2sec.RegenerateCertificateResponse, error) {
		t.Error("unexpected mint call")
		return nil, status.Error(codes.Internal, "unexpected")
	}}
}

// childFor makes a child key pair signed by ca, shaped by opts.
func childFor(t *testing.T, ca *csrtest.CA, opts csrtest.LeafOptions) certstore.Material {
	t.Helper()
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(k)
	if err != nil {
		t.Fatal(err)
	}
	return certstore.Material{
		KeyPEM:  pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}),
		CertPEM: []byte(ca.LeafWith(t, k.Public(), opts)),
		CAPEM:   []byte(ca.PEM),
	}
}

func serialOf(t *testing.T, certPEM []byte) string {
	t.Helper()
	b, _ := pem.Decode(certPEM)
	if b == nil {
		t.Fatal("no PEM block")
	}
	c, err := x509.ParseCertificate(b.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	return fmt.Sprintf("%x", c.SerialNumber)
}

func readLive(t *testing.T, p certstore.Paths) certstore.Material {
	t.Helper()
	read := func(path string) []byte {
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	return certstore.Material{KeyPEM: read(p.Key), CertPEM: read(p.Cert), CAPEM: read(p.CA)}
}

func sameMaterial(a, b certstore.Material) bool {
	return bytes.Equal(a.KeyPEM, b.KeyPEM) && bytes.Equal(a.CertPEM, b.CertPEM) && bytes.Equal(a.CAPEM, b.CAPEM)
}

// strayFiles lists every staged, previous or temporary file in dir.
func strayFiles(t *testing.T, dir string) []string {
	t.Helper()
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range ents {
		n := e.Name()
		if strings.HasSuffix(n, ".next") || strings.HasSuffix(n, ".prev") || strings.HasSuffix(n, ".tmp") {
			out = append(out, n)
		}
	}
	return out
}

// crashStore leaves p at the crash point of an adoption of child.
func crashStore(t *testing.T, p certstore.Paths, point string, child certstore.Material) {
	t.Helper()
	s := certstore.Open(p)
	stop := errors.New("test crash")
	s.SetCrashHook(func(at string) error {
		if at == point {
			return stop
		}
		return nil
	})
	if err := s.Prepare(); err != nil {
		t.Fatal(err)
	}
	if err := s.Adopt(child); !errors.Is(err, stop) {
		t.Fatalf("adopt did not stop at %s: %v", point, err)
	}
}

// pointsBetween returns the crash points from first to last, in list order.
func pointsBetween(t *testing.T, first, last string) []string {
	t.Helper()
	i, j := slices.Index(certstore.CrashPoints, first), slices.Index(certstore.CrashPoints, last)
	if i < 0 || j < i {
		t.Fatalf("crash points %s..%s are not in the list %v", first, last, certstore.CrashPoints)
	}
	return certstore.CrashPoints[i : j+1]
}

func captureLogs(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	old := roslog.L
	roslog.L = slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	t.Cleanup(func() { roslog.L = old })
	return &buf
}

func TestRenewAfterRollForwardDoesNotMint(t *testing.T) {
	for _, point := range pointsBetween(t, "committed", "dir-synced") {
		t.Run(point, func(t *testing.T) {
			ca := csrtest.NewCA(t)
			p := currentCreds(t, ca)
			child := childFor(t, ca, csrtest.LeafOptions{})
			crashStore(t, p, point, child)

			f := noMint(t)
			d, _ := newDeps(p, f)
			res, err := renewOnce(context.Background(), d)
			if err != nil {
				t.Fatal(err)
			}
			if res.Status != StatusAlreadyRenewed || res.Serial != serialOf(t, child.CertPEM) {
				t.Fatalf("got %+v, want already_renewed with the child serial", res)
			}
			if f.callCount() != 0 {
				t.Fatalf("mint calls = %d", f.callCount())
			}
			if !sameMaterial(readLive(t, p), child) {
				t.Fatal("live files are not the child")
			}
		})
	}

	t.Run("superseded", func(t *testing.T) {
		ca := csrtest.NewCA(t)
		p := currentCreds(t, ca)
		crashStore(t, p, "committed", childFor(t, ca, csrtest.LeafOptions{}))
		// A new registration replaced the live files.
		foreign := childFor(t, ca, csrtest.LeafOptions{})
		for _, w := range []struct {
			path string
			data []byte
		}{{p.CA, foreign.CAPEM}, {p.Key, foreign.KeyPEM}, {p.Cert, foreign.CertPEM}} {
			if err := os.WriteFile(w.path, w.data, 0o600); err != nil {
				t.Fatal(err)
			}
		}
		f := noMint(t)
		d, _ := newDeps(p, f)
		res, err := renewOnce(context.Background(), d)
		if err != nil {
			t.Fatal(err)
		}
		if res.Status != StatusAlreadyRenewed || res.Serial != serialOf(t, foreign.CertPEM) {
			t.Fatalf("got %+v, want already_renewed with the foreign serial", res)
		}
		if f.callCount() != 0 || !sameMaterial(readLive(t, p), foreign) {
			t.Fatal("a superseded store minted or changed the live files")
		}
	})
}

func TestExpectedSerialMismatchDoesNotMint(t *testing.T) {
	ca := csrtest.NewCA(t)
	p := currentCreds(t, ca)
	parentSerial := serialOf(t, readLive(t, p).CertPEM)

	// Another process finishes a whole adoption after the caller decided.
	f := &fakeL2{respond: csrNodeward(t, ca, p)}
	if _, err := renewVia(t, f, p); err != nil {
		t.Fatal(err)
	}
	f.respond = noMint(t).respond

	d, _ := newDeps(p, f)
	d.ExpectedSerial = parentSerial
	res, err := renewOnce(context.Background(), d)
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != StatusAlreadyRenewed || f.callCount() != 1 {
		t.Fatalf("got %+v after %d calls, want already_renewed and one mint", res, f.callCount())
	}
}

func TestTwoRenewalsOneMint(t *testing.T) {
	ca := csrtest.NewCA(t)
	p := currentCreds(t, ca)
	parentSerial := serialOf(t, readLive(t, p).CertPEM)
	sign := csrNodeward(t, ca, p)
	inMint := make(chan struct{})
	proceed := make(chan struct{})
	var once sync.Once
	f := &fakeL2{respond: func(in *l2sec.RegenerateCertificateRequest) (*l2sec.RegenerateCertificateResponse, error) {
		once.Do(func() { close(inMint) })
		<-proceed
		return sign(in)
	}}

	results := make([]Result, 2)
	errs := make([]error, 2)
	var wg sync.WaitGroup
	run := func(i int) {
		defer wg.Done()
		d, _ := newDeps(p, f)
		d.ExpectedSerial = parentSerial
		d.LockWait = 30 * time.Second
		results[i], errs[i] = renewOnce(context.Background(), d)
	}
	wg.Add(2)
	go run(0)
	<-inMint // the first holds the lock and is inside the mint
	go run(1)
	close(proceed)
	wg.Wait()

	for _, err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	got := []string{results[0].Status, results[1].Status}
	slices.Sort(got)
	if !slices.Equal(got, []string{StatusAlreadyRenewed, StatusRenewed}) || f.callCount() != 1 {
		t.Fatalf("statuses %v after %d mint calls, want one renewed, one already_renewed, one mint", got, f.callCount())
	}

	t.Run("lock held past the wait", func(t *testing.T) {
		release, err := certstore.AcquireRenewalLock(context.Background(), filepath.Dir(p.Cert), time.Second)
		if err != nil {
			t.Fatal(err)
		}
		defer release()
		g := noMint(t)
		d, _ := newDeps(p, g)
		d.LockWait = 300 * time.Millisecond
		_, err = renewOnce(context.Background(), d)
		if !errors.Is(err, certstore.ErrRenewalInProgress) || err.Error() != "renewal already in progress" {
			t.Fatalf("want renewal already in progress, got %v", err)
		}
		if g.callCount() != 0 {
			t.Fatal("a blocked renewal minted")
		}
	})
}

// csrKey returns the DER public key of the CSR of a request.
func csrKey(t *testing.T, in *l2sec.RegenerateCertificateRequest) string {
	t.Helper()
	b, _ := pem.Decode([]byte(in.CsrPem))
	csr, err := x509.ParseCertificateRequest(b.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKIXPublicKey(csr.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	return string(der)
}

func TestLostResponseRetriesWithFreshKey(t *testing.T) {
	lost := status.Error(codes.Unavailable, "connection reset by peer")
	busy := status.Error(codes.ResourceExhausted, "gateway is busy with an earlier request")

	// script answers call n (1-based) with the result of steps[n-1]: "mint"
	// signs and returns the child, "lost" signs and returns lost, any
	// other value is returned as the error. Every mint is recorded.
	type step struct {
		mint bool
		err  error
		resp func(t *testing.T, ca *csrtest.CA) *l2sec.RegenerateCertificateResponse
	}
	cases := []struct {
		name       string
		steps      []step
		wantCalls  int
		wantErr    codes.Code
		wantSleeps []time.Duration
	}{
		{name: "mint then lost answer", steps: []step{{mint: true, err: lost}, {mint: true}}, wantCalls: 2},
		{name: "unusable response", steps: []step{{resp: func(t *testing.T, ca *csrtest.CA) *l2sec.RegenerateCertificateResponse {
			_, foreign := ca.LegacyPair(t)
			return &l2sec.RegenerateCertificateResponse{CertificatePem: foreign, CaCert: ca.PEM}
		}}, {mint: true}}, wantCalls: 2},
		{name: "pre-mint refusal under Unavailable", steps: []step{
			{err: status.Error(codes.Unavailable, "could not check the minting rules: database unavailable")},
		}, wantCalls: 1, wantErr: codes.Unavailable},
		{name: "busy on the retry", steps: []step{{mint: true, err: lost}, {err: busy}, {mint: true}},
			wantCalls: 3, wantSleeps: []time.Duration{busyWait}},
		{name: "busy twice stops at three", steps: []step{{mint: true, err: lost}, {err: busy}, {err: busy}},
			wantCalls: 3, wantErr: codes.ResourceExhausted, wantSleeps: []time.Duration{busyWait}},
		{name: "two lost answers stop", steps: []step{{mint: true, err: lost}, {mint: true, err: lost}},
			wantCalls: 2, wantErr: codes.Unavailable},
		{name: "busy first is returned", steps: []step{{err: busy}}, wantCalls: 1, wantErr: codes.ResourceExhausted},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ca := csrtest.NewCA(t)
			p := currentCreds(t, ca)
			parent := readLive(t, p)
			sign := csrNodeward(t, ca, p)
			var lastLeaf string
			f := &fakeL2{}
			f.respond = func(in *l2sec.RegenerateCertificateRequest) (*l2sec.RegenerateCertificateResponse, error) {
				n := f.callCount()
				if n > len(tc.steps) {
					t.Fatalf("call %d is past the script", n)
				}
				s := tc.steps[n-1]
				if s.resp != nil {
					return s.resp(t, ca), nil
				}
				if s.mint {
					r, err := sign(in)
					if err != nil {
						return nil, err
					}
					lastLeaf = r.CertificatePem
					if s.err == nil {
						return r, nil
					}
				}
				return nil, s.err
			}
			d, e := newDeps(p, f)
			res, err := renewOnce(context.Background(), d)

			if f.callCount() != tc.wantCalls {
				t.Fatalf("calls = %d, want %d", f.callCount(), tc.wantCalls)
			}
			if f.callCount() > maxMintAttempts {
				t.Fatal("more than 3 mint attempts")
			}
			keys := map[string]bool{}
			for _, c := range f.calls {
				keys[csrKey(t, c)] = true
			}
			if len(keys) != len(f.calls) {
				t.Fatal("an attempt reused the CSR key of another attempt")
			}
			if !slices.Equal(e.sleeps, tc.wantSleeps) {
				t.Fatalf("sleeps = %v, want %v", e.sleeps, tc.wantSleeps)
			}
			if e.dials != 1 {
				t.Fatalf("dials = %d, want one connection", e.dials)
			}
			if tc.wantErr != codes.OK {
				if status.Code(err) != tc.wantErr {
					t.Fatalf("want the refusal %v unchanged, got %v", tc.wantErr, err)
				}
				if !sameMaterial(readLive(t, p), parent) {
					t.Fatal("a failed renewal changed the live files")
				}
				if left := strayFiles(t, filepath.Dir(p.Cert)); len(left) != 0 {
					t.Fatalf("a failed renewal left files: %v", left)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if res.Status != StatusRenewed || string(readLive(t, p).CertPEM) != lastLeaf {
				t.Fatal("the last child is not the live certificate")
			}
		})
	}

	t.Run("no third call when the budget is spent", func(t *testing.T) {
		ca := csrtest.NewCA(t)
		p := currentCreds(t, ca)
		var e *env
		f := &fakeL2{}
		f.respond = func(*l2sec.RegenerateCertificateRequest) (*l2sec.RegenerateCertificateResponse, error) {
			if f.callCount() == 1 {
				e.advance(mintBudget - 2*time.Second)
				return nil, lost
			}
			return nil, busy
		}
		var d deps
		d, e = newDeps(p, f)
		if _, err := renewOnce(context.Background(), d); status.Code(err) != codes.ResourceExhausted {
			t.Fatalf("want the busy refusal, got %v", err)
		}
		if f.callCount() != 2 || len(e.sleeps) != 0 {
			t.Fatalf("calls = %d, sleeps = %v; want 2 calls and no wait past the budget", f.callCount(), e.sleeps)
		}
	})
}

func TestClockSkewInsideOneAttempt(t *testing.T) {
	ca := csrtest.NewCA(t)
	p := currentCreds(t, ca)
	sign := csrNodeward(t, ca, p)
	f := &fakeL2{}
	f.respond = func(in *l2sec.RegenerateCertificateRequest) (*l2sec.RegenerateCertificateResponse, error) {
		switch f.callCount() {
		case 1:
			return nil, status.Error(codes.FailedPrecondition,
				fmt.Sprintf("mint proof time is outside the 5 minute window; server time %d", time.Now().Unix()))
		case 2:
			if _, err := sign(in); err != nil {
				return nil, err
			}
			return nil, status.Error(codes.Unavailable, "connection reset by peer")
		case 3:
			return nil, status.Error(codes.ResourceExhausted, "gateway is busy with an earlier request")
		}
		return sign(in)
	}
	d, e := newDeps(p, f)
	start := e.now()
	res, err := renewOnce(context.Background(), d)
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != StatusRenewed || f.callCount() != 4 {
		t.Fatalf("got %+v after %d wire calls, want renewed after 4", res, f.callCount())
	}
	if f.calls[0].CsrPem != f.calls[1].CsrPem {
		t.Fatal("the clock-skew correction changed the CSR of its attempt")
	}
	keys := map[string]bool{}
	for _, c := range f.calls {
		keys[csrKey(t, c)] = true
	}
	if len(keys) != 3 {
		t.Fatalf("distinct CSR keys = %d, want 3 attempts", len(keys))
	}
	if elapsed := e.now().Sub(start); elapsed >= mintBudget {
		t.Fatalf("the sequence took %v of the clock", elapsed)
	}
}

func TestLogFailureAfterMintKeepsChild(t *testing.T) {
	logs := captureLogs(t)
	ca := csrtest.NewCA(t)
	p := currentCreds(t, ca)
	f := &fakeL2{respond: csrNodeward(t, ca, p)}
	d, e := newDeps(p, f)
	e.logErr = errors.New("nodeward unreachable")
	res, err := renewOnce(context.Background(), d)
	if err != nil {
		t.Fatalf("a failed node log failed the renewal: %v", err)
	}
	if res.Status != StatusRenewed || !res.NodeLogFailed {
		t.Fatalf("got %+v", res)
	}
	if serialOf(t, readLive(t, p).CertPEM) != res.Serial || res.Serial == "" {
		t.Fatal("the child is not live")
	}
	if n := strings.Count(logs.String(), "Could not send the certificate renewal to the node log"); n != 1 {
		t.Fatalf("warnings = %d, want 1", n)
	}
}

func TestChildRejectedEndsRenewal(t *testing.T) {
	ca := csrtest.NewCA(t)
	p := currentCreds(t, ca)
	// A v2 parent of account a.
	parent := childFor(t, ca, csrtest.LeafOptions{DNSNames: []string{"acct-a.example.test"}, Version: 2, Type: 1})
	for _, w := range []struct {
		path string
		data []byte
	}{{p.CA, parent.CAPEM}, {p.Key, parent.KeyPEM}, {p.Cert, parent.CertPEM}} {
		if err := os.WriteFile(w.path, w.data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// Nodeward answers with a child of another account.
	f := &fakeL2{respond: func(in *l2sec.RegenerateCertificateRequest) (*l2sec.RegenerateCertificateResponse, error) {
		b, _ := pem.Decode([]byte(in.CsrPem))
		csr, err := x509.ParseCertificateRequest(b.Bytes)
		if err != nil {
			t.Fatal(err)
		}
		leaf := ca.LeafWith(t, csr.PublicKey, csrtest.LeafOptions{DNSNames: []string{"acct-b.example.test"}, Version: 2, Type: 1})
		return &l2sec.RegenerateCertificateResponse{CertificatePem: leaf, CaCert: ca.PEM}, nil
	}}
	d, _ := newDeps(p, f)
	_, err := renewOnce(context.Background(), d)
	if !errors.Is(err, certstore.ErrChildRejected) || certstore.RejectReason(err) != certstore.ReasonAccountMismatch {
		t.Fatalf("want ErrChildRejected account_mismatch, got %v", err)
	}
	if f.callCount() != 1 {
		t.Fatalf("calls = %d, want no second mint", f.callCount())
	}
	if !sameMaterial(readLive(t, p), parent) {
		t.Fatal("the parent changed")
	}
	if left := strayFiles(t, filepath.Dir(p.Cert)); len(left) != 0 {
		t.Fatalf("a rejected child left files: %v", left)
	}
}

// crashed is the panic value that stops renewOnce like a power loss.
type crashed struct{ point string }

// runUntilCrash runs renewOnce and recovers the crash panic.
func runUntilCrash(t *testing.T, d deps) {
	t.Helper()
	defer func() {
		r := recover()
		if _, ok := r.(crashed); !ok {
			t.Fatalf("renewOnce did not stop at the crash: %v", r)
		}
	}()
	_, err := renewOnce(context.Background(), d)
	t.Fatalf("renewOnce returned %v instead of stopping", err)
}

func TestCrashBetweenMintAndAdopt(t *testing.T) {
	for _, point := range []string{"prepared", "mint"} {
		t.Run("stop after "+point, func(t *testing.T) {
			ca := csrtest.NewCA(t)
			p := currentCreds(t, ca)
			parent := readLive(t, p)
			sign := csrNodeward(t, ca, p)
			f := &fakeL2{respond: func(in *l2sec.RegenerateCertificateRequest) (*l2sec.RegenerateCertificateResponse, error) {
				if _, err := sign(in); err != nil {
					return nil, err
				}
				panic(crashed{"mint"})
			}}
			d, _ := newDeps(p, f)
			d.storeHook = func(at string) error {
				if at == point {
					panic(crashed{at})
				}
				return nil
			}
			runUntilCrash(t, d)
			if err := certstore.Open(p).Recover(); err != nil {
				t.Fatal(err)
			}
			if !sameMaterial(readLive(t, p), parent) {
				t.Fatal("the parent is not live after the next Recover")
			}
			if left := strayFiles(t, filepath.Dir(p.Cert)); len(left) != 0 {
				t.Fatalf("files left after the next Recover: %v", left)
			}
		})
	}

	for _, point := range pointsBetween(t, "staged-cert", "checked") {
		t.Run("stop at "+point, func(t *testing.T) {
			ca := csrtest.NewCA(t)
			p := currentCreds(t, ca)
			parentSerial := serialOf(t, readLive(t, p).CertPEM)
			var leaf string
			sign := csrNodeward(t, ca, p)
			f := &fakeL2{respond: func(in *l2sec.RegenerateCertificateRequest) (*l2sec.RegenerateCertificateResponse, error) {
				r, err := sign(in)
				if err == nil {
					leaf = r.CertificatePem
				}
				return r, err
			}}
			d, _ := newDeps(p, f)
			d.storeHook = func(at string) error {
				if at == point {
					panic(crashed{at})
				}
				return nil
			}
			runUntilCrash(t, d)

			// The next start recovers, then the caller that evaluated the
			// parent asks again.
			recoverAtStart(context.Background(), p, time.Second)
			if string(readLive(t, p).CertPEM) != leaf {
				t.Fatal("the child is not live after the next start")
			}
			d, _ = newDeps(p, f)
			d.ExpectedSerial = parentSerial
			res, err := renewOnce(context.Background(), d)
			if err != nil {
				t.Fatal(err)
			}
			if res.Status != StatusAlreadyRenewed || f.callCount() != 1 {
				t.Fatalf("got %+v after %d calls, want already_renewed and no second mint", res, f.callCount())
			}
		})
	}
}

func TestFailedMintLeavesNoParentCopy(t *testing.T) {
	ca := csrtest.NewCA(t)
	p := currentCreds(t, ca)
	f := &fakeL2{respond: func(*l2sec.RegenerateCertificateRequest) (*l2sec.RegenerateCertificateResponse, error) {
		return nil, status.Error(codes.FailedPrecondition, "renewal is not open yet")
	}}
	d, _ := newDeps(p, f)
	if _, err := renewOnce(context.Background(), d); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("want the refusal unchanged, got %v", err)
	}
	if left := strayFiles(t, filepath.Dir(p.Cert)); len(left) != 0 {
		t.Fatalf("a refused mint left files: %v", left)
	}

	t.Run("dial failure", func(t *testing.T) {
		d, _ := newDeps(p, noMint(t))
		d.Dial = func(context.Context) (mintClient, func(), error) { return nil, nil, errors.New("no route to host") }
		_, err := renewOnce(context.Background(), d)
		var de *dialError
		if !errors.As(err, &de) {
			t.Fatalf("want a dial error, got %v", err)
		}
		if left := strayFiles(t, filepath.Dir(p.Cert)); len(left) != 0 {
			t.Fatalf("a failed dial left files: %v", left)
		}
	})
}

func TestDamagedStoreEndsRenewal(t *testing.T) {
	ca := csrtest.NewCA(t)
	p := currentCreds(t, ca)
	journal := filepath.Join(filepath.Dir(p.Cert), certstore.JournalName)
	if err := os.WriteFile(journal, []byte(`{"state":"unknown"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	f := noMint(t)
	d, _ := newDeps(p, f)
	_, err := renewOnce(context.Background(), d)
	if !errors.Is(err, certstore.ErrStoreDamaged) || !isStoreError(err) {
		t.Fatalf("want ErrStoreDamaged, got %v", err)
	}
	if f.callCount() != 0 {
		t.Fatal("a damaged store minted")
	}
}

func TestMayBeLost(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want bool
	}{
		{status.Error(codes.Unavailable, "connection reset"), true},
		{status.Error(codes.DeadlineExceeded, "deadline"), true},
		{status.Error(codes.Canceled, "canceled"), true},
		{status.Error(codes.Internal, "stream terminated"), true},
		{status.Error(codes.Unknown, "eof"), true},
		{status.Error(codes.Unavailable, "could not check the minting rules: x"), false},
		{status.Error(codes.Unavailable, "could not check the mint proof: x"), false},
		{status.Error(codes.Unavailable, "authentication check failed: x"), false},
		{status.Error(codes.ResourceExhausted, "busy"), false},
		{status.Error(codes.FailedPrecondition, "renewal is not open"), false},
		{errors.New("load current certificate and key"), false},
		{nil, false},
	} {
		if got := MayBeLost(tc.err); got != tc.want {
			t.Errorf("MayBeLost(%v) = %v, want %v", tc.err, got, tc.want)
		}
	}
}

func TestRecoverAtStartQuiet(t *testing.T) {
	logs := captureLogs(t)
	empty := t.TempDir()
	p := certstore.Paths{Cert: filepath.Join(empty, "mtls.crt"), Key: filepath.Join(empty, "mtls.key"), CA: filepath.Join(empty, "ca.crt")}
	recoverAtStart(context.Background(), p, time.Second)
	if ents, _ := os.ReadDir(empty); len(ents) != 0 {
		t.Fatalf("recovery created files on an unregistered node: %v", ents)
	}
	missing := filepath.Join(t.TempDir(), "absent")
	recoverAtStart(context.Background(), certstore.Paths{Cert: filepath.Join(missing, "mtls.crt")}, time.Second)
	if _, err := os.Stat(missing); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("recovery created the certificate directory")
	}
	if logs.Len() != 0 {
		t.Fatalf("recovery logged on an unregistered node: %s", logs.String())
	}

	t.Run("held lock warns and returns", func(t *testing.T) {
		ca := csrtest.NewCA(t)
		p := currentCreds(t, ca)
		release, err := certstore.AcquireRenewalLock(context.Background(), filepath.Dir(p.Cert), time.Second)
		if err != nil {
			t.Fatal(err)
		}
		defer release()
		logs.Reset()
		recoverAtStart(context.Background(), p, 300*time.Millisecond)
		if n := strings.Count(logs.String(), "\n"); n != 1 || !strings.Contains(logs.String(), "renewal already in progress") {
			t.Fatalf("want one warning line, got %s", logs.String())
		}
	})
}

// TestManualRenewOutput checks the output contract of the manual command.
func TestManualRenewOutput(t *testing.T) {
	jsonRun := func(t *testing.T, d deps) map[string]any {
		t.Helper()
		var out bytes.Buffer
		if err := renewCertificate(true, d, &out); err != nil {
			t.Fatal(err)
		}
		var got map[string]any
		if err := json.Unmarshal(out.Bytes(), &got); err != nil {
			t.Fatalf("not one JSON object: %v: %s", err, out.String())
		}
		if len(got) != 3 {
			t.Fatalf("members = %v, want status, notAfter, restartRequired", got)
		}
		if _, ok := got["notAfter"].(string); !ok {
			t.Fatalf("notAfter is not a string: %v", got)
		}
		if _, err := time.Parse(time.RFC3339, got["notAfter"].(string)); err != nil {
			t.Fatalf("notAfter is not RFC 3339: %v", got)
		}
		if r, ok := got["restartRequired"].(bool); !ok || !r {
			t.Fatalf("restartRequired is not true: %v", got)
		}
		return got
	}

	t.Run("renewed", func(t *testing.T) {
		ca := csrtest.NewCA(t)
		p := currentCreds(t, ca)
		d, _ := newDeps(p, &fakeL2{respond: csrNodeward(t, ca, p)})
		if got := jsonRun(t, d); got["status"] != StatusRenewed {
			t.Fatalf("status = %v", got["status"])
		}
	})

	t.Run("already_renewed", func(t *testing.T) {
		ca := csrtest.NewCA(t)
		p := currentCreds(t, ca)
		crashStore(t, p, "committed", childFor(t, ca, csrtest.LeafOptions{}))
		d, _ := newDeps(p, noMint(t))
		if got := jsonRun(t, d); got["status"] != StatusAlreadyRenewed {
			t.Fatalf("status = %v", got["status"])
		}
	})

	forbidden := func(t *testing.T, text string) {
		t.Helper()
		low := strings.ToLower(text)
		for _, w := range []string{"back up", "backup", "restor"} {
			if strings.Contains(low, w) {
				t.Fatalf("output names %q: %s", w, text)
			}
		}
	}

	t.Run("human output", func(t *testing.T) {
		ca := csrtest.NewCA(t)
		p := currentCreds(t, ca)
		d, e := newDeps(p, &fakeL2{respond: csrNodeward(t, ca, p)})
		e.logErr = errors.New("nodeward unreachable")
		var out bytes.Buffer
		if err := renewCertificate(false, d, &out); err != nil {
			t.Fatal(err)
		}
		text := out.String()
		forbidden(t, text)
		for _, want := range []string{
			"→ Requesting new certificate from Nodeward...",
			"→ Checking and adopting the new certificate...",
			"Could not record the renewal in the node log",
			"sudo systemctl restart runos.service",
			"You have a 5-minute grace period",
		} {
			if !strings.Contains(text, want) {
				t.Fatalf("output lacks %q:\n%s", want, text)
			}
		}
	})

	failRun := func(t *testing.T, d deps) (string, string) {
		t.Helper()
		logs := captureLogs(t)
		var out bytes.Buffer
		if err := renewCertificate(false, d, &out); !roslog.IsAlreadyReported(err) {
			t.Fatalf("want a reported failure, got %v", err)
		}
		forbidden(t, out.String()+logs.String())
		return out.String(), logs.String()
	}

	t.Run("rejected child", func(t *testing.T) {
		ca := csrtest.NewCA(t)
		p := currentCreds(t, ca)
		f := &fakeL2{respond: func(in *l2sec.RegenerateCertificateRequest) (*l2sec.RegenerateCertificateResponse, error) {
			b, _ := pem.Decode([]byte(in.CsrPem))
			csr, _ := x509.ParseCertificateRequest(b.Bytes)
			// A version extension other than 2.
			return &l2sec.RegenerateCertificateResponse{
				CertificatePem: ca.LeafWith(t, csr.PublicKey, csrtest.LeafOptions{Version: 3}), CaCert: ca.PEM}, nil
		}}
		d, _ := newDeps(p, f)
		_, logs := failRun(t, d)
		for _, want := range []string{certstore.ReasonVersionMismatch, "the current certificate is unchanged"} {
			if !strings.Contains(logs, want) {
				t.Fatalf("failure lacks %q: %s", want, logs)
			}
		}
	})

	t.Run("lock held", func(t *testing.T) {
		ca := csrtest.NewCA(t)
		p := currentCreds(t, ca)
		release, err := certstore.AcquireRenewalLock(context.Background(), filepath.Dir(p.Cert), time.Second)
		if err != nil {
			t.Fatal(err)
		}
		defer release()
		d, _ := newDeps(p, noMint(t))
		d.LockWait = 300 * time.Millisecond
		out, logs := failRun(t, d)
		if !strings.Contains(out, "→ Waiting for another certificate renewal to end...") {
			t.Fatalf("no lock wait line: %s", out)
		}
		for _, want := range []string{"renewal already in progress", "wait for the other renewal to end, then run the command again"} {
			if !strings.Contains(logs, want) {
				t.Fatalf("failure lacks %q: %s", want, logs)
			}
		}
	})
}

// TestCancelledContextDoesNotMint: a caller whose context ended while it
// waited for the lock gets no mint, and the lock is released.
func TestCancelledContextDoesNotMint(t *testing.T) {
	ca := csrtest.NewCA(t)
	p := currentCreds(t, ca)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	f := noMint(t)
	d, _ := newDeps(p, f)
	if _, err := renewOnce(ctx, d); !errors.Is(err, context.Canceled) {
		t.Fatalf("want context.Canceled, got %v", err)
	}
	if f.callCount() != 0 {
		t.Fatal("a cancelled caller minted")
	}
	if left := strayFiles(t, filepath.Dir(p.Cert)); len(left) != 0 {
		t.Fatalf("a cancelled caller left files: %v", left)
	}
	release, err := certstore.AcquireRenewalLock(context.Background(), filepath.Dir(p.Cert), 300*time.Millisecond)
	if err != nil {
		t.Fatalf("the lock stayed held: %v", err)
	}
	release()
}
