package certstore

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"encoding/asn1"
	"encoding/pem"
	"errors"
	"fmt"
	"slices"
	"time"
)

// Nodeward agent leaf extensions (Nodeward uc/cert/version.go). Each value
// is one raw byte.
var (
	versionOID = asn1.ObjectIdentifier{1, 3, 6, 1, 4, 1, 99999, 1, 1}
	typeOID    = asn1.ObjectIdentifier{1, 3, 6, 1, 4, 1, 99999, 1, 2}
)

// versionV2 is the only version extension value that the store accepts.
const versionV2 = 2

// Reason words of a rejected child.
const (
	ReasonMalformed       = "malformed"
	ReasonKeyMismatch     = "key_mismatch"
	ReasonExpired         = "expired"
	ReasonNoClientAuth    = "no_client_auth"
	ReasonChainInvalid    = "chain_invalid"
	ReasonAccountMismatch = "account_mismatch"
	ReasonTypeMismatch    = "type_mismatch"
	ReasonVersionMismatch = "version_mismatch"
)

// RejectError is a child that failed a local check. errors.Is matches it
// to ErrChildRejected. Reason is one reason word; Detail never holds PEM.
type RejectError struct {
	Reason string
	Detail string
}

func (e *RejectError) Error() string {
	if e.Detail == "" {
		return fmt.Sprintf("%s: %s", ErrChildRejected, e.Reason)
	}
	return fmt.Sprintf("%s: %s: %s", ErrChildRejected, e.Reason, e.Detail)
}

// Is makes errors.Is(err, ErrChildRejected) true.
func (e *RejectError) Is(target error) bool { return target == ErrChildRejected }

// RejectReason returns the reason word of a rejected child, or "".
func RejectReason(err error) string {
	var r *RejectError
	if errors.As(err, &r) {
		return r.Reason
	}
	return ""
}

func reject(reason, format string, args ...any) *RejectError {
	return &RejectError{Reason: reason, Detail: fmt.Sprintf(format, args...)}
}

// parseOneCert parses a PEM file that must hold exactly one CERTIFICATE
// block and nothing else but white space.
func parseOneCert(b []byte) (*x509.Certificate, error) {
	block, rest := pem.Decode(b)
	if block == nil || block.Type != "CERTIFICATE" {
		return nil, errors.New("no CERTIFICATE block")
	}
	if len(bytes.TrimSpace(rest)) != 0 {
		return nil, errors.New("more than one PEM block")
	}
	return x509.ParseCertificate(block.Bytes)
}

// checkChild runs the local checks of a staged child against the parent
// certificate. now is the clock of the store. It returns the parsed child
// or a *RejectError.
func checkChild(m Material, parent *x509.Certificate, now time.Time) (*x509.Certificate, error) {
	child, err := parseOneCert(m.CertPEM)
	if err != nil {
		return nil, reject(ReasonMalformed, "certificate: %v", err)
	}
	if kb, _ := pem.Decode(m.KeyPEM); kb == nil {
		return nil, reject(ReasonMalformed, "key: no PEM block")
	}
	if _, err := tls.X509KeyPair(m.CertPEM, m.KeyPEM); err != nil {
		return nil, reject(ReasonKeyMismatch, "%v", err)
	}
	at := now
	if child.NotBefore.After(at) {
		at = child.NotBefore
	}
	if at.After(child.NotAfter) {
		return nil, reject(ReasonExpired, "not after %s", child.NotAfter.UTC().Format(time.RFC3339))
	}
	if !slices.Contains(child.ExtKeyUsage, x509.ExtKeyUsageClientAuth) &&
		!slices.Contains(child.ExtKeyUsage, x509.ExtKeyUsageAny) {
		return nil, reject(ReasonNoClientAuth, "")
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(m.CAPEM) {
		return nil, reject(ReasonChainInvalid, "CA file holds no certificate")
	}
	if _, err := child.Verify(x509.VerifyOptions{
		Roots:       roots,
		CurrentTime: at,
		KeyUsages:   []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}); err != nil {
		return nil, reject(ReasonChainInvalid, "%v", err)
	}
	if len(parent.DNSNames) > 0 && !sameNames(parent.DNSNames, child.DNSNames) {
		return nil, reject(ReasonAccountMismatch, "DNS names differ from the held certificate")
	}
	if pt, ok := extValue(parent, typeOID); ok {
		if ct, ok := extValue(child, typeOID); !ok || !bytes.Equal(pt, ct) {
			return nil, reject(ReasonTypeMismatch, "type extension differs from the held certificate")
		}
	}
	if cv, ok := extValue(child, versionOID); ok && !bytes.Equal(cv, []byte{versionV2}) {
		return nil, reject(ReasonVersionMismatch, "version extension is not %d", versionV2)
	}
	return child, nil
}

// extValue returns the raw value of the extension oid.
func extValue(c *x509.Certificate, oid asn1.ObjectIdentifier) ([]byte, bool) {
	for _, e := range c.Extensions {
		if e.Id.Equal(oid) {
			return e.Value, true
		}
	}
	return nil, false
}

// sameNames compares two DNS name lists as sets.
func sameNames(a, b []string) bool {
	as, bs := slices.Clone(a), slices.Clone(b)
	slices.Sort(as)
	slices.Sort(bs)
	return slices.Equal(slices.Compact(as), slices.Compact(bs))
}

// serialText is the serial as lower-case hexadecimal text.
func serialText(c *x509.Certificate) string {
	return fmt.Sprintf("%x", c.SerialNumber)
}
