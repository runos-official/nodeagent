package backend

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"github.com/runos-official/nodeagent/config"
	"github.com/runos-official/nodeagent/l2sec"
	"github.com/runos-official/nodeagent/roslog"
	"github.com/runos-official/nodeagent/uc/certstore"
	"google.golang.org/grpc"
	"google.golang.org/grpc/backoff"
	"google.golang.org/grpc/credentials"
	"net"
	"os"
	"sync/atomic"
	"time"
)

// l2secDialTimeout bounds a single dial attempt so it cannot hang forever
// (replacing the unbounded grpc.WithBlock behavior). The in-process reconnect
// loop in the agent retries dialing on its own backoff schedule.
const l2secDialTimeout = 20 * time.Second

// l2secPort is the L2Sec port of Nodeward. A variable so a test can dial a
// loopback listener.
var l2secPort = 9192

// l2secNow is the clock of the local certificate check. A test seam.
var l2secNow = time.Now

// l2secDialLimit is the limit of one dial; l2secDialTimeout in production.
// A test seam.
var l2secDialLimit = l2secDialTimeout

// l2secConnectBackoff is grpc-go's reconnect schedule inside one dial. Most
// callers dial once and never retry a failed dial (install, UpdateStatus,
// AddNodelog, sync, certificate renew, status), so attempts must keep coming
// through the whole l2secDialTimeout window: after a 10 s nodeward restart the
// next attempt starts by about 18 s at worst. Keep the jitter small here.
// Spreading the fleet over time is the job of the supervised reconnect loop
// (commons.NewReconnectBackoff), not of this schedule (FPL315 review).
var l2secConnectBackoff = backoff.Config{
	BaseDelay:  100 * time.Millisecond,
	Multiplier: 1.6,
	Jitter:     0.2,
	MaxDelay:   10 * time.Second,
}

// l2secServiceConfig retries UNAVAILABLE on the idempotent unary methods
// only: GetInstallVpnCommands and GetControlPlaneNodes read and change
// nothing on nodeward. Every other unary method mints credentials or writes
// state (RegenerateCertificate, GetClusterAgentCredentials,
// GetNodeRegistrationToken, GetInstallCommands, ManualSync, UpdateStatus,
// AddNodelog, UpdateNodeInstallationStatus), so a lost reply must reach the
// caller and is never replayed here. Streams are not named, so they never
// retry. FPL315 phase 8.
const l2secServiceConfig = `{
	"methodConfig": [{
		"name": [
			{"service": "runos.nodeward.proto.l2sec.Nodeward", "method": "GetInstallVpnCommands"},
			{"service": "runos.nodeward.proto.l2sec.Nodeward", "method": "GetControlPlaneNodes"}
		],
		"retryPolicy": {
			"maxAttempts": 4,
			"initialBackoff": "0.5s",
			"maxBackoff": "5s",
			"backoffMultiplier": 2,
			"retryableStatusCodes": ["UNAVAILABLE"]
		}
	}]
}`

// loadL2SecClientTLS loads and validates the mTLS material for the L2Sec channel:
// the client keypair (certPath/keyPath) and the CA root (caPath, which must be a
// valid PEM bundle). It returns a configured *tls.Config or an error. It NEVER
// panics — a missing, empty, or non-PEM cert/CA (common on an unregistered or
// half-installed node) yields a clean error so the supervised caller can surface
// a message and back off instead of crashing the process with a stack trace.
//
// serverName pins the expected server hostname for verification. Extracted as a
// pure helper (no dialing, no globals) so this load+validate path is unit-testable
// against on-disk fixtures.
func loadL2SecClientTLS(certPath, keyPath, caPath, serverName string) (*tls.Config, error) {
	return loadL2SecClientTLSAt(certPath, keyPath, caPath, serverName, time.Now())
}

