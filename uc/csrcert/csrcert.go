// Package csrcert holds the agent side of CSR certificate issuance (FPL315).
//
// The agent makes its own ECDSA P-256 key, sends only a certificate signing
// request, and nodeward answers with the certificate. The private key never
// leaves the node. On the L2SEC minting methods the agent also proves that it
// holds the key of the leaf it presents (MintProof).
//
// Rules from the contract: never log a key or a CSR; fall back to the legacy
// key-returning response only when a successful response carries no
// certificate; never fall back because of an error.
package csrcert

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/runos-official/nodeagent/l2sec"
	"github.com/runos-official/nodeagent/roslog"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const (
	// ProofLabel is the first line of the proof signing input.
	ProofLabel = "runos-mint-proof-v1"
	// nonceLen is the exact proof nonce size the contract requires.
	nonceLen = 32
	// skewPrefix starts nodeward's clock-skew refusal text.
	skewPrefix = "mint proof time is outside the 5 minute window"
	// notBeforeSkew is how far a new leaf's NotBefore may sit ahead of the
	// local clock. Nodeward sets NotBefore to its own now.
	notBeforeSkew = 10 * time.Minute
)

// Key is a fresh agent key and the CSR for it. Make a new Key per mint call.
type Key struct {
	priv   *ecdsa.PrivateKey
	csrDER []byte
	csrPEM string
}

// NewKey makes a P-256 key in memory and a CSR for it. Nodeward ignores the
// CSR subject, so it is a fixed placeholder.
func NewKey() (*Key, error) {
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generate key: %w", err)
	}
	tmpl := &x509.CertificateRequest{
		Subject:            pkix.Name{CommonName: "runos-agent"},
		SignatureAlgorithm: x509.ECDSAWithSHA256,
	}
	der, err := x509.CreateCertificateRequest(rand.Reader, tmpl, priv)
	if err != nil {
		return nil, fmt.Errorf("create certificate signing request: %w", err)
	}
	return &Key{
		priv:   priv,
		csrDER: der,
		csrPEM: string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: der})),
	}, nil
}

// CSRPEM is the CSR to send. It holds public material only.
func (k *Key) CSRPEM() string { return k.csrPEM }

// CSRDER is the canonical request bytes the proof hashes.
func (k *Key) CSRDER() []byte { return k.csrDER }

// Public is the public half of the key.
func (k *Key) Public() *ecdsa.PublicKey { return &k.priv.PublicKey }

// Fingerprint is a short SHA-256 prefix of the public key, safe for logs.
func (k *Key) Fingerprint() string {
	der, err := x509.MarshalPKIXPublicKey(k.Public())
	if err != nil {
		return "unknown"
	}
	sum := sha256.Sum256(der)
	return hex.EncodeToString(sum[:])[:12]
}

// SigningInput builds the six-line proof input from the contract, joined by
// "\n" with no trailing newline.
func SigningInput(method string, csrDER []byte, unixSeconds int64, nonce, leafDER []byte) []byte {
	csrSum := sha256.Sum256(csrDER)
	leafSum := sha256.Sum256(leafDER)
	return []byte(strings.Join([]string{
		ProofLabel,
		method,
		hex.EncodeToString(csrSum[:]),
		strconv.FormatInt(unixSeconds, 10),
		hex.EncodeToString(nonce),
		hex.EncodeToString(leafSum[:]),
	}, "\n"))
}

// NewProof signs a proof with a new random nonce. signer is the private key of
// the leaf the call presents; leafDER is that leaf.
func NewProof(signer crypto.Signer, leafDER []byte, method string, csrDER []byte, at time.Time) (*l2sec.MintProof, error) {
	nonce := make([]byte, nonceLen)
	if _, err := rand.Read(nonce); err != nil {
		return nil, fmt.Errorf("make proof nonce: %w", err)
	}
	unix := at.Unix()
	digest := sha256.Sum256(SigningInput(method, csrDER, unix, nonce, leafDER))

	var opts crypto.SignerOpts
	switch signer.Public().(type) {
	case *ecdsa.PublicKey:
		// crypto.Signer on an ECDSA key gives an ASN.1 DER signature.
		opts = crypto.SHA256
	case *rsa.PublicKey:
		opts = &rsa.PSSOptions{SaltLength: rsa.PSSSaltLengthEqualsHash, Hash: crypto.SHA256}
	default:
		return nil, fmt.Errorf("leaf key type %T cannot sign a mint proof", signer.Public())
	}
	sig, err := signer.Sign(rand.Reader, digest[:], opts)
	if err != nil {
		return nil, fmt.Errorf("sign mint proof: %w", err)
	}
	return &l2sec.MintProof{UnixSeconds: unix, Nonce: nonce, Signature: sig}, nil
}

// ServerTimeFromSkew reads the server time from nodeward's clock-skew refusal.
func ServerTimeFromSkew(err error) (int64, bool) {
	if err == nil {
		return 0, false
	}
	st, ok := status.FromError(err)
	if !ok || st.Code() != codes.FailedPrecondition || !strings.HasPrefix(st.Message(), skewPrefix) {
		return 0, false
	}
	const marker = "server time "
	i := strings.LastIndex(st.Message(), marker)
	if i < 0 {
		return 0, false
	}
	ts, perr := strconv.ParseInt(strings.TrimSpace(st.Message()[i+len(marker):]), 10, 64)
	if perr != nil || ts <= 0 {
		return 0, false
	}
	return ts, true
}

