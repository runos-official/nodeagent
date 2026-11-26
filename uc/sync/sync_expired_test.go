package sync

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"log/slog"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/spf13/viper"

	"github.com/runos-official/nodeagent/backend"
	"github.com/runos-official/nodeagent/roslog"
)

// useHeldCert writes a self-signed leaf valid until notAfter as the
// configured client certificate.
func useHeldCert(t *testing.T, notAfter time.Time) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(42),
		Subject:      pkix.Name{CommonName: "node agent"},
		NotBefore:    notAfter.Add(-48 * time.Hour),
		NotAfter:     notAfter,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "mtls.crt")
	if err := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	old := viper.Get("mtls.crt")
	viper.Set("mtls.crt", path)
	t.Cleanup(func() { viper.Set("mtls.crt", old) })
}

func captureSyncLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	old := roslog.L
	roslog.L = slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	t.Cleanup(func() { roslog.L = old })
	return &buf
}

// TestForceVpnSyncSilentWhenExpired (finding 1772): with an expired held
// certificate the VPN sync returns CertificateExpiredError before the
// WireGuard and Kubernetes checks: no log line, no shell command, no dial.
func TestForceVpnSyncSilentWhenExpired(t *testing.T) {
	notAfter := time.Now().Add(-time.Minute)
	useHeldCert(t, notAfter)
	logs := captureSyncLog(t)
	dials := backend.DialAttempts()

	skipped, err := ForceVpnSyncWithCount()
	if !backend.IsCertificateExpired(err) || skipped != 0 {
		t.Fatalf("ForceVpnSyncWithCount = %d, %v; want CertificateExpiredError", skipped, err)
	}
	if e := backend.AsCertificateExpired(err); !e.NotAfter.Equal(notAfter.Truncate(time.Second)) || e.Serial != "2a" {
		t.Fatalf("error = %+v, want the held expiry and serial 2a", e)
	}
	if logs.Len() != 0 {
		t.Fatalf("the sync wrote log lines: %s", logs.String())
	}
	if backend.DialAttempts() != dials {
		t.Fatal("the sync dialed with an expired certificate")
	}
}

// TestForceVpnSyncPassesTheGuardWithAValidCertificate: a valid certificate is
// not refused by the new check; the sync goes on to its WireGuard step.
func TestForceVpnSyncPassesTheGuardWithAValidCertificate(t *testing.T) {
	if _, err := net.InterfaceByName("wg0"); err == nil {
		t.Skip("this host has wg0; the sync would go on to a real dial")
	}
	useHeldCert(t, time.Now().Add(24*time.Hour))
	captureSyncLog(t)
	_, err := ForceVpnSyncWithCount()
	if err == nil || backend.IsCertificateExpired(err) {
		t.Fatalf("err = %v, want the wg0 error after the certificate check", err)
	}
}