// loadL2SecClientTLSAt is loadL2SecClientTLS at the time now. A held leaf
// that has expired at now (now is not before NotAfter, the rule of the
// policy package) gives a CertificateExpiredError before any network call
// and with no log line.
func loadL2SecClientTLSAt(certPath, keyPath, caPath, serverName string, now time.Time) (*tls.Config, error) {
	cert, err := certstore.LoadKeyPair(certPath, keyPath)
	if err != nil {
		roslog.E("failed to load client cert", err)
		return nil, fmt.Errorf("failed to load client certificate: %w", err)
	}
	leaf := cert.Leaf
	if leaf == nil && len(cert.Certificate) > 0 {
		leaf, err = x509.ParseCertificate(cert.Certificate[0])
		if err != nil {
			roslog.E("failed to load client cert", err)
			return nil, fmt.Errorf("failed to parse client certificate: %w", err)
		}
	}
	if leaf != nil && !now.Before(leaf.NotAfter) {
		return nil, &CertificateExpiredError{NotAfter: leaf.NotAfter, Serial: fmt.Sprintf("%x", leaf.SerialNumber)}
	}

	ca := x509.NewCertPool()
	caBytes, err := os.ReadFile(caPath)
	if err != nil {
		roslog.E("failed to read ca cert", err, "ca_file_path", caPath)
		return nil, fmt.Errorf("failed to read CA certificate %s: %w", caPath, err)
	}
	if ok := ca.AppendCertsFromPEM(caBytes); !ok {
		roslog.E("failed to parse ca cert", nil, "ca_file_path", caPath)
		return nil, fmt.Errorf("failed to parse CA certificate %s", caPath)
	}

	cfg := &tls.Config{
		MinVersion:   tls.VersionTLS12,
		ServerName:   serverName,
		Certificates: []tls.Certificate{cert},
		RootCAs:      ca,
		// Runs after the normal chain and name checks (T-50).
		VerifyConnection: verifyL2SecServerLeaf,
	}
	return cfg, nil
}

// chooseClientCertificate gives a GetClientCertificate function that makes
// the choice crypto/tls makes from certs (the first certificate that the
// request supports, else none) and sets withheld when it sends none. The
// server then answers with a generic alert ("handshake failure" on TLS 1.2,
// "certificate required" on TLS 1.3, measured), and withheld lets the dial
// name the cause.
func chooseClientCertificate(certs []tls.Certificate, withheld *atomic.Bool) func(*tls.CertificateRequestInfo) (*tls.Certificate, error) {
	return func(cri *tls.CertificateRequestInfo) (*tls.Certificate, error) {
		for i := range certs {
			if cri.SupportsCertificate(&certs[i]) == nil {
				return &certs[i], nil
			}
		}
		withheld.Store(true)
		return &tls.Certificate{}, nil
	}
}

// CheckHeldCertificate reads the held client certificate and returns a
// CertificateExpiredError when it has expired (now is not before NotAfter).
// It makes no network call, writes no log line and creates no file. A
// certificate that does not read gives nil: the dial reports that fault.
//
// A caller that does local work before its dial (the VPN sync runs shell and
// Kubernetes checks that log) calls it first, so a timed caller stays silent
// on an expired certificate.
func CheckHeldCertificate() error {
	certPath, _, _ := config.MTLSPaths()
	b, err := os.ReadFile(certPath)
	if err != nil {
		return nil
	}
	block, _ := pem.Decode(b)
	if block == nil {
		return nil
	}
	leaf, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return nil
	}
	if !l2secNow().Before(leaf.NotAfter) {
		return &CertificateExpiredError{NotAfter: leaf.NotAfter, Serial: fmt.Sprintf("%x", leaf.SerialNumber)}
	}
	return nil
}

