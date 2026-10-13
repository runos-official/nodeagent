package certificate

import (
	"bytes"
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/runos-official/nodeagent/l2sec"
	"github.com/runos-official/nodeagent/roslog"
	"github.com/runos-official/nodeagent/uc/csrcert"
	"github.com/runos-official/nodeagent/uc/csrcert/csrtest"
	"github.com/runos-official/nodeagent/uc/registernode"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/emptypb"
)

// fakeL2 answers RegenerateCertificate; every other method panics.
type fakeL2 struct {
	l2sec.NodewardClient
	calls   []*l2sec.RegenerateCertificateRequest
	respond func(*l2sec.RegenerateCertificateRequest) (*l2sec.RegenerateCertificateResponse, error)
}

func (f *fakeL2) RegenerateCertificate(_ context.Context, in *l2sec.RegenerateCertificateRequest, _ ...grpc.CallOption) (*l2sec.RegenerateCertificateResponse, error) {
	f.calls = append(f.calls, in)
	return f.respond(in)
}

// currentCreds writes today's on-disk credentials: an RSA PKCS#1 key, as an
// old nodeward issued them.
func currentCreds(t *testing.T, ca *csrtest.CA) registernode.CredPaths {
	t.Helper()
	dir := t.TempDir()
	p := registernode.CredPaths{
		Cert: filepath.Join(dir, "mtls.crt"),
		Key:  filepath.Join(dir, "mtls.key"),
		CA:   filepath.Join(dir, "ca.crt"),
	}
	keyPEM, certPEM := ca.LegacyPair(t)
	mustWrite(t, p.Key, keyPEM, 0600)
	mustWrite(t, p.Cert, certPEM, 0644)
	mustWrite(t, p.CA, ca.PEM, 0644)
	return p
}

func mustWrite(t *testing.T, path, data string, mode os.FileMode) {
	t.Helper()
	if err := os.WriteFile(path, []byte(data), mode); err != nil {
		t.Fatal(err)
	}
}

// verifyProof checks a proof the way nodeward does (contract section 5).
func verifyProof(t *testing.T, p registernode.CredPaths, in *l2sec.RegenerateCertificateRequest) error {
	t.Helper()
	pair, err := tls.LoadX509KeyPair(p.Cert, p.Key)
	if err != nil {
		return err
	}
	leafDER := pair.Certificate[0]
	leaf, _ := x509.ParseCertificate(leafDER)
	if in.Proof == nil || len(in.Proof.Nonce) != 32 || len(in.Proof.Signature) == 0 {
		return errors.New("mint proof is missing or malformed")
	}
	block, _ := pem.Decode([]byte(in.CsrPem))
	if block == nil {
		return errors.New("no CSR")
	}
	input := csrcert.SigningInput(l2sec.Nodeward_RegenerateCertificate_FullMethodName, block.Bytes,
		in.Proof.UnixSeconds, in.Proof.Nonce, leafDER)
	digest := sha256.Sum256(input)
	switch pub := leaf.PublicKey.(type) {
	case *ecdsa.PublicKey:
		if !ecdsa.VerifyASN1(pub, digest[:], in.Proof.Signature) {
			return errors.New("ecdsa proof does not verify")
		}
	case *rsa.PublicKey:
		if err := rsa.VerifyPSS(pub, crypto.SHA256, digest[:], in.Proof.Signature,
			&rsa.PSSOptions{SaltLength: rsa.PSSSaltLengthEqualsHash, Hash: crypto.SHA256}); err != nil {
			return err
		}
	default:
		return fmt.Errorf("leaf key %T", leaf.PublicKey)
	}
	csr, err := x509.ParseCertificateRequest(block.Bytes)
	if err != nil {
		return err
	}
	if csr.PublicKey.(interface{ Equal(crypto.PublicKey) bool }).Equal(leaf.PublicKey) {
		return errors.New("CSR reuses the leaf key")
	}
	return nil
}

// csrNodeward signs CSRs after checking the proof, like a new nodeward.
func csrNodeward(t *testing.T, ca *csrtest.CA, p registernode.CredPaths) func(*l2sec.RegenerateCertificateRequest) (*l2sec.RegenerateCertificateResponse, error) {
	return func(in *l2sec.RegenerateCertificateRequest) (*l2sec.RegenerateCertificateResponse, error) {
		if in.CsrPem == "" {
			t.Fatal("renewal sent no CSR")
		}
		if err := verifyProof(t, p, in); err != nil {
			return nil, status.Error(codes.PermissionDenied, err.Error())
		}
		leaf, _, err := ca.SignCSR(t, in.CsrPem)
		if err != nil {
			return nil, status.Error(codes.InvalidArgument, err.Error())
		}
		return &l2sec.RegenerateCertificateResponse{CertificatePem: leaf, CaCert: ca.PEM}, nil
	}
}

