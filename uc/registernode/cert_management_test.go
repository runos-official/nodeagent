package registernode

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// genKeyPairPEM returns a freshly generated RSA private key and a self-signed
// certificate, each rendered as a PEM string in the same form the agent receives
// over the wire (PKCS#1 private key, X.509 certificate).
func genKeyPairPEM(t *testing.T) (privPEM string, certPEM string, key *rsa.PrivateKey, cert *x509.Certificate) {
	t.Helper()

	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("rsa.GenerateKey: %v", err)
	}

	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "test-node"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("x509.CreateCertificate: %v", err)
	}
	cert, err = x509.ParseCertificate(der)
	if err != nil {
		t.Fatalf("x509.ParseCertificate: %v", err)
	}

	privPEM = string(pem.EncodeToMemory(&pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: x509.MarshalPKCS1PrivateKey(key),
	}))
	certPEM = string(pem.EncodeToMemory(&pem.Block{
		Type:  "CERTIFICATE",
		Bytes: der,
	}))
	return privPEM, certPEM, key, cert
}

func TestStorePrivateKey_RoundTrip(t *testing.T) {
	privPEM, _, key, _ := genKeyPairPEM(t)
	path := filepath.Join(t.TempDir(), "mtls.key")

	if err := StorePrivateKey(privPEM, path); err != nil {
		t.Fatalf("StorePrivateKey returned error on valid input: %v", err)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading stored private key: %v", err)
	}

	block, _ := pem.Decode(raw)
	if block == nil {
		t.Fatal("stored private key is not valid PEM")
	}
	if block.Type != "RSA PRIVATE KEY" {
		t.Fatalf("PEM block type = %q, want %q", block.Type, "RSA PRIVATE KEY")
	}

	got, err := x509.ParsePKCS1PrivateKey(block.Bytes)
	if err != nil {
		t.Fatalf("parsing stored private key: %v", err)
	}
	if got.N.Cmp(key.N) != 0 || got.E != key.E {
		t.Fatal("stored private key does not match the original modulus/exponent")
	}
}

func TestStorePrivateKey_IsRootOnly0600(t *testing.T) {
	privPEM, _, _, _ := genKeyPairPEM(t)
	path := filepath.Join(t.TempDir(), "mtls.key")

	if err := StorePrivateKey(privPEM, path); err != nil {
		t.Fatalf("StorePrivateKey returned error on valid input: %v", err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat private key: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0600 {
		t.Fatalf("private key mode = %04o, want 0600", perm)
	}
}

func TestStorePrivateKey_TightensExisting0644(t *testing.T) {
	// A pre-existing world-readable key must be corrected to 0600 on next write.
	privPEM, _, _, _ := genKeyPairPEM(t)
	path := filepath.Join(t.TempDir(), "mtls.key")

	if err := os.WriteFile(path, []byte("stale"), 0644); err != nil {
		t.Fatalf("seeding 0644 key: %v", err)
	}
	if err := StorePrivateKey(privPEM, path); err != nil {
		t.Fatalf("StorePrivateKey returned error: %v", err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat private key: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0600 {
		t.Fatalf("private key mode = %04o, want 0600 after rewrite", perm)
	}
}

func TestStorePublicKey_RoundTrip(t *testing.T) {
	_, certPEM, _, cert := genKeyPairPEM(t)
	path := filepath.Join(t.TempDir(), "mtls.crt")

	if err := StorePublicKey(certPEM, path); err != nil {
		t.Fatalf("StorePublicKey returned error on valid input: %v", err)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading stored certificate: %v", err)
	}

	block, _ := pem.Decode(raw)
	if block == nil {
		t.Fatal("stored certificate is not valid PEM")
	}
	if block.Type != "CERTIFICATE" {
		t.Fatalf("PEM block type = %q, want %q", block.Type, "CERTIFICATE")
	}

	got, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatalf("parsing stored certificate: %v", err)
	}
	if got.Subject.CommonName != cert.Subject.CommonName {
		t.Fatalf("stored cert CN = %q, want %q", got.Subject.CommonName, cert.Subject.CommonName)
	}
	if got.SerialNumber.Cmp(cert.SerialNumber) != 0 {
		t.Fatal("stored cert serial number does not match the original")
	}
}

func TestStorePrivateKey_ReturnsErrorOnBadInput(t *testing.T) {
	// A certificate PEM is the wrong block type for a private key, and a
	// non-PEM string has no block at all. Both must return an error (not panic).
	_, certPEM, _, _ := genKeyPairPEM(t)
	cases := []struct {
		name     string
		contents string
	}{
		{"non-PEM garbage", "not a pem block"},
		{"wrong PEM type (certificate)", certPEM},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "bad.key")
			if err := StorePrivateKey(tc.contents, path); err == nil {
				t.Fatal("expected StorePrivateKey to return an error, got nil")
			}
		})
	}
}

func TestStorePublicKey_ReturnsErrorOnBadInput(t *testing.T) {
	// A private-key PEM is the wrong block type for a certificate, and a
	// non-PEM string has no block at all. Both must return an error (not panic).
	privPEM, _, _, _ := genKeyPairPEM(t)
	cases := []struct {
		name     string
		contents string
	}{
		{"non-PEM garbage", "not a pem block"},
		{"wrong PEM type (private key)", privPEM},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "bad.crt")
			if err := StorePublicKey(tc.contents, path); err == nil {
				t.Fatal("expected StorePublicKey to return an error, got nil")
			}
		})
	}
}
