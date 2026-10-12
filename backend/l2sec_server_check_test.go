package backend

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/asn1"
	"encoding/pem"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// T-50 (FPL315, F5 path). The agent CA signs both the L2SEC server certificate
// and every agent certificate. An agent certificate carries ServerAuth, so
// without this check one agent's key could impersonate nodeward to another
// agent. The agent now refuses a server leaf with ClientAuth or with any
// extension under the RunOS arc.

const t50ServerName = "nodeward.invalid"

type t50CA struct {
	cert *x509.Certificate
	key  *ecdsa.PrivateKey
	pem  []byte
}

func newT50CA(t *testing.T, cn string) t50CA {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: cn},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return t50CA{cert: cert, key: key, pem: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})}
}

// leaf issues a leaf for t50ServerName with the given usages and extensions.
func (ca t50CA) leaf(t *testing.T, ekus []x509.ExtKeyUsage, exts []pkix.Extension) tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(time.Now().UnixNano()),
		Subject:               pkix.Name{CommonName: t50ServerName},
		DNSNames:              []string{t50ServerName},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:           ekus,
		BasicConstraintsValid: true,
		ExtraExtensions:       exts,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.cert, &key.PublicKey, ca.key)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}

// The two extensions nodeward puts on every agent certificate (uc/cert/version.go).
var runosAgentExts = []pkix.Extension{
	{Id: asn1.ObjectIdentifier{1, 3, 6, 1, 4, 1, 99999, 1, 1}, Value: []byte{2}},
	{Id: asn1.ObjectIdentifier{1, 3, 6, 1, 4, 1, 99999, 1, 2}, Value: []byte{1}},
}

// handshakeAgainst runs a real TLS handshake: the agent's L2SEC client config
// (trusting caPEM, expecting serverName) against a server that presents leaf.
func handshakeAgainst(t *testing.T, caPEM []byte, serverName string, leaf tls.Certificate) error {
	t.Helper()
	dir := t.TempDir()
	certPath, keyPath := writeKeyPair(t, dir, "agent")
	caPath := filepath.Join(dir, "ca.crt")
	if err := os.WriteFile(caPath, caPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := loadL2SecClientTLS(certPath, keyPath, caPath, serverName)
	if err != nil {
		t.Fatalf("load client TLS: %v", err)
	}

	cliConn, srvConn := net.Pipe()
	defer cliConn.Close()
	defer srvConn.Close()
	_ = cliConn.SetDeadline(time.Now().Add(10 * time.Second))
	_ = srvConn.SetDeadline(time.Now().Add(10 * time.Second))

	srv := tls.Server(srvConn, &tls.Config{Certificates: []tls.Certificate{leaf}, ClientAuth: tls.RequestClientCert})
	srvDone := make(chan struct{})
	go func() {
		defer close(srvDone)
		_ = srv.Handshake()
		srvConn.Close()
	}()
	cli := tls.Client(cliConn, cfg)
	err = cli.Handshake()
	cliConn.Close()
	<-srvDone
	return err
}

func TestL2SecServerCheckRefusesAgentLeaves(t *testing.T) {
	ca := newT50CA(t, "agent CA")
	foreign := newT50CA(t, "foreign CA")

	cases := []struct {
		name       string
		caPEM      []byte
		serverName string
		leaf       tls.Certificate
		wantErr    string // empty means the handshake must succeed
	}{
		{
			// Today's nodeward server certificate: ServerAuth only, no RunOS extension.
			name: "server leaf passes", caPEM: ca.pem, serverName: t50ServerName,
			leaf: ca.leaf(t, []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, nil),
		},
		{
			// An agent certificate as nodeward issues it today.
			name: "agent leaf is refused", caPEM: ca.pem, serverName: t50ServerName,
			leaf:    ca.leaf(t, []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth}, runosAgentExts),
			wantErr: "T-50",
		},
		{
			name: "ClientAuth alone is refused", caPEM: ca.pem, serverName: t50ServerName,
			leaf:    ca.leaf(t, []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth}, nil),
			wantErr: "T-50",
		},
		{
			name: "RunOS extension alone is refused", caPEM: ca.pem, serverName: t50ServerName,
			leaf:    ca.leaf(t, []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, runosAgentExts[1:]),
			wantErr: "T-50",
		},
		{
			name: "deeper arc extension is refused", caPEM: ca.pem, serverName: t50ServerName,
			leaf: ca.leaf(t, []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
				[]pkix.Extension{{Id: asn1.ObjectIdentifier{1, 3, 6, 1, 4, 1, 99999, 7, 3, 1}, Value: []byte{0x05, 0x00}}}),
			wantErr: "T-50",
		},
		{
			// Keep the existing CA check.
			name: "foreign CA is refused", caPEM: ca.pem, serverName: t50ServerName,
			leaf:    foreign.leaf(t, []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, nil),
			wantErr: "certificate",
		},
		{
			// Keep the existing server-name check.
			name: "wrong server name is refused", caPEM: ca.pem, serverName: "other.invalid",
			leaf:    ca.leaf(t, []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, nil),
			wantErr: "certificate",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := handshakeAgainst(t, c.caPEM, c.serverName, c.leaf)
			if c.wantErr == "" {
				if err != nil {
					t.Fatalf("handshake failed: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("handshake succeeded, want refusal")
			}
			if !strings.Contains(err.Error(), c.wantErr) {
				t.Fatalf("error %q does not mention %q", err, c.wantErr)
			}
		})
	}
}

func TestL2SecServerCheckOnlyLooksAtTheRunOSArc(t *testing.T) {
	// A sibling arc (99998) and a prefix that only shares digits (999990) are not RunOS.
	for _, oid := range []asn1.ObjectIdentifier{
		{1, 3, 6, 1, 4, 1, 99998, 1, 1},
		{1, 3, 6, 1, 4, 1, 999990, 1},
		{1, 3, 6, 1, 4, 1},
	} {
		if isRunOSExtension(oid) {
			t.Errorf("%v must not count as a RunOS extension", oid)
		}
	}
	if !isRunOSExtension(asn1.ObjectIdentifier{1, 3, 6, 1, 4, 1, 99999}) {
		t.Error("the arc root itself is a RunOS OID")
	}
}

func TestL2SecServerCheckNeedsAPeerCertificate(t *testing.T) {
	if err := verifyL2SecServerLeaf(tls.ConnectionState{}); err == nil {
		t.Fatal("no peer certificate must be refused")
	}
}
