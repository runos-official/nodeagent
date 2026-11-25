package backend

import (
	"context"
	"errors"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"google.golang.org/grpc/credentials"
)

// CertificateExpiredError is the result of an L2Sec dial when the held client
// certificate has expired. The dial path returns it before any network call
// and writes no log line for it: the agent reports the expired state once,
// through its own reporter, and each caller decides what to print.
type CertificateExpiredError struct {
	// NotAfter is the expiry of the held certificate.
	NotAfter time.Time
	// Serial is the serial of the held certificate, lower-case hexadecimal.
	Serial string
}

func (e *CertificateExpiredError) Error() string {
	return "node agent certificate expired on " + e.NotAfter.UTC().Format(time.RFC3339)
}

// IsCertificateExpired reports whether err is or wraps a CertificateExpiredError.
func IsCertificateExpired(err error) bool {
	var e *CertificateExpiredError
	return errors.As(err, &e)
}

// AsCertificateExpired returns the CertificateExpiredError in err, or nil.
func AsCertificateExpired(err error) *CertificateExpiredError {
	var e *CertificateExpiredError
	if errors.As(err, &e) {
		return e
	}
	return nil
}

// ClientCertificateRefusedError is the result of an L2Sec dial that ended
// because the server refused the client certificate in the TLS handshake.
type ClientCertificateRefusedError struct {
	// Reason is the TLS alert text, for example "expired certificate".
	Reason string
	// Cause is the original dial error.
	Cause error
}

func (e *ClientCertificateRefusedError) Error() string {
	return "Nodeward refused the node agent certificate in the TLS handshake (" + e.Reason + "): " + e.Cause.Error()
}

func (e *ClientCertificateRefusedError) Unwrap() error { return e.Cause }

// IsClientCertificateRefused reports whether err is or wraps a
// ClientCertificateRefusedError.
func IsClientCertificateRefused(err error) bool {
	var e *ClientCertificateRefusedError
	return errors.As(err, &e)
}

// tlsAlertPrefix opens the text of a TLS alert that the peer sent. Go's
// crypto/tls formats a received alert as "remote error: tls: <alert>"
// (crypto/tls alert.go, the alert strings; measured through a gRPC dial, see
// classifyClientCertRefusal).
const tlsAlertPrefix = "remote error: tls: "

// clientCertAlerts are the TLS alerts by which a server refuses the client
// certificate. The texts are the alert strings of crypto/tls.
var clientCertAlerts = []string{
	"expired certificate",
	"bad certificate",
	"certificate revoked",
	"unknown certificate authority",
	"certificate required",
	"unsupported certificate",
	"unknown certificate",
}

// classifyClientCertRefusal returns a ClientCertificateRefusedError that wraps
// err when the text of err holds a TLS alert about the client certificate.
// Otherwise it returns err unchanged.
//
// The match is on text because gRPC flattens the transport error into a
// string ("connection error: desc = \"transport: authentication handshake
// failed: remote error: tls: ...\"" for TLS 1.2, and "... error reading
// server preface: remote error: tls: ..." for TLS 1.3). The texts were
// measured with a loopback gRPC server that requires a client certificate.
func classifyClientCertRefusal(err error) error {
	if err == nil || IsClientCertificateRefused(err) {
		return err
	}
	msg := err.Error()
	i := strings.Index(msg, tlsAlertPrefix)
	if i < 0 {
		return err
	}
	rest := msg[i+len(tlsAlertPrefix):]
	for _, a := range clientCertAlerts {
		if strings.HasPrefix(rest, a) {
			return &ClientCertificateRefusedError{Reason: a, Cause: err}
		}
	}
	return err
}

// withheldReason is the Reason of a refusal after the agent sent no client
// certificate because the server named no CA that issued the held one.
const withheldReason = "the server accepts no issuer of the node agent certificate"

// refusalWatch records the first TLS refusal of the client certificate that
// any connection attempt of one dial meets. On TLS 1.3 the server checks the
// client certificate after the client finished its handshake, so the alert
// arrives on the first read; a later attempt of the same dial can end with
// "connection reset by peer" instead (measured: 2 of 30 dials). The dial
// error is the error of the LAST attempt, so the watch keeps the alert.
type refusalWatch struct {
	mu     sync.Mutex
	reason string
}

func (w *refusalWatch) see(err error) {
	if err == nil {
		return
	}
	var r *ClientCertificateRefusedError
	if !errors.As(classifyClientCertRefusal(err), &r) {
		return
	}
	w.mu.Lock()
	if w.reason == "" {
		w.reason = r.Reason
	}
	w.mu.Unlock()
}

func (w *refusalWatch) seen() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.reason
}

// watchCreds wraps transport credentials so that every handshake error and
// every read or write error of the handshaken connection reaches w.
type watchCreds struct {
	credentials.TransportCredentials
	w *refusalWatch
}

func (c watchCreds) ClientHandshake(ctx context.Context, authority string, raw net.Conn) (net.Conn, credentials.AuthInfo, error) {
	conn, info, err := c.TransportCredentials.ClientHandshake(ctx, authority, raw)
	if err != nil {
		c.w.see(err)
		return conn, info, err
	}
	return watchConn{Conn: conn, w: c.w}, info, nil
}

func (c watchCreds) Clone() credentials.TransportCredentials {
	return watchCreds{TransportCredentials: c.TransportCredentials.Clone(), w: c.w}
}

type watchConn struct {
	net.Conn
	w *refusalWatch
}

func (c watchConn) Read(b []byte) (int, error) {
	n, err := c.Conn.Read(b)
	c.w.see(err)
	return n, err
}

func (c watchConn) Write(b []byte) (int, error) {
	n, err := c.Conn.Write(b)
	c.w.see(err)
	return n, err
}

// classifyDialRefusal is classifyClientCertRefusal plus the withheld case:
// when the dial sent no client certificate because the server accepts no
// issuer of the held one, a dial that failed with a TLS alert ("handshake
// failure" on TLS 1.2, "certificate required" on TLS 1.3) or with a broken
// connection after the handshake is a refusal of the certificate.
//
// seen is the reason of a refusal that an earlier attempt of the same dial
// met ("" for none); it types an error whose last attempt lost the alert.
func classifyDialRefusal(err error, withheld bool, seen string) error {
	if err == nil {
		return nil
	}
	if c := classifyClientCertRefusal(err); IsClientCertificateRefused(c) && !withheld {
		return c
	}
	if seen != "" && !withheld {
		return &ClientCertificateRefusedError{Reason: seen, Cause: err}
	}
	if withheld {
		return &ClientCertificateRefusedError{Reason: withheldReason, Cause: err}
	}
	return err
}

// dialAttempts counts L2Sec dials that passed the local certificate check and
// opened a network connection attempt. It is a test seam and changes no
// behaviour.
var dialAttempts atomic.Int64

// DialAttempts returns the number of L2Sec dials that passed the local
// certificate check since the process started.
func DialAttempts() int64 { return dialAttempts.Load() }