func renewOnce(t *testing.T, c l2sec.NodewardClient, p registernode.CredPaths) (*issued, error) {
	t.Helper()
	got, err := requestCertificate(context.Background(), c, p, time.Now)
	if err != nil {
		return nil, err
	}
	return got, writeCertificates(got, p)
}

func TestRenewSendsCSRAndStoresOwnKey(t *testing.T) {
	ca := csrtest.NewCA(t)
	p := currentCreds(t, ca)
	f := &fakeL2{respond: csrNodeward(t, ca, p)}

	// First renewal: the proof is signed with today's RSA key.
	got, err := renewOnce(t, f, p)
	if err != nil {
		t.Fatal(err)
	}
	if got.outcome != csrcert.OutcomeCertificate {
		t.Fatalf("outcome = %v", got.outcome)
	}
	pair, err := tls.LoadX509KeyPair(p.Cert, p.Key)
	if err != nil {
		t.Fatalf("new pair does not load: %v", err)
	}
	if _, ok := pair.PrivateKey.(*ecdsa.PrivateKey); !ok {
		t.Fatalf("stored key is %T, want ECDSA", pair.PrivateKey)
	}
	if info, _ := os.Stat(p.Key); info.Mode().Perm() != 0600 {
		t.Fatalf("key mode = %v", info.Mode().Perm())
	}

	// Second renewal: the proof is signed with the ECDSA key just stored.
	if _, err := renewOnce(t, f, p); err != nil {
		t.Fatalf("renewal from an ECDSA leaf: %v", err)
	}
	if len(f.calls) != 2 {
		t.Fatalf("calls = %d", len(f.calls))
	}
}

func TestRenewFallsBackToLegacyResponse(t *testing.T) {
	ca := csrtest.NewCA(t)
	p := currentCreds(t, ca)
	keyPEM, certPEM := ca.LegacyPair(t)
	f := &fakeL2{respond: func(*l2sec.RegenerateCertificateRequest) (*l2sec.RegenerateCertificateResponse, error) {
		return &l2sec.RegenerateCertificateResponse{PrivateKey: keyPEM, PublicKey: certPEM, CaCert: ca.PEM}, nil
	}}
	got, err := renewOnce(t, f, p)
	if err != nil {
		t.Fatal(err)
	}
	if got.outcome != csrcert.OutcomeLegacy {
		t.Fatalf("outcome = %v", got.outcome)
	}
	if data, _ := os.ReadFile(p.Cert); string(data) != certPEM {
		t.Fatal("legacy certificate not stored")
	}
	if _, err := tls.LoadX509KeyPair(p.Cert, p.Key); err != nil {
		t.Fatal(err)
	}
}

func TestRenewErrorNeverFallsBack(t *testing.T) {
	ca := csrtest.NewCA(t)
	p := currentCreds(t, ca)
	before, _ := os.ReadFile(p.Key)
	f := &fakeL2{respond: func(*l2sec.RegenerateCertificateRequest) (*l2sec.RegenerateCertificateResponse, error) {
		return nil, status.Error(codes.FailedPrecondition, "this agent must renew with a certificate signing request")
	}}
	if _, err := requestCertificate(context.Background(), f, p, time.Now); err == nil {
		t.Fatal("expected an error")
	}
	if len(f.calls) != 1 || f.calls[0].CsrPem == "" {
		t.Fatalf("want exactly one CSR call, got %d", len(f.calls))
	}
	if after, _ := os.ReadFile(p.Key); string(after) != string(before) {
		t.Fatal("key changed after a refused renewal")
	}
}

func TestRenewRefusesUnusableResponse(t *testing.T) {
	ca := csrtest.NewCA(t)
	p := currentCreds(t, ca)
	_, foreignLeaf := ca.LegacyPair(t)
	f := &fakeL2{respond: func(*l2sec.RegenerateCertificateRequest) (*l2sec.RegenerateCertificateResponse, error) {
		return &l2sec.RegenerateCertificateResponse{CertificatePem: foreignLeaf, CaCert: ca.PEM}, nil
	}}
	_, err := requestCertificate(context.Background(), f, p, time.Now)
	if !errors.Is(err, csrcert.ErrUnusableResponse) {
		t.Fatalf("want ErrUnusableResponse, got %v", err)
	}
}

