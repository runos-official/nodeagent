package certrenew

import (
	"bytes"
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/runos-official/nodeagent/l2sec"
	"github.com/runos-official/nodeagent/roslog"
	"github.com/runos-official/nodeagent/uc/certificate"
	"github.com/runos-official/nodeagent/uc/certstore"
	"github.com/runos-official/nodeagent/uc/csrcert"
	"github.com/runos-official/nodeagent/uc/csrcert/csrtest"
)

// testNodeID is a made-up node id; it only sets the jitter.
const testNodeID = "node-under-test"

// leafShape is the v2 shape of every leaf in these tests.
var leafShape = csrtest.LeafOptions{DNSNames: []string{"acct-a.example.test"}, Version: 2, Type: 1}

// creds is a certificate directory with a held leaf signed by ca.
type creds struct {
	ca    *csrtest.CA
	paths certstore.Paths
	state string
}

// newCreds writes a held ECDSA key and a v2 leaf valid from notBefore to
// notAfter. Call it inside the synctest bubble so every time is bubble
// time.
func newCreds(t *testing.T, notBefore, notAfter time.Time) *creds {
	t.Helper()
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(k)
	if err != nil {
		t.Fatal(err)
	}
	return writeCreds(t, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), k.Public(), notBefore, notAfter)
}

func writeCreds(t *testing.T, keyPEM []byte, pub crypto.PublicKey, notBefore, notAfter time.Time) *creds {
	t.Helper()
	ca := csrtest.NewCA(t)
	dir := t.TempDir()
	c := &creds{
		ca: ca,
		paths: certstore.Paths{
			Cert: filepath.Join(dir, "mtls.crt"),
			Key:  filepath.Join(dir, "mtls.key"),
			CA:   filepath.Join(dir, "ca.crt"),
		},
		state: filepath.Join(t.TempDir(), "run", "certificate-renewal.json"),
	}
	opts := leafShape
	opts.NotBefore, opts.NotAfter = notBefore, notAfter
	mustWrite(t, c.paths.Key, keyPEM, 0o600)
	mustWrite(t, c.paths.Cert, []byte(ca.LeafWith(t, pub, opts)), 0o644)
	mustWrite(t, c.paths.CA, []byte(ca.PEM), 0o644)
	return c
}

func mustWrite(t *testing.T, path string, b []byte, mode os.FileMode) {
	t.Helper()
	if err := os.WriteFile(path, b, mode); err != nil {
		t.Fatal(err)
	}
}

// replaceLeaf writes a new leaf for a fresh key (an operator re-issue).
func (c *creds) replaceLeaf(t *testing.T, notBefore, notAfter time.Time) {
	t.Helper()
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, _ := x509.MarshalPKCS8PrivateKey(k)
	opts := leafShape
	opts.NotBefore, opts.NotAfter = notBefore, notAfter
	mustWrite(t, c.paths.Key, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), 0o600)
	mustWrite(t, c.paths.Cert, []byte(c.ca.LeafWith(t, k.Public(), opts)), 0o644)
}

func (c *creds) serial(t *testing.T) string {
	t.Helper()
	l, err := ReadLeafFile(c.paths.Cert)
	if err != nil {
		t.Fatal(err)
	}
	return l.Serial
}

