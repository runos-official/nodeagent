package backend

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"flag"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/runos-official/nodeagent/config"
	"github.com/runos-official/nodeagent/l1sec"
	"github.com/runos-official/nodeagent/roslog"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
)

var (
	addrL1Sec *string
	onceL1Sec sync.Once
)

// NodewardL1Sec dials the L1Sec (TLS) registration channel and returns the
// client plus the context, cancel func and connection the caller must clean up.
func NodewardL1Sec() (l1sec.NodewardClient, context.Context, context.CancelFunc, *grpc.ClientConn) {
	onceL1Sec.Do(func() {
		connectionString := fmt.Sprintf("%s:%d", config.GetNodewardHost(), 9191)
		roslog.I("Connecting to nodeward", "connection_string", connectionString)
		addrL1Sec = flag.String("addr_nodeward_l1sec", connectionString, "the address to connect to")
		flag.Parse()
	})

	// Build the TLS config explicitly (instead of credentials.NewClientTLSFromFile)
	// so we can set a MinVersion floor. The server CA pinning (RootCAs) and
	// hostname verification (ServerName) are unchanged from the prior helper.
	caPath := config.GetPublicCACertPath()
	caBytes, err := os.ReadFile(caPath)
	if err != nil {
		roslog.Fail("Load the Nodeward registration CA certificate",
			fmt.Sprintf("could not read %s: %v", caPath, err),
			"This node was not bootstrapped by the installer (which downloads the CA). Run the full install command from the RunOS console; do not run 'register' on its own.")
		os.Exit(1)
	}

	// Pin the L1Sec public CA: it was downloaded over a CDN with no integrity
	// check, so verify it against the expected sha256 before trusting it as the
	// registration TLS root. Mismatch with a configured pin is fatal (a MITM of
	// the CA fetch could otherwise impersonate nodeward -> root RCE). If no pin
	// is configured we proceed but warn loudly.
	if pinned, perr := verifyL1SecCAPin(caBytes); perr != nil {
		roslog.Fail("Verify the Nodeward registration CA (pin check)",
			fmt.Sprintf("CA at %s failed pin verification: %v", caPath, perr),
			"The downloaded CA does not match the expected fingerprint — a network MITM of the CA download is possible. Do not proceed; report this to your operator.")
		os.Exit(1)
	} else if !pinned {
		roslog.W("l1sec CA is NOT pinned (no mtls.public-ca-sha256 / build pin set); the registration CA is trusted without integrity verification", nil, "ca_file_path", caPath)
	} else {
		roslog.I("l1sec CA pin verified")
	}

	caPool := x509.NewCertPool()
	if ok := caPool.AppendCertsFromPEM(caBytes); !ok {
		roslog.Fail("Parse the Nodeward registration CA certificate",
			fmt.Sprintf("%s is not a valid PEM certificate", caPath),
			"The CA file is corrupt or truncated (a proxy or error page saved as the cert?). Re-run the full install command to re-download it.")
		os.Exit(1)
	}
	creds := credentials.NewTLS(&tls.Config{
		MinVersion: tls.VersionTLS12,
		ServerName: config.GetNodewardHost(),
		RootCAs:    caPool,
	})

	// Set up a connection to the server.
	conn, err := grpc.Dial(*addrL1Sec, grpc.WithTransportCredentials(creds))

	if err != nil {
		roslog.Fail("Connect to Nodeward (L1Sec registration)",
			fmt.Sprintf("could not dial %s: %v", *addrL1Sec, err),
			"Check the node can reach the Nodeward host on TCP 9191 (egress firewall / DNS / proxy). Verify with: nc -vz "+config.GetNodewardHost()+" 9191")
		os.Exit(1)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)

	c := l1sec.NewNodewardClient(conn)

	return c, ctx, cancel, conn
}
