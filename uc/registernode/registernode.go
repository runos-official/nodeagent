package registernode

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/runos-official/nodeagent/backend"
	"github.com/runos-official/nodeagent/commons"
	"github.com/runos-official/nodeagent/config"
	pb "github.com/runos-official/nodeagent/l1sec"
	"github.com/runos-official/nodeagent/roslog"
	"github.com/runos-official/nodeagent/uc/csrcert"
	"github.com/runos-official/nodeagent/uc/hostnet"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// RegisterNode registers this node with Nodeward over L1Sec, storing the issued
// mTLS certificates and CA, and persisting the resolved account and node IDs.
// It returns an error (never log.Fatalf) so the caller can exit non-zero with an
// actionable message; gRPC failures are mapped to operator remedies.
func RegisterNode(token, aid, machineId, server string) error {
	config.SetNodewardHost(server)

	c, _, backendCancel, conn := backend.NodewardL1Sec()
	defer backendCancel()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	defer conn.Close()

	// The machine's own networks travel with the registration (ADR-0003). For a cluster's FIRST
	// node the control plane picks an overlay range that AVOIDS them, which is the only moment a
	// collision can be prevented rather than reported. For a later node they are compared against
	// the range the cluster already holds, and a collision refuses the registration.
	//
	// Best effort: a machine that cannot enumerate its own interfaces sends fewer entries, or
	// none, and registration proceeds exactly as it did before this field existed.
	hostCidrs := hostnet.Collect()

	request := &pb.NodeRegistrationRequest{
		Token:     token,
		Aid:       aid,
		MachineId: machineId,
		Os:        commons.GetOSInfo(),
		HostCidrs: hostCidrs,
	}

	// Progress lines are diagnostics, not program output: send them to stderr so
	// stdout stays clean for anything a caller might pipe/capture.
	fmt.Fprintln(os.Stderr, "Requesting certificate and writing credentials to file...")
	nid, err := registerAndStore(ctx, c, request, DefaultCredPaths())
	if err != nil {
		var rpc *rpcError
		if errors.As(err, &rpc) {
			return classifyRegisterError(rpc.err, aid, server)
		}
		if errors.Is(err, csrcert.ErrUnusableResponse) {
			return fmt.Errorf("registration returned credentials this node cannot use (%w). The token is now spent: generate a fresh registration command from the RunOS console and re-run", err)
		}
		return fmt.Errorf("could not store registration credentials (is /etc/runos writable? run as root): %w", err)
	}

	if err := config.UpdateConfig(aid, nid); err != nil {
		return fmt.Errorf("could not persist registration to /etc/runos/config.yaml (is it writable? run as root): %w", err)
	}

	if err := backend.AddNodelog(3, "NodeInstallation", "Node agent has been registered."); err != nil {
		roslog.I("Could not add nodelog", err)
	}
	return nil
}

// rpcError marks a failed Register call, so the caller can map its status.
type rpcError struct{ err error }

func (e *rpcError) Error() string { return e.err.Error() }
func (e *rpcError) Unwrap() error { return e.err }

// registerAndStore makes a fresh key, sends its CSR with the registration
// (FPL315), and stores what nodeward returns. The private key stays here. An
// old nodeward ignores the CSR and returns a key, which is stored as before.
// It returns the node id.
func registerAndStore(ctx context.Context, c pb.NodewardClient, req *pb.NodeRegistrationRequest, p CredPaths) (string, error) {
	key, err := csrcert.NewKey()
	if err != nil {
		return "", err
	}
	req.CsrPem = key.CSRPEM()
	r, err := c.Register(ctx, req)
	if err != nil {
		return "", &rpcError{err: err}
	}
	outcome, err := StoreCredentials(key, csrcert.Response{
		CertificatePEM: r.CertificatePem,
		CACertPEM:      r.CaCert,
		PrivateKey:     r.PrivateKey,
		PublicKey:      r.PublicKey,
	}, p)
	if err != nil {
		return "", err
	}
	roslog.I("Registration credentials stored", "path", outcome.String())
	return r.Nid, nil
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
