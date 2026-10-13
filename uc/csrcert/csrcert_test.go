package csrcert

import (
	"bytes"
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/runos-official/nodeagent/l2sec"
	"github.com/runos-official/nodeagent/roslog"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// testCA is an RSA CA like nodeward's level 2 CA.
type testCA struct {
	key  *rsa.PrivateKey
	cert *x509.Certificate
	pem  string
}

func newTestCA(t *testing.T) *testCA {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "test ca"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	c, _ := x509.ParseCertificate(der)
	return &testCA{key: key, cert: c, pem: string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))}
}

type leafOpts struct {
	notBefore time.Time
	notAfter  time.Time
	eku       []x509.ExtKeyUsage
}

// sign makes a leaf shaped like nodeward's for pub.
func (ca *testCA) sign(t *testing.T, pub crypto.PublicKey, o leafOpts) string {
	t.Helper()
	if o.notBefore.IsZero() {
		o.notBefore = time.Now().Add(-time.Minute)
	}
	if o.notAfter.IsZero() {
		o.notAfter = time.Now().Add(12 * time.Hour)
	}
	if o.eku == nil {
		o.eku = []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth}
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(time.Now().UnixNano()),
		Subject:               pkix.Name{CommonName: "RunOS L2Sec"},
		NotBefore:             o.notBefore,
		NotAfter:              o.notAfter,
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment | x509.KeyUsageKeyAgreement,
		ExtKeyUsage:           o.eku,
		BasicConstraintsValid: true,
		DNSNames:              []string{"node.example.test"},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.cert, pub, ca.key)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
}

