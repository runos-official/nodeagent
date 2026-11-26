package backend

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/spf13/viper"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"

	"github.com/runos-official/nodeagent/roslog"
)

// clientLeaf issues a client leaf of ca valid from notBefore to notAfter.
func (ca t50CA) clientLeaf(t *testing.T, notBefore, notAfter time.Time) tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject:      pkix.Name{CommonName: "node agent"},
		NotBefore:    notBefore,
		NotAfter:     notAfter,
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.cert, &key.PublicKey, ca.key)
	if err != nil {
		t.Fatal(err)
	}
	leaf, _ := x509.ParseCertificate(der)
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key, Leaf: leaf}
}

// serverLeafFor issues a server leaf of ca for the loopback address.
func (ca t50CA) serverLeafFor(t *testing.T) tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject:      pkix.Name{CommonName: "nodeward"},
		IPAddresses:  []net.IP{net.IPv4(127, 0, 0, 1)},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.cert, &key.PublicKey, ca.key)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}

// useHeldPair writes leaf and the CA of the agent as the configured
// credential files and points the Nodeward host at the loopback address.
func useHeldPair(t *testing.T, leaf tls.Certificate, agentCA t50CA) {
	t.Helper()
	dir := t.TempDir()
	keyDER, err := x509.MarshalPKCS8PrivateKey(leaf.PrivateKey)
	if err != nil {
		t.Fatal(err)
	}
	files := map[string][]byte{
		"mtls.crt": pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: leaf.Certificate[0]}),
		"mtls.key": pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}),
		"ca.crt":   agentCA.pem,
	}
	for name, b := range files {
		if err := os.WriteFile(filepath.Join(dir, name), b, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	keys := map[string]string{
		"mtls.crt":               filepath.Join(dir, "mtls.crt"),
		"mtls.key":               filepath.Join(dir, "mtls.key"),
		"mtls.ca":                filepath.Join(dir, "ca.crt"),
		"client.server.nodeward": "127.0.0.1",
	}
	for k, v := range keys {
		old := viper.Get(k)
		viper.Set(k, v)
		t.Cleanup(func() { viper.Set(k, old) })
	}
}

// usePort points the dial at port for the test.
func usePort(t *testing.T, port int) {
	t.Helper()
	old := l2secPort
	l2secPort = port
	t.Cleanup(func() { l2secPort = old })
}

// captureBackendLog sends roslog to a buffer for the test.
func captureBackendLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	old := roslog.L
	roslog.L = slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	t.Cleanup(func() { roslog.L = old })
	return &buf
}

// countingListener counts accepted connections.
type countingListener struct {
	net.Listener
	accepted atomic.Int64
}

func (l *countingListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err == nil {
		l.accepted.Add(1)
	}
	return c, err
}

func portOf(t *testing.T, l net.Listener) int {
	t.Helper()
	_, p, err := net.SplitHostPort(l.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	var n int
	fmt.Sscanf(p, "%d", &n)
	return n
}

// TestDialRefusesExpiredLeafLocally (criterion 1): an expired held leaf gives
// CertificateExpiredError in under 1 s, with no connection to the target and
// no log line.
func TestDialRefusesExpiredLeafLocally(t *testing.T) {
	ca := newT50CA(t, "agent CA")
	now := time.Now()
	leaf := ca.clientLeaf(t, now.Add(-48*time.Hour), now.Add(-time.Minute))
	useHeldPair(t, leaf, ca)

	raw, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	l := &countingListener{Listener: raw}
	defer l.Close()
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()
	usePort(t, portOf(t, l))
	logs := captureBackendLog(t)
	before := DialAttempts()

	start := time.Now()
	_, _, _, conn, err := NodewardL2Sec()
	took := time.Since(start)
	if conn != nil {
		conn.Close()
	}
	var expired *CertificateExpiredError
	if !errors.As(err, &expired) {
		t.Fatalf("err = %v, want CertificateExpiredError", err)
	}
	if !IsCertificateExpired(err) || AsCertificateExpired(err) != expired {
		t.Fatal("IsCertificateExpired / AsCertificateExpired do not find the error")
	}
	if !expired.NotAfter.Equal(leaf.Leaf.NotAfter) || expired.Serial != fmt.Sprintf("%x", leaf.Leaf.SerialNumber) {
		t.Fatalf("error = %+v, want NotAfter %s and serial %x", expired, leaf.Leaf.NotAfter, leaf.Leaf.SerialNumber)
	}
	want := "node agent certificate expired on " + leaf.Leaf.NotAfter.UTC().Format(time.RFC3339)
	if err.Error() != want {
		t.Fatalf("text = %q, want %q", err.Error(), want)
	}
	if took >= time.Second {
		t.Fatalf("the dial took %s, want under 1 s", took)
	}
	time.Sleep(50 * time.Millisecond)
	if n := l.accepted.Load(); n != 0 {
		t.Fatalf("the target accepted %d connections, want 0", n)
	}
	if DialAttempts() != before {
		t.Fatal("the dial counter rose for an expired leaf")
	}
	if logs.Len() != 0 {
		t.Fatalf("the dial wrote log lines: %s", logs.String())
	}
}

// refusingServer serves gRPC over TLS on loopback with server config cfg.
func refusingServer(t *testing.T, cfg *tls.Config) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := grpc.NewServer(grpc.Creds(credentials.NewTLS(cfg)))
	go srv.Serve(l)
	t.Cleanup(srv.Stop)
	return portOf(t, l)
}