// NodewardL2Sec dials the L2Sec (mTLS) operational channel and returns the
// client plus the context, cancel func and connection the caller must clean up.
//
// The dial is bounded by l2secDialTimeout: it returns an error instead of
// blocking forever, so a supervised caller can back off and re-dial rather than
// wedging the process. Local/unrecoverable setup errors (missing or unparsable
// client cert / CA, common on an unregistered or half-installed node) are
// returned as errors too, never panicked, so callers surface a clean message
// instead of a raw Go stack trace.
//
// An expired held certificate gives a CertificateExpiredError at once: no
// network call and no log line. A TLS refusal of the client certificate gives
// a ClientCertificateRefusedError that wraps the dial error.
func NodewardL2Sec() (l2sec.NodewardClient, context.Context, context.CancelFunc, *grpc.ClientConn, error) {
	// Check the held certificate before the first log line, so an expired
	// certificate writes nothing here.
	tlsConfig, err := loadL2SecClientTLSAt(
		config.GetPublicKeyPath(), config.GetPrivateKeyPath(),
		config.GetCACertPath(), config.GetNodewardHost(), l2secNow())
	if err != nil {
		return nil, nil, nil, nil, err
	}

	addrL2Sec := fmt.Sprintf("%s:%d", config.GetNodewardHost(), l2secPort)
	roslog.I("Connecting to nodeward L2", "connection_string", addrL2Sec)
	dialAttempts.Add(1)

	var withheld atomic.Bool
	tlsConfig.GetClientCertificate = chooseClientCertificate(tlsConfig.Certificates, &withheld)
	watch := &refusalWatch{}
	conn, err := dialL2Sec(addrL2Sec, watchCreds{TransportCredentials: credentials.NewTLS(tlsConfig), w: watch})
	if err != nil {
		if refused := classifyDialRefusal(err, withheld.Load(), watch.seen()); IsClientCertificateRefused(refused) {
			// The server refused the certificate: the name resolves, so the
			// DNS diagnosis does not apply.
			roslog.W("did not connect to nodeward L2", refused)
			return nil, nil, nil, nil, refused
		}
		// Transient connect failure: return the error so the caller can back off
		// and re-dial instead of crashing the process.
		//
		// A node whose per-link DNS scopes were cleared and never replaced fails
		// here with a bare "context deadline exceeded", which never mentions DNS
		// and gives the operator no next step. Attach the resolver diagnosis when
		// the Nodeward host does not resolve, so the log names the real fault and
		// the command that repairs it. The original error stays wrapped.
		err = explainNodewardDialFailure(config.GetNodewardHost(), err, net.DefaultResolver.LookupHost)
		roslog.W("did not connect to nodeward L2", err)
		return nil, nil, nil, nil, err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	c := l2sec.NewNodewardClient(conn)

	return c, ctx, cancel, conn, nil
}

// dialL2Sec dials addr with the L2Sec connect schedule and service config,
// bounded by l2secDialTimeout.
func dialL2Sec(addr string, creds credentials.TransportCredentials) (*grpc.ClientConn, error) {
	attemptCount := 0
	// Bound a single dial attempt. We use DialContext with a timeout instead of
	// grpc.WithBlock + no deadline, so a dead control plane cannot hang the dial
	// forever; the agent's reconnect loop owns retry/backoff at a higher level.
	dialCtx, dialCancel := context.WithTimeout(context.Background(), l2secDialLimit)
	defer dialCancel()
	return grpc.DialContext(dialCtx, addr,
		grpc.WithTransportCredentials(creds),
		grpc.WithDefaultServiceConfig(l2secServiceConfig),
		grpc.WithBlock(),
		// A dial that ends returns the last connection error, not only
		// "context deadline exceeded", so a TLS refusal of the client
		// certificate reaches the caller.
		grpc.WithReturnConnectionError(),
		grpc.WithConnectParams(grpc.ConnectParams{Backoff: l2secConnectBackoff}),
		grpc.WithUnaryInterceptor(func(ctx context.Context, method string, req, reply any, cc *grpc.ClientConn, invoker grpc.UnaryInvoker, opts ...grpc.CallOption) error {
			attemptCount++
			roslog.I("Attempting connection", "attempt", attemptCount, "address", addr)
			err := invoker(ctx, method, req, reply, cc, opts...)
			if err != nil {
				roslog.W("Connection attempt failed", err, "attempt", attemptCount)
			}
			return err
		}),
	)
}