// CallWithProof runs call with a fresh proof. On a clock-skew refusal it
// retries once with a new nonce and the server's clock. Any other error is
// returned as is, so a refusal never downgrades the agent.
func CallWithProof(signer crypto.Signer, leafDER []byte, method string, k *Key, now func() time.Time, call func(*l2sec.MintProof) error) error {
	proof, err := NewProof(signer, leafDER, method, k.CSRDER(), now())
	if err != nil {
		return err
	}
	err = call(proof)
	serverUnix, skewed := ServerTimeFromSkew(err)
	if !skewed {
		return err
	}
	offset := time.Unix(serverUnix, 0).Sub(now())
	roslog.W("Mint proof refused for clock skew, retrying once with the server time", nil,
		"offset_seconds", int64(offset.Seconds()))
	proof, perr := NewProof(signer, leafDER, method, k.CSRDER(), now().Add(offset))
	if perr != nil {
		return perr
	}
	return call(proof)
}

// ErrUnusableResponse marks a minting response the agent refused. The caller
// keeps its old credentials.
var ErrUnusableResponse = errors.New("unusable minting response")

// Outcome says which path a minting response took.
type Outcome int

const (
	// OutcomeCertificate means nodeward signed the CSR: store the local key.
	OutcomeCertificate Outcome = iota + 1
	// OutcomeLegacy means an old nodeward returned a key: store it as today.
	OutcomeLegacy
)

func (o Outcome) String() string {
	switch o {
	case OutcomeCertificate:
		return "certificate"
	case OutcomeLegacy:
		return "legacy"
	}
	return "none"
}

// Response is the minting fields every minting response carries.
type Response struct {
	CertificatePEM string
	CACertPEM      string
	PrivateKey     string
	PublicKey      string
}

// Resolve decides the path by the response content only. On the certificate
// path it checks the certificate against the local key and the CA. Any check
// failure is an error, and the caller keeps its old credentials.
func Resolve(k *Key, r Response, now time.Time) (Outcome, error) {
	if r.CertificatePEM == "" {
		if r.PrivateKey != "" && r.PublicKey != "" {
			roslog.W("nodeward returned a legacy key; CSR not supported", nil)
			return OutcomeLegacy, nil
		}
		return 0, fmt.Errorf("%w: nodeward returned neither a certificate nor a key", ErrUnusableResponse)
	}
	if r.PrivateKey != "" || r.PublicKey != "" {
		// Contract: nodeward never sets both. Use our own key and say so.
		roslog.W("nodeward returned legacy key fields with a certificate; ignoring them", nil)
	}
	if err := checkCertificate(k, r.CertificatePEM, r.CACertPEM, now); err != nil {
		return 0, fmt.Errorf("%w: issued certificate refused: %v", ErrUnusableResponse, err)
	}
	roslog.I("Certificate issued for the local key", "key_sha256", k.Fingerprint())
	return OutcomeCertificate, nil
}

func checkCertificate(k *Key, certPEM, caPEM string, now time.Time) error {
	block, rest := pem.Decode([]byte(certPEM))
	if block == nil || block.Type != "CERTIFICATE" {
		return errors.New("not a CERTIFICATE PEM block")
	}
	if len(strings.TrimSpace(string(rest))) != 0 {
		return errors.New("more than one PEM block")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return fmt.Errorf("parse: %w", err)
	}
	pub, ok := cert.PublicKey.(*ecdsa.PublicKey)
	if !ok || !pub.Equal(k.Public()) {
		return errors.New("public key does not match the local key")
	}
	hasClientAuth := false
	for _, u := range cert.ExtKeyUsage {
		if u == x509.ExtKeyUsageClientAuth {
			hasClientAuth = true
		}
	}
	if !hasClientAuth {
		return errors.New("no client authentication usage")
	}
	if !cert.NotAfter.After(now) {
		return errors.New("already expired")
	}
	at := now
	if cert.NotBefore.After(now) {
		if cert.NotBefore.Sub(now) > notBeforeSkew {
			return errors.New("not valid until well in the future")
		}
		at = cert.NotBefore
	}
	roots := x509.NewCertPool()
	if caPEM == "" || !roots.AppendCertsFromPEM([]byte(caPEM)) {
		return errors.New("no usable CA certificate in the response")
	}
	if _, err := cert.Verify(x509.VerifyOptions{
		Roots:       roots,
		CurrentTime: at,
		KeyUsages:   []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}); err != nil {
		return fmt.Errorf("does not verify against the CA: %w", err)
	}
	return nil
}

// WriteKeyFile stores the key as PKCS#8 with mode 0600. It writes a temp file
// in the same directory and renames it, so a crash never leaves half a key.
func WriteKeyFile(k *Key, path string) error {
	der, err := x509.MarshalPKCS8PrivateKey(k.priv)
	if err != nil {
		return fmt.Errorf("encode key: %w", err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".mtls-key-*")
	if err != nil {
		return fmt.Errorf("create temp key file: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op after a successful rename
	if err := tmp.Chmod(0600); err != nil {
		tmp.Close()
		return fmt.Errorf("set key file mode: %w", err)
	}
	if err := pem.Encode(tmp, &pem.Block{Type: "PRIVATE KEY", Bytes: der}); err != nil {
		tmp.Close()
		return fmt.Errorf("write key: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("sync key: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close key file: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("move key into place: %w", err)
	}
	roslog.I("Private key written", "path", path, "key_sha256", k.Fingerprint())
	return nil
}
