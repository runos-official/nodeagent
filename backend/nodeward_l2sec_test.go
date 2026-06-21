package backend

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// Pins the panic->return fix in the L2Sec mTLS setup: a missing, empty, or
// non-PEM client cert / CA must yield a clean ERROR (never a panic), so a
// half-installed/unregistered node surfaces a readable message and the reconnect
// loop can back off instead of crash-looping on a Go stack trace.
func TestLoadL2SecClientTLS(t *testing.T) {
	dir := t.TempDir()

	// A real, self-signed keypair + CA PEM so the happy path actually parses.
	certPath, keyPath := writeKeyPair(t, dir, "client")
	caPath := certPath // a valid cert PEM doubles as a valid CA bundle for AppendCertsFromPEM

	missing := filepath.Join(dir, "does-not-exist.pem")
	emptyPEM := filepath.Join(dir, "empty.pem")
	if err := os.WriteFile(emptyPEM, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	garbagePEM := filepath.Join(dir, "garbage.pem")
	if err := os.WriteFile(garbagePEM, []byte("this is not a certificate"), 0o600); err != nil {
		t.Fatal(err)
	}

	t.Run("valid material loads", func(t *testing.T) {
		cfg, err := loadL2SecClientTLS(certPath, keyPath, caPath, "nodeward.example")
		if err != nil {
			t.Fatalf("valid material: unexpected error: %v", err)
		}
		if cfg == nil {
			t.Fatal("valid material: nil tls.Config")
		}
		if cfg.ServerName != "nodeward.example" {
			t.Errorf("ServerName = %q, want nodeward.example", cfg.ServerName)
		}
		if len(cfg.Certificates) != 1 {
			t.Errorf("expected exactly 1 client certificate, got %d", len(cfg.Certificates))
		}
		if cfg.RootCAs == nil {
			t.Error("RootCAs not set")
		}
	})

	t.Run("missing client cert errors, no panic", func(t *testing.T) {
		if _, err := loadL2SecClientTLS(missing, keyPath, caPath, "h"); err == nil {
			t.Fatal("missing client cert: want error, got nil")
		}
	})

	t.Run("missing CA errors, no panic", func(t *testing.T) {
		if _, err := loadL2SecClientTLS(certPath, keyPath, missing, "h"); err == nil {
			t.Fatal("missing CA: want error, got nil")
		}
	})

	t.Run("non-PEM CA errors, no panic", func(t *testing.T) {
		// A readable-but-garbage CA must fail at AppendCertsFromPEM, not panic.
		if _, err := loadL2SecClientTLS(certPath, keyPath, garbagePEM, "h"); err == nil {
			t.Fatal("non-PEM CA: want error, got nil")
		}
	})

	t.Run("empty CA errors, no panic", func(t *testing.T) {
		if _, err := loadL2SecClientTLS(certPath, keyPath, emptyPEM, "h"); err == nil {
			t.Fatal("empty CA: want error, got nil")
		}
	})

	t.Run("empty client cert errors, no panic", func(t *testing.T) {
		if _, err := loadL2SecClientTLS(emptyPEM, keyPath, caPath, "h"); err == nil {
			t.Fatal("empty client cert: want error, got nil")
		}
	})
}

// writeKeyPair writes a self-signed ECDSA cert PEM and its key PEM to dir and
// returns their paths. Enough to satisfy tls.LoadX509KeyPair + AppendCertsFromPEM
// in the happy-path assertion without depending on any fixture files.
func writeKeyPair(t *testing.T, dir, name string) (certPath, keyPath string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: name},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		IsCA:         true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}

	certPath = filepath.Join(dir, name+".crt")
	keyPath = filepath.Join(dir, name+".key")
	if err := os.WriteFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), 0o600); err != nil {
		t.Fatal(err)
	}
	return certPath, keyPath
}
