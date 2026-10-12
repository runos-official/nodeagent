package backend

import (
	"context"
	"encoding/json"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/runos-official/nodeagent/l2sec"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
	"google.golang.org/protobuf/types/known/emptypb"
)

// FPL315 phase 8: the L2SEC service config retries UNAVAILABLE on idempotent
// unary methods only. The old config nested everything under an unknown
// "serviceConfig" key, which grpc-go ignores, so nothing ever retried.

type parsedServiceConfig struct {
	MethodConfig []struct {
		Name []struct {
			Service string `json:"service"`
			Method  string `json:"method"`
		} `json:"name"`
		RetryPolicy *struct {
			MaxAttempts          int      `json:"maxAttempts"`
			InitialBackoff       string   `json:"initialBackoff"`
			MaxBackoff           string   `json:"maxBackoff"`
			BackoffMultiplier    float64  `json:"backoffMultiplier"`
			RetryableStatusCodes []string `json:"retryableStatusCodes"`
		} `json:"retryPolicy"`
	} `json:"methodConfig"`
}

func TestL2SecServiceConfigIsValid(t *testing.T) {
	dec := json.NewDecoder(strings.NewReader(l2secServiceConfig))
	dec.DisallowUnknownFields()
	var sc parsedServiceConfig
	if err := dec.Decode(&sc); err != nil {
		t.Fatalf("service config is not the expected JSON shape: %v", err)
	}
	if len(sc.MethodConfig) != 1 || sc.MethodConfig[0].RetryPolicy == nil {
		t.Fatalf("want one methodConfig with a retryPolicy, got %+v", sc)
	}
	rp := sc.MethodConfig[0].RetryPolicy
	if rp.MaxAttempts != 4 || rp.InitialBackoff != "0.5s" || rp.MaxBackoff != "5s" || rp.BackoffMultiplier != 2 {
		t.Errorf("retry policy = %+v, want 4 attempts, 0.5s, 5s, x2", *rp)
	}
	if len(rp.RetryableStatusCodes) != 1 || rp.RetryableStatusCodes[0] != "UNAVAILABLE" {
		t.Errorf("retryable codes = %v, want only UNAVAILABLE", rp.RetryableStatusCodes)
	}

	// grpc-go itself must accept it: an invalid default service config fails the client build.
	conn, err := grpc.NewClient("passthrough:///l2sec.invalid",
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithDefaultServiceConfig(l2secServiceConfig))
	if err != nil {
		t.Fatalf("grpc rejected the service config: %v", err)
	}
	conn.Close()
}

func TestL2SecServiceConfigNamesOnlyIdempotentMethods(t *testing.T) {
	var sc parsedServiceConfig
	if err := json.Unmarshal([]byte(l2secServiceConfig), &sc); err != nil {
		t.Fatal(err)
	}
	// Methods that mint credentials or change state on nodeward.
	never := map[string]bool{
		"RegenerateCertificate":        true,
		"GetClusterAgentCredentials":   true,
		"GetNodeRegistrationToken":     true,
		"GetInstallCommands":           true,
		"ManualSync":                   true,
		"UpdateStatus":                 true,
		"AddNodelog":                   true,
		"UpdateNodeInstallationStatus": true,
	}
	for _, mc := range sc.MethodConfig {
		for _, n := range mc.Name {
			if n.Service != "runos.nodeward.proto.l2sec.Nodeward" {
				t.Errorf("unexpected service %q", n.Service)
			}
			if n.Method == "" {
				t.Errorf("a name without a method would retry every method of %s", n.Service)
			}
			if never[n.Method] {
				t.Errorf("%s must never be retried", n.Method)
			}
		}
	}
}

// flakyNodeward answers UNAVAILABLE for the first `fail` calls of each method.
type flakyNodeward struct {
	mu    sync.Mutex
	fail  int
	calls map[string]int
}

