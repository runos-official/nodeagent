package registernode

import (
	"context"
	"fmt"
	"time"

	"github.com/runos-official/nodeagent/backend"
	"github.com/runos-official/nodeagent/commons"
	"github.com/runos-official/nodeagent/config"
	pb "github.com/runos-official/nodeagent/l1sec"
	"github.com/runos-official/nodeagent/roslog"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// RegisterNode registers this node with Nodeward over L1Sec, storing the issued
// mTLS certificates and CA, and persisting the resolved account and node IDs.
// It returns an error (never log.Fatalf) so the caller can exit non-zero with an
// actionable message; gRPC failures are mapped to operator remedies.
func RegisterNode(token, aid, machineId, cp, server string) error {
	config.SetNodewardHost(server)

	c, _, backendCancel, conn := backend.NodewardL1Sec()
	defer backendCancel()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	defer conn.Close()

	request := &pb.NodeRegistrationRequest{
		Token:     token,
		Aid:       aid,
		MachineId: machineId,
		Os:        commons.GetOSInfo(),
	}
	r, err := c.Register(ctx, request)
	if err != nil {
		return classifyRegisterError(err, aid, server)
	}

	fmt.Println("Writing CA certificate to file...")
	if err := StorePublicKey(r.CaCert, config.GetCACertPath()); err != nil {
		return fmt.Errorf("could not store CA certificate (is /etc/runos writable? run as root): %w", err)
	}

	fmt.Println("Writing public certificate to file...")
	if err := StorePublicKey(r.PublicKey, config.GetPublicKeyPath()); err != nil {
		return fmt.Errorf("could not store public certificate (is /etc/runos writable? run as root): %w", err)
	}

	fmt.Println("Writing private certificate to file...")
	if err := StorePrivateKey(r.PrivateKey, config.GetPrivateKeyPath()); err != nil {
		return fmt.Errorf("could not store private certificate (is /etc/runos writable? run as root): %w", err)
	}

	if err := config.UpdateConfig(aid, r.Nid); err != nil {
		return fmt.Errorf("could not persist registration to /etc/runos/config.yaml (is it writable? run as root): %w", err)
	}

	if err := backend.AddNodelog(3, "NodeInstallation", "Node agent has been registered."); err != nil {
		roslog.I("Could not add nodelog", err)
	}
	return nil
}

// classifyRegisterError maps a gRPC registration failure to an actionable
// operator message: a fixable token/account problem vs an unreachable host.
func classifyRegisterError(err error, aid, server string) error {
	st, ok := status.FromError(err)
	if !ok {
		return fmt.Errorf("registration failed: %w", err)
	}
	switch st.Code() {
	case codes.Unauthenticated, codes.PermissionDenied:
		return fmt.Errorf("registration rejected: the token is expired, already used, or invalid. Generate a fresh registration command from the RunOS console and re-run (server: %s)", server)
	case codes.NotFound, codes.InvalidArgument:
		return fmt.Errorf("registration rejected: account ID (--aid %q) not found or malformed. Copy the exact command from the console; do not edit --aid by hand", aid)
	case codes.Unavailable, codes.DeadlineExceeded:
		return fmt.Errorf("could not reach Nodeward at %s:9191 (%s). Check egress firewall on TCP 9191, DNS, proxy, and that --server is correct. Run: sudo runos preflight --server %s", server, st.Message(), server)
	default:
		return fmt.Errorf("registration failed (%s): %s", st.Code(), st.Message())
	}
}
