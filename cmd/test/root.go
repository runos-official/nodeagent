package test

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/runos-official/nodeagent/backend"
	"github.com/runos-official/nodeagent/roslog"
	"github.com/spf13/cobra"
	"google.golang.org/grpc"
	"google.golang.org/grpc/reflection/grpc_reflection_v1alpha"
)

var (
	jsonOutput bool
	verbose    bool
)

var RootCmd = &cobra.Command{
	Use:   "test",
	Short: "Run agent self-diagnostics",
	Long: `Run agent self-diagnostics against the Nodeward control plane.

This checks the live agent plumbing, not the host environment (that is what
'runos preflight' is for). It verifies:

  - the L1Sec (TLS) connection to Nodeward can be established
  - the Nodeward gRPC server answers a reflection request
  - control-plane nodes can be listed
  - a node log line can be written end to end

The reflection check confirms Nodeward is serving; pass --verbose to also print
every reflected service name.

Note: this command writes one test entry to the node log (visible in the
console and 'runos logs') to verify the write path. It does not mutate any
other remote state.

Exit status is non-zero if any check fails, so 'runos test' can be used in
health checks and shell "&&" chains.`,
	Example: `  runos test
  runos test --json
  runos test --verbose`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		return runTest()
	},
}

func init() {
	RootCmd.Flags().BoolVar(&jsonOutput, "json", false, "Emit results as a single JSON object to stdout")
	RootCmd.Flags().BoolVar(&verbose, "verbose", false, "Print extra detail (e.g. every reflected service name)")
}

