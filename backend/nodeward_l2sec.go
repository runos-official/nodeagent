package backend

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"github.com/runos-official/nodeagent/config"
	"github.com/runos-official/nodeagent/l2sec"
	"github.com/runos-official/nodeagent/roslog"
	"google.golang.org/grpc"
	"google.golang.org/grpc/backoff"
	"google.golang.org/grpc/credentials"
	"os"
	"time"
)

// l2secDialTimeout bounds a single dial attempt so it cannot hang forever
// (replacing the unbounded grpc.WithBlock behavior). The in-process reconnect
// loop in the agent retries dialing on its own backoff schedule.
const l2secDialTimeout = 20 * time.Second

// NodewardL2Sec dials the L2Sec (mTLS) operational channel and returns the
// client plus the context, cancel func and connection the caller must clean up.
//
// The dial is bounded by l2secDialTimeout: it returns an error instead of
// blocking forever, so a supervised caller can back off and re-dial rather than
// wedging the process. Local/unrecoverable setup errors (missing or unparsable
// client cert / CA, common on an unregistered or half-installed node) are
// returned as errors too, never panicked, so callers surface a clean message
// instead of a raw Go stack trace.
func NodewardL2Sec() (l2sec.NodewardClient, context.Context, context.CancelFunc, *grpc.ClientConn, error) {
	addrL2Sec := fmt.Sprintf("%s:%d", config.GetNodewardHost(), 9192)
	roslog.I("Connecting to nodeward L2", "connection_string", addrL2Sec)

	cert, err := tls.LoadX509KeyPair(config.GetPublicKeyPath(), config.GetPrivateKeyPath())
	if err != nil {
		roslog.E("failed to load client cert", err)
		return nil, nil, nil, nil, fmt.Errorf("failed to load client certificate: %w", err)
	}

	ca := x509.NewCertPool()
	caFilePath := config.GetCACertPath()
	caBytes, err := os.ReadFile(caFilePath)
	if err != nil {
		roslog.E("failed to read ca cert", err, "ca_file_path", caFilePath)
		return nil, nil, nil, nil, fmt.Errorf("failed to read CA certificate %s: %w", caFilePath, err)
	}
	if ok := ca.AppendCertsFromPEM(caBytes); !ok {
		roslog.E("failed to parse ca cert", nil, "ca_file_path", caFilePath)
		return nil, nil, nil, nil, fmt.Errorf("failed to parse CA certificate %s", caFilePath)
	}

	tlsConfig := &tls.Config{
		MinVersion:   tls.VersionTLS12,
		ServerName:   config.GetNodewardHost(),
		Certificates: []tls.Certificate{cert},
		RootCAs:      ca,
	}

	attemptCount := 0
	// Bound a single dial attempt. We use DialContext with a timeout instead of
	// grpc.WithBlock + no deadline, so a dead control plane cannot hang the dial
	// forever; the agent's reconnect loop owns retry/backoff at a higher level.
	dialCtx, dialCancel := context.WithTimeout(context.Background(), l2secDialTimeout)
	defer dialCancel()
	conn, err := grpc.DialContext(dialCtx, addrL2Sec,
		grpc.WithTransportCredentials(credentials.NewTLS(tlsConfig)),
		grpc.WithDefaultServiceConfig(`{
            "serviceConfig": {
                "healthCheckConfig": {
                    "serviceName": ""
                },
                "retryPolicy": {
                    "MaxAttempts": 0,
                    "InitialBackoff": "0.1s",
                    "MaxBackoff": "60s",
                    "BackoffMultiplier": 2.0,
                    "RetryableStatusCodes": [
                        "UNAVAILABLE"
                    ]
                }
            }
        }`),
		grpc.WithBlock(),
		grpc.WithConnectParams(grpc.ConnectParams{
			Backoff: backoff.Config{
				BaseDelay:  100 * time.Millisecond,
				Multiplier: 1.6,
				Jitter:     0.2,
				MaxDelay:   10 * time.Second,
			},
		}),
		grpc.WithUnaryInterceptor(func(ctx context.Context, method string, req, reply any, cc *grpc.ClientConn, invoker grpc.UnaryInvoker, opts ...grpc.CallOption) error {
			attemptCount++
			roslog.I("Attempting connection", "attempt", attemptCount, "address", addrL2Sec)
			err := invoker(ctx, method, req, reply, cc, opts...)
			if err != nil {
				roslog.W("Connection attempt failed", err, "attempt", attemptCount)
			}
			return err
		}),
	)
	if err != nil {
		// Transient connect failure: return the error so the caller can back off
		// and re-dial instead of crashing the process.
		roslog.W("did not connect to nodeward L2", err)
		return nil, nil, nil, nil, err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	c := l2sec.NewNodewardClient(conn)

	return c, ctx, cancel, conn, nil
}