func useDialLimit(t *testing.T, d time.Duration) {
	t.Helper()
	old := l2secDialLimit
	l2secDialLimit = d
	t.Cleanup(func() { l2secDialLimit = old })
}

func useCheckClock(t *testing.T, at time.Time) {
	t.Helper()
	old := l2secNow
	l2secNow = func() time.Time { return at }
	t.Cleanup(func() { l2secNow = old })
}

// TestClientCertificateRefusalIsTyped (criterion 2): a server that refuses the
// client certificate in the TLS handshake gives ClientCertificateRefusedError
// with the reason, for TLS 1.2 and TLS 1.3. An unreachable address keeps the
// deadline error.
func TestClientCertificateRefusalIsTyped(t *testing.T) {
	if testing.Short() {
		t.Skip("each refused dial waits for the dial limit")
	}
	useDialLimit(t, 2*time.Second)
	agentCA := newT50CA(t, "agent CA")
	foreignCA := newT50CA(t, "foreign CA")
	serverLeaf := agentCA.serverLeafFor(t)
	pool := x509.NewCertPool()
	pool.AddCert(agentCA.cert)
	now := time.Now()

	// verifyAgainst checks the client chain against pool without naming a
	// CA in the certificate request, so the client sends any certificate.
	verifyAgainst := func(rawCerts [][]byte, _ [][]*x509.Certificate) error {
		c, err := x509.ParseCertificate(rawCerts[0])
		if err != nil {
			return err
		}
		_, err = c.Verify(x509.VerifyOptions{Roots: pool, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}})
		return err
	}

	type tcase struct {
		name      string
		leaf      tls.Certificate
		checkAt   time.Time
		server    func(v uint16) *tls.Config
		reasonFor map[uint16]string
	}
	verifying := func(v uint16) *tls.Config {
		return &tls.Config{Certificates: []tls.Certificate{serverLeaf}, ClientAuth: tls.RequireAndVerifyClientCert,
			ClientCAs: pool, MinVersion: v, MaxVersion: v}
	}
	cases := []tcase{
		{
			// The leaf passes the local check (the check clock is before
			// NotAfter) and has expired when the server checks it.
			name:      "leaf expires between the local check and the handshake",
			leaf:      agentCA.clientLeaf(t, now.Add(-48*time.Hour), now.Add(-time.Hour)),
			checkAt:   now.Add(-2 * time.Hour),
			server:    verifying,
			reasonFor: map[uint16]string{tls.VersionTLS12: "expired certificate", tls.VersionTLS13: "expired certificate"},
		},
		{
			// The server names its CA; crypto/tls sends no certificate of
			// another issuer.
			name:      "unknown CA, the server names its CA",
			leaf:      foreignCA.clientLeaf(t, now.Add(-time.Hour), now.Add(time.Hour)),
			server:    verifying,
			reasonFor: map[uint16]string{tls.VersionTLS12: withheldReason, tls.VersionTLS13: withheldReason},
		},
		{
			// The server names no CA and checks the chain itself.
			name: "unknown CA, the server names no CA",
			leaf: foreignCA.clientLeaf(t, now.Add(-time.Hour), now.Add(time.Hour)),
			server: func(v uint16) *tls.Config {
				return &tls.Config{Certificates: []tls.Certificate{serverLeaf}, ClientAuth: tls.RequireAnyClientCert,
					VerifyPeerCertificate: verifyAgainst, MinVersion: v, MaxVersion: v}
			},
			reasonFor: map[uint16]string{tls.VersionTLS12: "bad certificate", tls.VersionTLS13: "bad certificate"},
		},
	}
	for _, c := range cases {
		for _, v := range []uint16{tls.VersionTLS12, tls.VersionTLS13} {
			name := fmt.Sprintf("%s/TLS1.%d", c.name, v-tls.VersionTLS10)
			t.Run(name, func(t *testing.T) {
				useHeldPair(t, c.leaf, agentCA)
				if !c.checkAt.IsZero() {
					useCheckClock(t, c.checkAt)
				}
				usePort(t, refusingServer(t, c.server(v)))
				captureBackendLog(t)

				_, _, _, conn, err := NodewardL2Sec()
				if conn != nil {
					conn.Close()
				}
				var refused *ClientCertificateRefusedError
				if !errors.As(err, &refused) || !IsClientCertificateRefused(err) {
					t.Fatalf("err = %v, want ClientCertificateRefusedError", err)
				}
				t.Logf("measured dial error: %q", refused.Cause.Error())
				if refused.Reason != c.reasonFor[v] {
					t.Fatalf("reason = %q, want %q (cause %q)", refused.Reason, c.reasonFor[v], refused.Cause)
				}
				if errors.Unwrap(err) != refused.Cause {
					t.Fatal("the typed error does not wrap the dial error")
				}
			})
		}
	}

	t.Run("unreachable address keeps the deadline error", func(t *testing.T) {
		useHeldPair(t, agentCA.clientLeaf(t, now.Add(-time.Hour), now.Add(time.Hour)), agentCA)
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		port := portOf(t, l)
		l.Close()
		usePort(t, port)
		captureBackendLog(t)

		_, _, _, conn, err := NodewardL2Sec()
		if conn != nil {
			conn.Close()
		}
		if err == nil || IsClientCertificateRefused(err) || IsCertificateExpired(err) {
			t.Fatalf("err = %v, want the untyped dial error", err)
		}
		if !strings.Contains(err.Error(), context.DeadlineExceeded.Error()) {
			t.Fatalf("err = %q, want the deadline error", err)
		}
		t.Logf("measured dial error: %q", err.Error())
	})
}