// The fake nodeward runs on its own clock and stamps NotBefore with it, as the
// real one does. After the skew retry the agent must store the new leaf, since
// nodeward has already put the old one on its grace revocation.
func TestRenewRetriesOnceOnClockSkew(t *testing.T) {
	for _, ahead := range []time.Duration{12 * time.Minute, 20 * time.Minute} {
		t.Run(ahead.String(), func(t *testing.T) {
			ca := csrtest.NewCA(t)
			p := currentCreds(t, ca)
			serverClock := func() time.Time { return time.Now().Add(ahead) }
			ca.ServerNow = serverClock
			sign := csrNodeward(t, ca, p)
			f := &fakeL2{}
			f.respond = func(in *l2sec.RegenerateCertificateRequest) (*l2sec.RegenerateCertificateResponse, error) {
				serverNow := serverClock().Unix()
				if d := in.Proof.UnixSeconds - serverNow; d > 300 || d < -300 {
					return nil, status.Error(codes.FailedPrecondition,
						fmt.Sprintf("mint proof time is outside the 5 minute window; server time %d", serverNow))
				}
				return sign(in)
			}
			if _, err := renewOnce(t, f, p); err != nil {
				t.Fatal(err)
			}
			if len(f.calls) != 2 {
				t.Fatalf("calls = %d, want 2", len(f.calls))
			}
			if f.calls[0].CsrPem != f.calls[1].CsrPem {
				t.Error("retry changed the CSR")
			}
			pair, err := tls.LoadX509KeyPair(p.Cert, p.Key)
			if err != nil {
				t.Fatalf("stored pair does not load: %v", err)
			}
			if _, ok := pair.PrivateKey.(*ecdsa.PrivateKey); !ok {
				t.Fatalf("stored key is %T, want the agent's own ECDSA key", pair.PrivateKey)
			}
			leaf, _ := x509.ParseCertificate(pair.Certificate[0])
			if !leaf.NotBefore.After(time.Now().Add(ahead - time.Minute)) {
				t.Fatalf("leaf NotBefore %v is not on the server clock", leaf.NotBefore)
			}
		})
	}
}

// An old nodeward or nodehub decodes the request with the old, empty message
// and encodes it again. The new fields must survive as unknown fields.
func TestRequestFieldsSurviveOldProtoRoundTrip(t *testing.T) {
	in := &l2sec.RegenerateCertificateRequest{
		CsrPem: "csr",
		Proof:  &l2sec.MintProof{UnixSeconds: 7, Nonce: make([]byte, 32), Signature: []byte{1}},
	}
	wire, err := proto.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	var old emptypb.Empty
	if err := proto.Unmarshal(wire, &old); err != nil {
		t.Fatal(err)
	}
	again, err := proto.Marshal(&old)
	if err != nil {
		t.Fatal(err)
	}
	var out l2sec.RegenerateCertificateRequest
	if err := proto.Unmarshal(again, &out); err != nil {
		t.Fatal(err)
	}
	if !proto.Equal(in, &out) {
		t.Fatalf("fields lost: %v", &out)
	}

	// A legacy response decodes into the new type with no certificate.
	legacy, _ := proto.Marshal(&l2sec.RegenerateCertificateResponse{PrivateKey: "k", PublicKey: "c", CaCert: "a"})
	var resp l2sec.RegenerateCertificateResponse
	if err := proto.Unmarshal(legacy, &resp); err != nil || resp.CertificatePem != "" {
		t.Fatalf("legacy response: %v %q", err, resp.CertificatePem)
	}
}

func TestRenewLogsNoKeyMaterial(t *testing.T) {
	var buf bytes.Buffer
	old := roslog.L
	roslog.L = slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	defer func() { roslog.L = old }()

	ca := csrtest.NewCA(t)
	p := currentCreds(t, ca)
	f := &fakeL2{respond: csrNodeward(t, ca, p)}
	if _, err := renewOnce(t, f, p); err != nil {
		t.Fatal(err)
	}
	keyPEM, certPEM := ca.LegacyPair(t)
	f.respond = func(*l2sec.RegenerateCertificateRequest) (*l2sec.RegenerateCertificateResponse, error) {
		return &l2sec.RegenerateCertificateResponse{PrivateKey: keyPEM, PublicKey: certPEM, CaCert: ca.PEM}, nil
	}
	if _, err := renewOnce(t, f, p); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if out == "" {
		t.Fatal("expected log output")
	}
	for _, bad := range []string{"PRIVATE KEY", "CERTIFICATE REQUEST", f.calls[0].CsrPem} {
		if strings.Contains(out, bad) {
			t.Fatalf("log contains %q", bad)
		}
	}
}