func (c *creds) readState(t *testing.T) State {
	t.Helper()
	s, err := ReadState(c.state)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// fakeNodeward signs each CSR with a leaf of the given life, after it
// checks the proof against the held leaf.
type fakeNodeward struct {
	l2sec.NodewardClient
	t     *testing.T
	c     *creds
	life  time.Duration
	mu    sync.Mutex
	calls []*l2sec.RegenerateCertificateRequest
	at    []time.Time
}

func (f *fakeNodeward) RegenerateCertificate(_ context.Context, in *l2sec.RegenerateCertificateRequest, _ ...grpc.CallOption) (*l2sec.RegenerateCertificateResponse, error) {
	f.mu.Lock()
	f.calls = append(f.calls, in)
	f.at = append(f.at, time.Now())
	f.mu.Unlock()
	if err := checkProof(f.c.paths, in); err != nil {
		return nil, status.Error(codes.PermissionDenied, err.Error())
	}
	block, _ := pem.Decode([]byte(in.CsrPem))
	if block == nil {
		return nil, status.Error(codes.InvalidArgument, "no CSR")
	}
	csr, err := x509.ParseCertificateRequest(block.Bytes)
	if err != nil || csr.CheckSignature() != nil {
		return nil, status.Error(codes.InvalidArgument, "bad CSR")
	}
	opts := leafShape
	opts.NotBefore = time.Now()
	opts.NotAfter = opts.NotBefore.Add(f.life)
	return &l2sec.RegenerateCertificateResponse{CertificatePem: f.c.ca.LeafWith(f.t, csr.PublicKey, opts), CaCert: f.c.ca.PEM}, nil
}

func (f *fakeNodeward) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

// checkProof checks the mint proof against the held leaf, as Nodeward does.
func checkProof(p certstore.Paths, in *l2sec.RegenerateCertificateRequest) error {
	pair, err := certstore.LoadKeyPair(p.Cert, p.Key)
	if err != nil {
		return err
	}
	leafDER := pair.Certificate[0]
	leaf, _ := x509.ParseCertificate(leafDER)
	if in.Proof == nil || len(in.Proof.Nonce) != 32 {
		return errors.New("mint proof is missing or malformed")
	}
	block, _ := pem.Decode([]byte(in.CsrPem))
	if block == nil {
		return errors.New("no CSR")
	}
	digest := sha256.Sum256(csrcert.SigningInput(l2sec.Nodeward_RegenerateCertificate_FullMethodName, block.Bytes,
		in.Proof.UnixSeconds, in.Proof.Nonce, leafDER))
	switch pub := leaf.PublicKey.(type) {
	case *ecdsa.PublicKey:
		if !ecdsa.VerifyASN1(pub, digest[:], in.Proof.Signature) {
			return errors.New("mint proof signature does not verify")
		}
	case *rsa.PublicKey:
		if err := rsa.VerifyPSS(pub, crypto.SHA256, digest[:], in.Proof.Signature,
			&rsa.PSSOptions{SaltLength: rsa.PSSSaltLengthEqualsHash, Hash: crypto.SHA256}); err != nil {
			return errors.New("mint proof signature does not verify")
		}
	default:
		return errors.New("unknown leaf key")
	}
	return nil
}

// realRenew renews through the one renewal function against f.
func realRenew(c *creds, f *fakeNodeward) func(context.Context, Request) (certificate.Result, error) {
	return func(ctx context.Context, r Request) (certificate.Result, error) {
		return certificate.Renew(ctx, certificate.Options{
			Event: "CertificateAutoRenewal", Message: "test",
			ExpectedSerial: r.ExpectedSerial, LostResponse: r.LostResponse,
			Paths: c.paths,
			Dial: func(context.Context) (certificate.MintClient, func(), error) {
				return f, func() {}, nil
			},
			NodeLog: func(string, string) error { return nil },
		})
	}
}

// stub is a scripted Renew. Each call runs the next step; the last step
// repeats.
type stub struct {
	mu    sync.Mutex
	steps []func(r Request) (certificate.Result, error)
	at    []time.Time
}

func (s *stub) renew(_ context.Context, r Request) (certificate.Result, error) {
	s.mu.Lock()
	n := len(s.at)
	s.at = append(s.at, time.Now())
	step := s.steps[min(n, len(s.steps)-1)]
	s.mu.Unlock()
	return step(r)
}

func (s *stub) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.at)
}

func (s *stub) times() []time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]time.Time(nil), s.at...)
}

func fails(err error) func(Request) (certificate.Result, error) {
	return func(Request) (certificate.Result, error) { return certificate.Result{}, err }
}

// newLoop gives a loop on c with renew and a fixed random value.
func newLoop(c *creds, renew func(context.Context, Request) (certificate.Result, error)) *Loop {
	return New(Deps{
		Renew:     renew,
		ReadLeaf:  func() (Leaf, error) { return ReadLeafFile(c.paths.Cert) },
		NodeID:    testNodeID,
		Rand:      func() float64 { return 0.5 },
		StatePath: c.state,
		PID:       4242,
	})
}

// logBuffer is a log sink that the loop goroutine and the test share.
type logBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *logBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

// lines returns the decoded log lines with the message msg ("" for all).
func (b *logBuffer) lines(t *testing.T, msg string) []map[string]any {
	t.Helper()
	b.mu.Lock()
	text := b.buf.String()
	b.mu.Unlock()
	var out []map[string]any
	for _, l := range strings.Split(strings.TrimSpace(text), "\n") {
		if l == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(l), &m); err != nil {
			t.Fatalf("log line is not JSON: %s", l)
		}
		if msg == "" || m["msg"] == msg {
			out = append(out, m)
		}
	}
	return out
}

func (b *logBuffer) text() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func captureLogs(t *testing.T) *logBuffer {
	t.Helper()
	b := &logBuffer{}
	old := roslog.L
	roslog.L = slog.New(slog.NewJSONHandler(b, &slog.HandlerOptions{Level: slog.LevelDebug}))
	t.Cleanup(func() { roslog.L = old })
	return b
}

func countLevel(lines []map[string]any, level string) int {
	n := 0
	for _, l := range lines {
		if l["level"] == level {
			n++
		}
	}
	return n
}

// startLoop starts l on a context that the cleanup cancels, and waits for
// the loop to end before the bubble closes.
func startLoop(t *testing.T, l *Loop) context.CancelFunc {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	l.Start(ctx)
	t.Cleanup(func() {
		cancel()
		if !l.Wait(time.Hour) {
			t.Error("the loop did not end")
		}
	})
	return cancel
}
