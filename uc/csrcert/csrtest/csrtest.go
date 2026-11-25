// Package csrtest is a tiny fake nodeward PKI for agent tests (FPL315). It
// signs CSRs the way nodeward does and makes legacy key responses. Tests only.
package csrtest

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/asn1"
	"encoding/pem"
	"errors"
	"math/big"
	"testing"
	"time"
)

// CA is an RSA CA like nodeward's level 2 CA.
type CA struct {
	Key  *rsa.PrivateKey
	Cert *x509.Certificate
	PEM  string
	// ServerNow, when set, is the fake nodeward clock. Leaves then get
	// NotBefore = ServerNow(), as nodeward stamps them.
	ServerNow func() time.Time
}

// NewCA makes a fresh test CA.
func NewCA(t testing.TB) *CA {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "test ca"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(48 * time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	c, _ := x509.ParseCertificate(der)
	return &CA{Key: key, Cert: c, PEM: string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))}
}

// Nodeward agent leaf extensions (Nodeward uc/cert/version.go). Each
// extension value is one raw byte, not DER.
var (
	VersionOID = asn1.ObjectIdentifier{1, 3, 6, 1, 4, 1, 99999, 1, 1}
	TypeOID    = asn1.ObjectIdentifier{1, 3, 6, 1, 4, 1, 99999, 1, 2}
)

// LeafOptions shapes a leaf like a Nodeward v2 agent leaf. The zero value
// gives the plain leaf of Leaf.
type LeafOptions struct {
	// DNSNames are the account DNS names that a v2 leaf carries.
	DNSNames []string
	// Version is the value of the version extension; 0 omits the extension.
	Version byte
	// Type is the value of the type extension; 0 omits the extension.
	Type byte
	// NoClientAuth drops the ClientAuth extended key usage.
	NoClientAuth bool
	// NotBefore and NotAfter override the validity when not zero.
	NotBefore, NotAfter time.Time
}

// Leaf signs a leaf shaped like nodeward's for pub.
func (ca *CA) Leaf(t testing.TB, pub crypto.PublicKey) string {
	t.Helper()
	return ca.LeafWith(t, pub, LeafOptions{})
}

// LeafWith signs a leaf for pub with the shape that opts gives.
func (ca *CA) LeafWith(t testing.TB, pub crypto.PublicKey, opts LeafOptions) string {
	t.Helper()
	notBefore := time.Now().Add(-time.Minute)
	if ca.ServerNow != nil {
		notBefore = ca.ServerNow()
	}
	if !opts.NotBefore.IsZero() {
		notBefore = opts.NotBefore
	}
	notAfter := notBefore.Add(24 * time.Hour)
	if !opts.NotAfter.IsZero() {
		notAfter = opts.NotAfter
	}
	eku := []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth}
	if opts.NoClientAuth {
		eku = []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(time.Now().UnixNano()),
		Subject:               pkix.Name{CommonName: "RunOS L2Sec"},
		NotBefore:             notBefore,
		NotAfter:              notAfter,
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment | x509.KeyUsageKeyAgreement,
		ExtKeyUsage:           eku,
		BasicConstraintsValid: true,
		DNSNames:              opts.DNSNames,
	}
	if opts.Version != 0 {
		tmpl.ExtraExtensions = append(tmpl.ExtraExtensions, pkix.Extension{Id: VersionOID, Value: []byte{opts.Version}})
	}
	if opts.Type != 0 {
		tmpl.ExtraExtensions = append(tmpl.ExtraExtensions, pkix.Extension{Id: TypeOID, Value: []byte{opts.Type}})
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.Cert, pub, ca.Key)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
}

// SignCSR checks a CSR the way nodeward does and returns a leaf for its key.
func (ca *CA) SignCSR(t testing.TB, csrPEM string) (string, *x509.CertificateRequest, error) {
	t.Helper()
	block, _ := pem.Decode([]byte(csrPEM))
	if block == nil || block.Type != "CERTIFICATE REQUEST" {
		return "", nil, errors.New("no CSR block")
	}
	csr, err := x509.ParseCertificateRequest(block.Bytes)
	if err != nil {
		return "", nil, err
	}
	if err := csr.CheckSignature(); err != nil {
		return "", nil, err
	}
	return ca.Leaf(t, csr.PublicKey), csr, nil
}

// LegacyPair makes the RSA key and certificate an old nodeward returns.
func (ca *CA) LegacyPair(t testing.TB) (keyPEM, certPEM string) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	keyPEM = string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}))
	return keyPEM, ca.Leaf(t, &key.PublicKey)
}
