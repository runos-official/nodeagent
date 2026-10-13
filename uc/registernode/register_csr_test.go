package registernode

import (
	"context"
	"crypto/ecdsa"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"os"
	"path/filepath"
	"testing"

	pb "github.com/runos-official/nodeagent/l1sec"
	"github.com/runos-official/nodeagent/uc/csrcert/csrtest"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// fakeL1 answers Register; every other method panics through the nil embed.
type fakeL1 struct {
	pb.NodewardClient
	got     *pb.NodeRegistrationRequest
	respond func(*pb.NodeRegistrationRequest) (*pb.NodeRegistrationResponse, error)
}

func (f *fakeL1) Register(_ context.Context, in *pb.NodeRegistrationRequest, _ ...grpc.CallOption) (*pb.NodeRegistrationResponse, error) {
	f.got = in
	return f.respond(in)
}

func tempPaths(t *testing.T) CredPaths {
	dir := t.TempDir()
	return CredPaths{
		Cert: filepath.Join(dir, "mtls.crt"),
		Key:  filepath.Join(dir, "mtls.key"),
		CA:   filepath.Join(dir, "ca.crt"),
	}
}

func TestRegisterSendsCSRAndStoresOwnKey(t *testing.T) {
	ca := csrtest.NewCA(t)
	p := tempPaths(t)
	var csrKey *ecdsa.PublicKey
	f := &fakeL1{respond: func(in *pb.NodeRegistrationRequest) (*pb.NodeRegistrationResponse, error) {
		if in.CsrPem == "" {
			t.Fatal("request carries no CSR")
		}
		leaf, csr, err := ca.SignCSR(t, in.CsrPem)
		if err != nil {
			t.Fatalf("nodeward would refuse this CSR: %v", err)
		}
		csrKey = csr.PublicKey.(*ecdsa.PublicKey)
		return &pb.NodeRegistrationResponse{Nid: "n1", CertificatePem: leaf, CaCert: ca.PEM}, nil
	}}
	nid, err := registerAndStore(context.Background(), f, &pb.NodeRegistrationRequest{Token: "t"}, p)
	if err != nil {
		t.Fatal(err)
	}
	if nid != "n1" || f.got.Token != "t" {
		t.Fatalf("nid %q, token %q", nid, f.got.Token)
	}
	pair, err := tls.LoadX509KeyPair(p.Cert, p.Key)
	if err != nil {
		t.Fatalf("stored pair does not load: %v", err)
	}
	if !pair.PrivateKey.(*ecdsa.PrivateKey).PublicKey.Equal(csrKey) {
		t.Fatal("stored key is not the key the CSR was for")
	}
	info, _ := os.Stat(p.Key)
	if info.Mode().Perm() != 0600 {
		t.Fatalf("key mode = %v", info.Mode().Perm())
	}
	data, _ := os.ReadFile(p.Key)
	if b, _ := pem.Decode(data); b == nil || b.Type != "PRIVATE KEY" {
		t.Fatal("key is not PKCS#8")
	}
	caData, _ := os.ReadFile(p.CA)
	if string(caData) != ca.PEM {
		t.Fatal("CA not stored")
	}
}

func TestRegisterFallsBackToLegacyResponse(t *testing.T) {
	ca := csrtest.NewCA(t)
	p := tempPaths(t)
	keyPEM, certPEM := ca.LegacyPair(t)
	f := &fakeL1{respond: func(*pb.NodeRegistrationRequest) (*pb.NodeRegistrationResponse, error) {
		// An old nodeward ignores csrPem and returns a key.
		return &pb.NodeRegistrationResponse{Nid: "n1", PrivateKey: keyPEM, PublicKey: certPEM, CaCert: ca.PEM}, nil
	}}
	if _, err := registerAndStore(context.Background(), f, &pb.NodeRegistrationRequest{}, p); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(p.Key)
	b, _ := pem.Decode(data)
	if b == nil || b.Type != "RSA PRIVATE KEY" {
		t.Fatal("legacy key not stored as today")
	}
	if _, err := x509.ParsePKCS1PrivateKey(b.Bytes); err != nil {
		t.Fatal(err)
	}
	if _, err := tls.LoadX509KeyPair(p.Cert, p.Key); err != nil {
		t.Fatal(err)
	}
}

func TestRegisterRefusesUnusableResponsesWithoutWriting(t *testing.T) {
	ca := csrtest.NewCA(t)
	other := csrtest.NewCA(t)
	_, foreignLeaf := ca.LegacyPair(t)
	cases := map[string]func(*pb.NodeRegistrationRequest) *pb.NodeRegistrationResponse{
		"both empty": func(*pb.NodeRegistrationRequest) *pb.NodeRegistrationResponse {
			return &pb.NodeRegistrationResponse{Nid: "n1", CaCert: ca.PEM}
		},
		"foreign key": func(*pb.NodeRegistrationRequest) *pb.NodeRegistrationResponse {
			return &pb.NodeRegistrationResponse{Nid: "n1", CertificatePem: foreignLeaf, CaCert: ca.PEM}
		},
		"wrong CA": func(in *pb.NodeRegistrationRequest) *pb.NodeRegistrationResponse {
			leaf, _, _ := ca.SignCSR(t, in.CsrPem)
			return &pb.NodeRegistrationResponse{Nid: "n1", CertificatePem: leaf, CaCert: other.PEM}
		},
	}
	for name, mk := range cases {
		t.Run(name, func(t *testing.T) {
			p := tempPaths(t)
			for _, path := range []string{p.Cert, p.Key, p.CA} {
				_ = os.WriteFile(path, []byte("old"), 0600)
			}
			f := &fakeL1{respond: func(in *pb.NodeRegistrationRequest) (*pb.NodeRegistrationResponse, error) {
				return mk(in), nil
			}}
			if _, err := registerAndStore(context.Background(), f, &pb.NodeRegistrationRequest{}, p); err == nil {
				t.Fatal("expected an error")
			}
			for _, path := range []string{p.Cert, p.Key, p.CA} {
				if data, _ := os.ReadFile(path); string(data) != "old" {
					t.Fatalf("%s was changed", filepath.Base(path))
				}
			}
		})
	}
}

func TestRegisterRPCErrorKeepsItsStatus(t *testing.T) {
	p := tempPaths(t)
	f := &fakeL1{respond: func(*pb.NodeRegistrationRequest) (*pb.NodeRegistrationResponse, error) {
		return nil, status.Error(codes.PermissionDenied, "token used")
	}}
	_, err := registerAndStore(context.Background(), f, &pb.NodeRegistrationRequest{}, p)
	var rpc *rpcError
	if !errors.As(err, &rpc) {
		t.Fatalf("want an rpcError, got %T %v", err, err)
	}
	if status.Code(rpc.err) != codes.PermissionDenied {
		t.Fatalf("status lost: %v", rpc.err)
	}
}