func (f *flakyNodeward) handle(_ any, stream grpc.ServerStream) error {
	method, _ := grpc.MethodFromServerStream(stream)
	if err := stream.RecvMsg(&emptypb.Empty{}); err != nil {
		return err
	}
	f.mu.Lock()
	f.calls[method]++
	n := f.calls[method]
	f.mu.Unlock()
	if n <= f.fail {
		return status.Error(codes.Unavailable, "nodeward restarting")
	}
	return stream.SendMsg(&emptypb.Empty{})
}

func (f *flakyNodeward) count(method string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls["/runos.nodeward.proto.l2sec.Nodeward/"+method]
}

func dialFlaky(t *testing.T, fail int) (l2sec.NodewardClient, *flakyNodeward) {
	t.Helper()
	f := &flakyNodeward{fail: fail, calls: map[string]int{}}
	lis := bufconn.Listen(1 << 16)
	srv := grpc.NewServer(grpc.UnknownServiceHandler(f.handle))
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)

	conn, err := grpc.NewClient("passthrough:///l2sec.invalid",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return lis.DialContext(ctx) }),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithDefaultServiceConfig(l2secServiceConfig))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	return l2sec.NewNodewardClient(conn), f
}

func TestL2SecRetriesIdempotentMethodsOnUnavailable(t *testing.T) {
	t.Parallel()
	c, f := dialFlaky(t, 3)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if _, err := c.GetControlPlaneNodes(ctx, &l2sec.GetControlPlaneNodesRequest{}); err != nil {
		t.Fatalf("GetControlPlaneNodes after 3 UNAVAILABLE: %v", err)
	}
	if got := f.count("GetControlPlaneNodes"); got != 4 {
		t.Errorf("GetControlPlaneNodes calls = %d, want 4", got)
	}
	if _, err := c.GetInstallVpnCommands(ctx, &l2sec.GetInstallVpnCommandsRequest{}); err != nil {
		t.Fatalf("GetInstallVpnCommands after 3 UNAVAILABLE: %v", err)
	}
	if got := f.count("GetInstallVpnCommands"); got != 4 {
		t.Errorf("GetInstallVpnCommands calls = %d, want 4", got)
	}
}

func TestL2SecRetryStopsAtFourAttempts(t *testing.T) {
	t.Parallel()
	c, f := dialFlaky(t, 100)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	_, err := c.GetControlPlaneNodes(ctx, &l2sec.GetControlPlaneNodesRequest{})
	if status.Code(err) != codes.Unavailable {
		t.Fatalf("want UNAVAILABLE after the last attempt, got %v", err)
	}
	if got := f.count("GetControlPlaneNodes"); got != 4 {
		t.Errorf("calls = %d, want 4", got)
	}
}

func TestL2SecNeverRetriesMintingOrMutatingMethods(t *testing.T) {
	t.Parallel()
	c, f := dialFlaky(t, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	calls := map[string]func() error{
		"RegenerateCertificate": func() error {
			_, err := c.RegenerateCertificate(ctx, &l2sec.RegenerateCertificateRequest{})
			return err
		},
		"GetClusterAgentCredentials": func() error {
			_, err := c.GetClusterAgentCredentials(ctx, &l2sec.GetClusterAgentCredentialsRequest{})
			return err
		},
		"GetInstallCommands": func() error {
			_, err := c.GetInstallCommands(ctx, &l2sec.GetInstallCommandsRequest{})
			return err
		},
		"ManualSync": func() error {
			_, err := c.ManualSync(ctx, &l2sec.ManualSyncRequest{})
			return err
		},
		"UpdateStatus": func() error {
			_, err := c.UpdateStatus(ctx, &l2sec.UpdateStatusRequest{})
			return err
		},
		"AddNodelog": func() error {
			_, err := c.AddNodelog(ctx, &l2sec.AddNodelogRequest{})
			return err
		},
	}
	for name, call := range calls {
		if err := call(); status.Code(err) != codes.Unavailable {
			t.Errorf("%s: want the first UNAVAILABLE returned as is, got %v", name, err)
		}
		if got := f.count(name); got != 1 {
			t.Errorf("%s calls = %d, want 1", name, got)
		}
	}
}