// TestClassifyClientCertRefusal pins the match rule on the measured texts.
func TestClassifyClientCertRefusal(t *testing.T) {
	measured := map[string]string{
		// TLS 1.2: the alert arrives in the handshake.
		`context deadline exceeded: connection error: desc = "transport: authentication handshake failed: remote error: tls: expired certificate"`: "expired certificate",
		// TLS 1.3: the alert arrives at the first read after the handshake.
		`context deadline exceeded: connection error: desc = "error reading server preface: remote error: tls: expired certificate"`:  "expired certificate",
		`context deadline exceeded: connection error: desc = "error reading server preface: remote error: tls: bad certificate"`:      "bad certificate",
		`context deadline exceeded: connection error: desc = "error reading server preface: remote error: tls: certificate required"`: "certificate required",
		`remote error: tls: unknown certificate authority`:                                                                            "unknown certificate authority",
		`remote error: tls: certificate revoked`:                                                                                      "certificate revoked",
	}
	for text, reason := range measured {
		got := classifyClientCertRefusal(errors.New(text))
		var refused *ClientCertificateRefusedError
		if !errors.As(got, &refused) || refused.Reason != reason {
			t.Errorf("%q: got %v, want reason %q", text, got, reason)
		}
	}
	for _, text := range []string{
		"context deadline exceeded",
		`context deadline exceeded: connection error: desc = "transport: authentication handshake failed: remote error: tls: handshake failure"`,
		`context deadline exceeded: connection error: desc = "transport: Error while dialing: dial tcp 127.0.0.1:1: connect: connection refused"`,
		"remote error: tls: protocol version not supported",
	} {
		err := errors.New(text)
		if got := classifyClientCertRefusal(err); got != err {
			t.Errorf("%q: classified as %v, want unchanged", text, got)
		}
		if got := classifyDialRefusal(err, false, ""); got != err {
			t.Errorf("%q: classifyDialRefusal changed an error with no withheld certificate", text)
		}
	}
	if got := classifyDialRefusal(errors.New("write: broken pipe"), true, ""); !IsClientCertificateRefused(got) {
		t.Errorf("a withheld certificate must type the error, got %v", got)
	}
	reset := errors.New("context deadline exceeded: write tcp 127.0.0.1:1->127.0.0.1:2: write: connection reset by peer")
	var refused *ClientCertificateRefusedError
	if got := classifyDialRefusal(reset, false, "bad certificate"); !errors.As(got, &refused) || refused.Reason != "bad certificate" || refused.Cause != reset {
		t.Errorf("a refusal seen on an earlier attempt must type a reset of the last attempt, got %v", got)
	}
}

// TestCheckHeldCertificate: an expired held certificate gives the typed
// error; a valid, missing or unreadable one gives nil, with no log line.
func TestCheckHeldCertificate(t *testing.T) {
	ca := newT50CA(t, "agent CA")
	now := time.Now()
	logs := captureBackendLog(t)

	useHeldPair(t, ca.clientLeaf(t, now.Add(-48*time.Hour), now.Add(-time.Second)), ca)
	if err := CheckHeldCertificate(); !IsCertificateExpired(err) {
		t.Fatalf("expired: err = %v, want CertificateExpiredError", err)
	}
	useHeldPair(t, ca.clientLeaf(t, now.Add(-time.Hour), now.Add(time.Hour)), ca)
	if err := CheckHeldCertificate(); err != nil {
		t.Fatalf("valid: err = %v, want nil", err)
	}
	missing := filepath.Join(t.TempDir(), "absent.crt")
	viper.Set("mtls.crt", missing)
	if err := CheckHeldCertificate(); err != nil {
		t.Fatalf("missing: err = %v, want nil", err)
	}
	if _, err := os.Stat(missing); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("the check created the certificate file")
	}
	if logs.Len() != 0 {
		t.Fatalf("the check wrote log lines: %s", logs.String())
	}
}