func TestNewKeyMakesP256CSR(t *testing.T) {
	k, err := NewKey()
	if err != nil {
		t.Fatal(err)
	}
	block, rest := pem.Decode([]byte(k.CSRPEM()))
	if block == nil || block.Type != "CERTIFICATE REQUEST" || len(bytes.TrimSpace(rest)) != 0 {
		t.Fatalf("CSR PEM is not one CERTIFICATE REQUEST block")
	}
	if !bytes.Equal(block.Bytes, k.CSRDER()) {
		t.Fatal("CSRDER does not match the PEM block")
	}
	csr, err := x509.ParseCertificateRequest(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	if err := csr.CheckSignature(); err != nil {
		t.Fatalf("CSR self-signature: %v", err)
	}
	if csr.SignatureAlgorithm != x509.ECDSAWithSHA256 {
		t.Errorf("signature algorithm = %v", csr.SignatureAlgorithm)
	}
	pub, ok := csr.PublicKey.(*ecdsa.PublicKey)
	if !ok || pub.Curve != elliptic.P256() {
		t.Fatalf("CSR key is not P-256: %T", csr.PublicKey)
	}
	if !pub.Equal(k.Public()) {
		t.Fatal("CSR key differs from the generated key")
	}
	k2, _ := NewKey()
	if k2.Public().Equal(k.Public()) {
		t.Fatal("two calls returned the same key")
	}
}

func TestSigningInputIsTheContractFormat(t *testing.T) {
	csrDER := []byte("csr-der")
	leafDER := []byte("leaf-der")
	nonce := bytes.Repeat([]byte{0xab}, 32)
	got := string(SigningInput("/runos.nodeward.proto.l2sec.Nodeward/RegenerateCertificate", csrDER, 1760000000, nonce, leafDER))
	csrSum := sha256.Sum256(csrDER)
	leafSum := sha256.Sum256(leafDER)
	want := strings.Join([]string{
		"runos-mint-proof-v1",
		"/runos.nodeward.proto.l2sec.Nodeward/RegenerateCertificate",
		hex.EncodeToString(csrSum[:]),
		"1760000000",
		strings.Repeat("ab", 32),
		hex.EncodeToString(leafSum[:]),
	}, "\n")
	if got != want {
		t.Fatalf("signing input\n got %q\nwant %q", got, want)
	}
}

func TestSignProofECDSALeaf(t *testing.T) {
	leafKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	k, _ := NewKey()
	method := l2sec.Nodeward_RegenerateCertificate_FullMethodName
	leafDER := []byte("leaf")
	now := time.Unix(1760000000, 0)
	p, err := NewProof(leafKey, leafDER, method, k.CSRDER(), now)
	if err != nil {
		t.Fatal(err)
	}
	if p.UnixSeconds != now.Unix() || len(p.Nonce) != 32 {
		t.Fatalf("proof time %d, nonce len %d", p.UnixSeconds, len(p.Nonce))
	}
	digest := sha256.Sum256(SigningInput(method, k.CSRDER(), p.UnixSeconds, p.Nonce, leafDER))
	if !ecdsa.VerifyASN1(&leafKey.PublicKey, digest[:], p.Signature) {
		t.Fatal("ECDSA proof does not verify")
	}
	p2, _ := NewProof(leafKey, leafDER, method, k.CSRDER(), now)
	if bytes.Equal(p.Nonce, p2.Nonce) {
		t.Fatal("nonce repeated across calls")
	}
}

func TestSignProofRSALeafUsesPSS(t *testing.T) {
	leafKey, _ := rsa.GenerateKey(rand.Reader, 2048)
	k, _ := NewKey()
	method := l2sec.Nodeward_GetClusterAgentCredentials_FullMethodName
	p, err := NewProof(leafKey, []byte("leaf"), method, k.CSRDER(), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(SigningInput(method, k.CSRDER(), p.UnixSeconds, p.Nonce, []byte("leaf")))
	err = rsa.VerifyPSS(&leafKey.PublicKey, crypto.SHA256, digest[:], p.Signature,
		&rsa.PSSOptions{SaltLength: rsa.PSSSaltLengthEqualsHash, Hash: crypto.SHA256})
	if err != nil {
		t.Fatalf("RSA-PSS proof does not verify: %v", err)
	}
}

func TestSignProofRefusesOtherKeyTypes(t *testing.T) {
	_, edKey, _ := ed25519.GenerateKey(rand.Reader)
	k, _ := NewKey()
	if _, err := NewProof(edKey, []byte("leaf"), "/m", k.CSRDER(), time.Now()); err == nil {
		t.Fatal("expected an error for an Ed25519 leaf key")
	}
}

func TestResolveCertificatePath(t *testing.T) {
	ca := newTestCA(t)
	k, _ := NewKey()
	certPEM := ca.sign(t, k.Public(), leafOpts{})
	got, err := Resolve(k, Response{CertificatePEM: certPEM, CACertPEM: ca.pem}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if got != OutcomeCertificate {
		t.Fatalf("outcome = %v, want certificate", got)
	}
}

func TestResolveToleratesNotBeforeSkew(t *testing.T) {
	ca := newTestCA(t)
	k, _ := NewKey()
	// Nodeward sets NotBefore to its own now; its clock may run a little ahead.
	certPEM := ca.sign(t, k.Public(), leafOpts{notBefore: time.Now().Add(30 * time.Second)})
	if _, err := Resolve(k, Response{CertificatePEM: certPEM, CACertPEM: ca.pem}, time.Now()); err != nil {
		t.Fatalf("a few seconds of NotBefore skew must not refuse the certificate: %v", err)
	}
}

// Nodeward has already acted when the answer arrives (the token is spent or
// the old leaf is on its grace), so local clock skew must never refuse a
// correctly signed certificate for our key.
func TestResolveAcceptsNotBeforeFarAhead(t *testing.T) {
	for _, ahead := range []time.Duration{12 * time.Minute, 15 * time.Minute, 2 * time.Hour} {
		ca := newTestCA(t)
		k, _ := NewKey()
		certPEM := ca.sign(t, k.Public(), leafOpts{notBefore: time.Now().Add(ahead)})
		if _, err := Resolve(k, Response{CertificatePEM: certPEM, CACertPEM: ca.pem}, time.Now()); err != nil {
			t.Fatalf("NotBefore %v ahead refused: %v", ahead, err)
		}
	}
}

func TestResolveCertificateWinsOverLegacyFields(t *testing.T) {
	ca := newTestCA(t)
	k, _ := NewKey()
	certPEM := ca.sign(t, k.Public(), leafOpts{})
	got, err := Resolve(k, Response{CertificatePEM: certPEM, CACertPEM: ca.pem, PrivateKey: "x", PublicKey: "y"}, time.Now())
	if err != nil || got != OutcomeCertificate {
		t.Fatalf("outcome %v, err %v; want certificate", got, err)
	}
}

func TestResolveLegacyResponse(t *testing.T) {
	k, _ := NewKey()
	got, err := Resolve(k, Response{PrivateKey: "priv", PublicKey: "pub", CACertPEM: "ca"}, time.Now())
	if err != nil || got != OutcomeLegacy {
		t.Fatalf("outcome %v, err %v; want legacy", got, err)
	}
}

func TestResolveRefusesBadResponses(t *testing.T) {
	ca := newTestCA(t)
	other := newTestCA(t)
	k, _ := NewKey()
	foreign, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	good := ca.sign(t, k.Public(), leafOpts{})
	cases := map[string]Response{
		"both empty":       {CACertPEM: ca.pem},
		"only private key": {PrivateKey: "priv", CACertPEM: ca.pem},
		"foreign key":      {CertificatePEM: ca.sign(t, &foreign.PublicKey, leafOpts{}), CACertPEM: ca.pem},
		"no client auth":   {CertificatePEM: ca.sign(t, k.Public(), leafOpts{eku: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}), CACertPEM: ca.pem},
		"expired":          {CertificatePEM: ca.sign(t, k.Public(), leafOpts{notBefore: time.Now().Add(-2 * time.Hour), notAfter: time.Now().Add(-time.Hour)}), CACertPEM: ca.pem},
		"wrong CA":         {CertificatePEM: good, CACertPEM: other.pem},
		"no CA":            {CertificatePEM: good},
		"two certificates": {CertificatePEM: good + good, CACertPEM: ca.pem},
		"not a PEM":        {CertificatePEM: "garbage", CACertPEM: ca.pem},
		"wrong PEM type":   {CertificatePEM: strings.Replace(good, "CERTIFICATE", "SOMETHING", 2), CACertPEM: ca.pem},
	}
	for name, resp := range cases {
		t.Run(name, func(t *testing.T) {
			got, err := Resolve(k, resp, time.Now())
			if !errors.Is(err, ErrUnusableResponse) {
				t.Fatalf("want ErrUnusableResponse, got outcome %v, err %v", got, err)
			}
		})
	}
}

func TestServerTimeFromSkew(t *testing.T) {
	err := status.Error(codes.FailedPrecondition, "mint proof time is outside the 5 minute window; server time 1760000123")
	ts, ok := ServerTimeFromSkew(err)
	if !ok || ts != 1760000123 {
		t.Fatalf("got %d %v", ts, ok)
	}
	for _, e := range []error{
		nil,
		errors.New("mint proof time is outside the 5 minute window; server time 1"),
		status.Error(codes.PermissionDenied, "mint proof time is outside the 5 minute window; server time 1"),
		status.Error(codes.FailedPrecondition, "this agent must renew with a certificate signing request"),
		status.Error(codes.FailedPrecondition, "mint proof time is outside the 5 minute window; server time x"),
	} {
		if _, ok := ServerTimeFromSkew(e); ok {
			t.Errorf("parsed a server time from %v", e)
		}
	}
}

func TestCallWithProofRetriesOnceOnSkew(t *testing.T) {
	leafKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	k, _ := NewKey()
	local := time.Unix(1760000000, 0)
	clock := func() time.Time { return local }
	server := local.Add(10 * time.Minute).Unix()
	var proofs []*l2sec.MintProof
	err := CallWithProof(leafKey, []byte("leaf"), "/m", k, clock, func(p *l2sec.MintProof) error {
		proofs = append(proofs, p)
		if len(proofs) == 1 {
			return status.Error(codes.FailedPrecondition, fmt.Sprintf("mint proof time is outside the 5 minute window; server time %d", server))
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(proofs) != 2 {
		t.Fatalf("calls = %d, want 2", len(proofs))
	}
	if proofs[1].UnixSeconds != server {
		t.Errorf("retry time = %d, want server time %d", proofs[1].UnixSeconds, server)
	}
	if bytes.Equal(proofs[0].Nonce, proofs[1].Nonce) {
		t.Error("retry reused the nonce")
	}
}

func TestCallWithProofRetriesOnlyOnce(t *testing.T) {
	leafKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	k, _ := NewKey()
	calls := 0
	skew := status.Error(codes.FailedPrecondition, "mint proof time is outside the 5 minute window; server time 1")
	err := CallWithProof(leafKey, []byte("leaf"), "/m", k, time.Now, func(*l2sec.MintProof) error {
		calls++
		return skew
	})
	if err == nil || calls != 2 {
		t.Fatalf("calls = %d, err = %v; want 2 calls and an error", calls, err)
	}
}

func TestCallWithProofDoesNotRetryOtherErrors(t *testing.T) {
	leafKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	k, _ := NewKey()
	calls := 0
	err := CallWithProof(leafKey, []byte("leaf"), "/m", k, time.Now, func(*l2sec.MintProof) error {
		calls++
		return status.Error(codes.PermissionDenied, "mint proof nonce was already used")
	})
	if err == nil || calls != 1 {
		t.Fatalf("calls = %d, err = %v; want 1 call and an error", calls, err)
	}
}

func TestWriteKeyFileIsPKCS8And0600(t *testing.T) {
	ca := newTestCA(t)
	k, _ := NewKey()
	dir := t.TempDir()
	keyPath := filepath.Join(dir, "mtls.key")
	// An old world-readable RSA key sits there today.
	if err := os.WriteFile(keyPath, []byte("old"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := WriteKeyFile(k, keyPath); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(keyPath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0600 {
		t.Fatalf("mode = %v, want 0600", info.Mode().Perm())
	}
	data, _ := os.ReadFile(keyPath)
	block, _ := pem.Decode(data)
	if block == nil || block.Type != "PRIVATE KEY" {
		t.Fatalf("key file is not a PKCS#8 PRIVATE KEY block")
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	if !parsed.(*ecdsa.PrivateKey).PublicKey.Equal(k.Public()) {
		t.Fatal("stored key differs")
	}
	// The pair loads the way the agent loads it at start.
	certPath := filepath.Join(dir, "mtls.crt")
	_ = os.WriteFile(certPath, []byte(ca.sign(t, k.Public(), leafOpts{})), 0644)
	if _, err := tls.LoadX509KeyPair(certPath, keyPath); err != nil {
		t.Fatalf("LoadX509KeyPair: %v", err)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 2 {
		t.Fatalf("temp files left behind: %d entries", len(entries))
	}
}

func TestNoKeyMaterialInLogs(t *testing.T) {
	var buf bytes.Buffer
	old := roslog.L
	roslog.L = slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	defer func() { roslog.L = old }()

	ca := newTestCA(t)
	leafKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	k, _ := NewKey()
	_ = CallWithProof(leafKey, []byte("leaf"), "/m", k, time.Now, func(*l2sec.MintProof) error {
		return status.Error(codes.FailedPrecondition, "mint proof time is outside the 5 minute window; server time 1")
	})
	certPEM := ca.sign(t, k.Public(), leafOpts{})
	_, _ = Resolve(k, Response{CertificatePEM: certPEM, CACertPEM: ca.pem, PrivateKey: "legacy-key-text"}, time.Now())
	_, _ = Resolve(k, Response{PrivateKey: "legacy-key-text", PublicKey: "p"}, time.Now())
	_ = WriteKeyFile(k, filepath.Join(t.TempDir(), "k"))

	out := buf.String()
	if out == "" {
		t.Fatal("expected some log output")
	}
	for _, bad := range []string{"PRIVATE KEY", "CERTIFICATE REQUEST", "legacy-key-text", k.CSRPEM()} {
		if strings.Contains(out, bad) {
			t.Fatalf("log contains %q", bad)
		}
	}
}