// checkResult is one entry in the --json output's checks array.
type checkResult struct {
	Name  string `json:"name"`
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`
}

// testReport is the stable struct emitted by --json.
type testReport struct {
	OK     bool          `json:"ok"`
	Checks []checkResult `json:"checks"`
}

// failure carries the operator-facing block fields for the first failed check so
// runTest can surface it via roslog.Fail after the run completes.
type failure struct {
	step   string
	cause  string
	remedy string
}

// runTest executes each diagnostic check in order, rendering a ✓/✗ line per
// check to stdout (or a JSON object with --json), and returns a non-nil error
// when any check fails so the process exits non-zero.
func runTest() error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	var (
		results []checkResult
		fail    *failure
	)

	// record appends a result, emits the ✓/✗ line (unless --json), keeps a
	// durable log line, and remembers the first failure's operator block.
	record := func(name string, err error, f *failure) bool {
		ok := err == nil
		errStr := ""
		if err != nil {
			errStr = err.Error()
		}
		results = append(results, checkResult{Name: name, OK: ok, Error: errStr})
		if ok {
			roslog.I("test check passed", "check", name)
		} else {
			roslog.E("test check failed", err, "check", name)
		}
		if !jsonOutput {
			printCheck(name, ok, errStr)
		}
		if !ok && fail == nil {
			fail = f
		}
		return ok
	}

	if !jsonOutput {
		roslog.Printf("\n%s%sRunOS Node Agent Self-Test%s\n\n", roslog.ColorBold, roslog.ColorCyan, roslog.ColorReset)
	}

	// 1. Backend (L1Sec) connection.
	//
	// NodewardL1Sec reports and exits the process itself on CA-load/dial
	// failure (it predates this command's error-return contract), so a nil
	// conn here means a transport-level fault we could not catch; treat it as a
	// failed check rather than dereferencing nil.
	_, _, connCancel, conn := backend.NodewardL1Sec()
	if conn == nil {
		record("Backend connection (L1Sec)",
			fmt.Errorf("could not establish a connection to Nodeward"),
			&failure{
				step:   "Connect to Nodeward (self-test)",
				cause:  "the L1Sec connection to Nodeward could not be established",
				remedy: "Check the node can reach the Nodeward host on TCP 9191 (egress firewall / DNS / proxy).",
			})
		return finish(results, fail)
	}
	defer connCancel()
	defer conn.Close()
	record("Backend connection (L1Sec)", nil, nil)

	// 2. Nodeward serves gRPC reflection (proves the server is answering).
	services, reflErr := listServices(ctx, conn)
	record("Nodeward gRPC reflection", reflErr, &failure{
		step:   "Query Nodeward gRPC reflection",
		cause:  causeOf(reflErr),
		remedy: "Nodeward is reachable but not answering gRPC reflection. Confirm the Nodeward service is healthy; if it persists, contact your operator.",
	})
	if reflErr == nil && verbose && !jsonOutput {
		if len(services) == 0 {
			roslog.Printf("      (no services reported)\n")
		}
		for _, s := range services {
			roslog.Printf("      service: %s\n", s)
		}
	}

	// 3. Control-plane nodes can be listed.
	cpNodes, cpErr := backend.GetControlPlaneNodes()
	record("List control-plane nodes", cpErr, &failure{
		step:   "List control-plane nodes",
		cause:  causeOf(cpErr),
		remedy: "Nodeward could not return the control-plane node list. Confirm this node is registered ('runos status') and Nodeward is healthy.",
	})
	if cpErr == nil && !jsonOutput {
		roslog.Printf("      found %d control-plane node(s)\n", len(cpNodes))
	}

	// 4. Node log write path (end to end). Documented in the help text.
	logErr := backend.AddNodelog(3, "Test", fmt.Sprintf("Test command executed at %s", time.Now().Format(time.RFC3339)))
	record("Write node log entry", logErr, &failure{
		step:   "Write a node log entry",
		cause:  causeOf(logErr),
		remedy: "Nodeward rejected a node log write. Confirm this node's mTLS certificate is valid ('runos status') and Nodeward is healthy.",
	})

	return finish(results, fail)
}

// finish emits the final summary (JSON or human) and returns a non-nil,
// already-reported error when any check failed so the process exits non-zero
// with exactly one failure block.
func finish(results []checkResult, fail *failure) error {
	allOK := true
	for _, r := range results {
		if !r.OK {
			allOK = false
			break
		}
	}

	if jsonOutput {
		out, err := json.MarshalIndent(testReport{OK: allOK, Checks: results}, "", "  ")
		if err != nil {
			return roslog.Fail("Render self-test JSON", err.Error(),
				"This is an internal error; email support@runos.com with the command you ran.")
		}
		roslog.Println(string(out))
		if allOK {
			return nil
		}
		// JSON already conveys the failure; suppress the second human block but
		// still exit non-zero.
		return roslog.AlreadyReported(fmt.Errorf("self-test failed"))
	}

	if allOK {
		roslog.Printf("\n%s%s✓ All checks passed%s\n\n", roslog.ColorBold, roslog.ColorGreen, roslog.ColorReset)
		return nil
	}

	if fail != nil {
		return roslog.Fail(fail.step, fail.cause, fail.remedy)
	}
	// Defensive: a failure was recorded but no block was attached.
	return roslog.AlreadyReported(fmt.Errorf("self-test failed"))
}

// printCheck renders a single ✓/✗ result line to stdout in the status/install
// house style.
func printCheck(name string, ok bool, errStr string) {
	if ok {
		roslog.Printf("  %s✓%s %s\n", roslog.ColorGreen, roslog.ColorReset, name)
		return
	}
	roslog.Printf("  %s✗%s %s\n", roslog.ColorRed, roslog.ColorReset, name)
	if errStr != "" {
		roslog.Printf("      %s%s%s\n", roslog.ColorDimGray, errStr, roslog.ColorReset)
	}
}

// listServices issues a single ListServices reflection request and returns the
// reflected service names.
func listServices(ctx context.Context, conn grpc.ClientConnInterface) ([]string, error) {
	client := grpc_reflection_v1alpha.NewServerReflectionClient(conn)

	stream, err := client.ServerReflectionInfo(ctx)
	if err != nil {
		return nil, fmt.Errorf("could not open reflection stream: %w", err)
	}

	if err := stream.Send(&grpc_reflection_v1alpha.ServerReflectionRequest{
		MessageRequest: &grpc_reflection_v1alpha.ServerReflectionRequest_ListServices{
			ListServices: "*",
		},
	}); err != nil {
		return nil, fmt.Errorf("could not send reflection request: %w", err)
	}

	resp, err := stream.Recv()
	if err != nil {
		return nil, fmt.Errorf("could not read reflection response: %w", err)
	}

	listResp := resp.GetListServicesResponse()
	if listResp == nil {
		return nil, fmt.Errorf("reflection response contained no service list")
	}

	names := make([]string, 0, len(listResp.Service))
	for _, s := range listResp.Service {
		names = append(names, s.Name)
	}
	return names, nil
}

func causeOf(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
