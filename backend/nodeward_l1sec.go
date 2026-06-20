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
		roslog.E("failed to read l1sec ca cert", err, "ca_file_path", caPath)
		panic(err)
	}
	caPool := x509.NewCertPool()
	if ok := caPool.AppendCertsFromPEM(caBytes); !ok {
		roslog.E("failed to parse l1sec ca cert", nil, "ca_file_path", caPath)
		panic("failed to parse l1sec ca file")
	}
	creds := credentials.NewTLS(&tls.Config{
		MinVersion: tls.VersionTLS12,
		ServerName: config.GetNodewardHost(),
		RootCAs:    caPool,
	})

	// Set up a connection to the server.
	conn, err := grpc.Dial(*addrL1Sec, grpc.WithTransportCredentials(creds))

	if err != nil {
		roslog.E("did not connect", err)
		panic(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)

	c := l1sec.NewNodewardClient(conn)

	return c, ctx, cancel, conn
}
