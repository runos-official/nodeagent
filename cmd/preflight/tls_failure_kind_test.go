package preflight

import (
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"testing"
)

// MEASURED on a lab box 2026-08-22. The first join attempt was BLOCKED by nodeward-tls-pin with
// "the secure handshake FAILED: read tcp 198.51.100.226:52618->203.0.113.98:9191: i/o timeout",
// and told the operator it indicated "a TLS-intercepting proxy or a network MITM" with a remedy of
// exempting hosts from TLS inspection. Verified by hand seconds later: TCP connect succeeded and an
// openssl s_client handshake CONNECTED and returned the chain. The identical command then succeeded
// on retry with nothing changed.
//
// An i/o timeout is a TRANSPORT failure. The offered remedy cannot fix one, so the message sends
// the operator hunting for a proxy that does not exist.
func TestIsTlsTransportFailure_TimeoutIsTransportNotInterception(t *testing.T) {
	// exactly the shape the failing box produced
	err := fmt.Errorf("read tcp 198.51.100.226:52618->203.0.113.98:9191: %w", os.ErrDeadlineExceeded)
	if !isTlsTransportFailure(err) {
		t.Fatal("an i/o timeout must be classed as a transport failure, not as interception")
	}
}

func TestIsTlsTransportFailure_ResetAndEofAreTransport(t *testing.T) {
	for _, err := range []error{
		io.EOF,
		io.ErrUnexpectedEOF,
		&net.OpError{Op: "read", Err: errors.New("connection reset by peer")},
	} {
		if !isTlsTransportFailure(err) {
			t.Errorf("%v must be classed as a transport failure", err)
		}
	}
}

// The check must keep blocking on a genuine trust failure: that is the whole reason it exists.
func TestIsTlsTransportFailure_CertificateErrorsAreNotTransport(t *testing.T) {
	for _, err := range []error{
		x509.UnknownAuthorityError{},
		x509.HostnameError{Host: "nodeward.dev.runos.com"},
		x509.CertificateInvalidError{Reason: x509.Expired},
	} {
		if isTlsTransportFailure(err) {
			t.Errorf("%T is a trust failure and must still block", err)
		}
	}
}
