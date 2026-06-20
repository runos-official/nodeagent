package backend

import (
	"context"
	"flag"
	"fmt"
	"github.com/runos-official/nodeagent/config"
	"github.com/runos-official/nodeagent/l1sec"
	"github.com/runos-official/nodeagent/roslog"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"sync"
	"time"
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

	//creds, err := credentials.NewClientTLSFromFile(commons.Path(config.GetPublicCACertPath()),
	creds, err := credentials.NewClientTLSFromFile(config.GetPublicCACertPath(),
		config.GetNodewardHost())
	if err != nil {
		roslog.E("failed to load credentials", err)
		panic(err)
	}

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
