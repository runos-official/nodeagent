package backend_test

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/runos-official/nodeagent/backend"
	"github.com/runos-official/nodeagent/l2sec"
	"github.com/runos-official/nodeagent/uc/certificate"
	"github.com/runos-official/nodeagent/uc/certstore"
	"github.com/runos-official/nodeagent/uc/csrcert/csrtest"
)

var leafShape = csrtest.LeafOptions{DNSNames: []string{"acct-a.example.test"}, Version: 2, Type: 1}

// signer is a fake Nodeward that signs each CSR.
type signer struct {
	l2sec.NodewardClient
	t      *testing.T
	ca     *csrtest.CA
	issued string
}

func (s *signer) RegenerateCertificate(_ context.Context, in *l2sec.RegenerateCertificateRequest, _ ...grpc.CallOption) (*l2sec.RegenerateCertificateResponse, error) {
	b, _ := pem.Decode([]byte(in.CsrPem))
	if b == nil {
		return nil, status.Error(codes.InvalidArgument, "no CSR")
	}
	csr, err := x509.ParseCertificateRequest(b.Bytes)
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	s.issued = s.ca.LeafWith(s.t, csr.PublicKey, leafShape)
	return &l2sec.RegenerateCertificateResponse{CertificatePem: s.issued, CaCert: s.ca.PEM}, nil
}

// TestNextDialUsesRenewedCertificate: after a renewal through the one
// renewal function, the next L2Sec dial loads the child from disk. The
// agent forces no reconnect; this is how a new dial moves to the child.
func TestNextDialUsesRenewedCertificate(t *testing.T) {
	ca := csrtest.NewCA(t)
	dir := t.TempDir()
	p := certstore.Paths{Cert: filepath.Join(dir, "mtls.crt"), Key: filepath.Join(dir, "mtls.key"), CA: filepath.Join(dir, "ca.crt")}
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, _ := x509.MarshalPKCS8PrivateKey(k)
	for _, w := range []struct {
		path string
		data []byte
		mode os.FileMode
	}{
		{p.Key, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), 0o600},
		{p.Cert, []byte(ca.LeafWith(t, k.Public(), leafShape)), 0o644},
		{p.CA, []byte(ca.PEM), 0o644},
	} {
		if err := os.WriteFile(w.path, w.data, w.mode); err != nil {
			t.Fatal(err)
		}
	}
	serialOf := func() string {
		cfg, err := backend.LoadL2SecClientTLS(p.Cert, p.Key, p.CA, "nodeward.example")
		if err != nil {
			t.Fatal(err)
		}
		leaf, err := x509.ParseCertificate(cfg.Certificates[0].Certificate[0])
		if err != nil {
			t.Fatal(err)
		}
		return fmt.Sprintf("%x", leaf.SerialNumber)
	}
	parent := serialOf()

	s := &signer{t: t, ca: ca}
	res, err := certificate.Renew(context.Background(), certificate.Options{
		Event: "CertificateAutoRenewal", Message: "test", ExpectedSerial: parent, Paths: p,
		Dial:    func(context.Context) (certificate.MintClient, func(), error) { return s, func() {}, nil },
		NodeLog: func(string, string) error { return nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := serialOf(); got == parent || got != res.Serial {
		t.Fatalf("the next dial loads serial %s, want the child %s (parent %s)", got, res.Serial, parent)
	}
}
