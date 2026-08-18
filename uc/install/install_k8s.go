package install

import (
	"context"
	"fmt"
	"strings"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/runos-official/nodeagent/backend"
	"github.com/runos-official/nodeagent/commons"
	pb "github.com/runos-official/nodeagent/l2sec"
	"github.com/runos-official/nodeagent/roslog"
)

// WAITING FOR THE FIRST CONTROL PLANE (goal 30, G30-F1).
//
// Two control planes may now be started together on an empty cluster: nodeward hands the
// first-node role to exactly one of them (a unique index, not a count) and tells the other to WAIT
// rather than fail. The wait arrives as a gRPC FailedPrecondition whose message starts with
// waitForControlPlanePrefix; nodeward's copy of the prefix is uc/prep/wait_signal.go, and the two
// must stay identical. Every other error from GetInstallCommands keeps its old meaning.
//
// The bound is generous because a bare-metal first install takes about ten minutes, and the message
// nodeward sends says whether waiting can help (the first control plane is installing) or cannot
// (it died, or the cluster's control planes are all offline), so an operator reading the log knows
// which. Past the bound the install fails with the last reason nodeward gave.

const (
	// waitForControlPlanePrefix pairs with codes.FailedPrecondition to mean "ask again later".
	waitForControlPlanePrefix = "WAIT_FOR_CONTROL_PLANE: "
	// waitForControlPlaneBound is how long this node waits for a control plane before giving up.
	waitForControlPlaneBound = 30 * time.Minute
	// waitForControlPlanePoll is how often it asks again.
	waitForControlPlanePoll = 15 * time.Second
)

// isWaitForControlPlane recognises nodeward's "wait, then ask again" answer and returns the reason
// without its prefix.
func isWaitForControlPlane(err error) (string, bool) {
	st, ok := status.FromError(err)
	if !ok || st.Code() != codes.FailedPrecondition || !strings.HasPrefix(st.Message(), waitForControlPlanePrefix) {
		return "", false
	}
	return strings.TrimPrefix(st.Message(), waitForControlPlanePrefix), true
}

// waitVerdict decides what one refused attempt means, given how long we have waited so far. Pure,
// so the policy is testable without a network.
type waitVerdict int

const (
	waitAgain waitVerdict = iota
	waitGiveUp
)

func decideWait(waited, bound time.Duration) waitVerdict {
	if waited >= bound {
		return waitGiveUp
	}
	return waitAgain
}

// reportInstallError tells Nodeward this install is over, when it dies BEFORE a command list was
// ever fetched (goal 30, G30-F3). The command runner reports INSTALL_ERROR when a command fails,
// but a fetch that fails or a wait that gives up happens before any command runs, so the node sat
// at not_installed with a dead installer: conductor's provisioning job could not fast-fail on it
// and waited out its whole readiness clock, and a first-node claim held by such a node could not
// expire on failure. Best-effort: the status is a courtesy to the callers, the error is returned
// either way.
var reportInstallError = func() {
	if err := backend.UpdateStatus("INSTALL_ERROR"); err != nil {
		roslog.W("Could not report INSTALL_ERROR to Nodeward", err)
	}
}

// K8s installs Kubernetes on this node by fetching and running the install
// command list from Nodeward.
func K8s() error {
	c, _, backendCancel, conn, err := backend.NodewardL2Sec()
	if err != nil {
		return err
	}
	// The backend hands back its own cancel; defer it before reassigning so the
	// original context is not leaked when we replace ctx/cancel below.
	defer backendCancel()
	defer conn.Close()

	var fileList map[string]string

	// Get all files in /etc/netplan
	fileList, err = commons.GetAllFilesInDirectory("/etc/netplan")
	if err != nil {
		roslog.E("Error getting files in /etc/netplan", err)
	}

	// Encode the file list to JSON and then to base64
	fileListEncoded, err := commons.JSONB64Encode(fileList)

	request := &pb.GetInstallCommandsRequest{
		JsonB64FileList: fileListEncoded,
	}

	// Retry logic for transient "node agent not connected" errors
	// This can happen during cloud-init when there's a timing issue with stream connectivity
	var res *pb.InstallCommandList
	maxRetries := 5
	retryDelay := 2 * time.Second

	waitStarted := time.Time{}
	lastWaitReason := ""
	waitAnnounced := false

	for attempt := 1; attempt <= maxRetries; attempt++ {
		// One deadline per call: a wait can outlive any single deadline by design. 90 s, not 30:
		// the FIRST call after a wait ends is the expensive one, because nodeward then fetches the
		// join command from the just-ready control plane, and a 30 s deadline there failed the
		// install on the happy path the wait exists to serve (review defect 8).
		ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
		res, err = c.GetInstallCommands(ctx, request)
		cancel()
		if err == nil {
			break
		}

		// Check if this is a transient "not connected" error
		if strings.Contains(err.Error(), "node agent") && strings.Contains(err.Error(), "is not connected") {
			if attempt < maxRetries {
				roslog.W("Node agent stream not ready, retrying", err, "attempt", attempt, "delay", retryDelay)
				time.Sleep(retryDelay)
				retryDelay *= 2 // Exponential backoff
				continue
			}
		}

		// Nodeward says the cluster's control plane is not ready to be joined yet, and to ask
		// again. This is the concurrent-join case, not a failure, so it does not consume the
		// retry budget above; it has its own bound.
		if reason, waiting := isWaitForControlPlane(err); waiting {
			if waitStarted.IsZero() {
				waitStarted = time.Now()
			}
			waited := time.Since(waitStarted).Round(time.Second)
			if reason != lastWaitReason || !waitAnnounced {
				roslog.InstallInfo(fmt.Sprintf("Waiting for a control plane to join (%s so far): %s", waited, reason))
				lastWaitReason = reason
				waitAnnounced = true
			}
			if decideWait(waited, waitForControlPlaneBound) == waitGiveUp {
				roslog.E("Gave up waiting for a control plane", err, "waited", waited)
				reportInstallError()
				return fmt.Errorf("waited %s for a control plane to join and none became ready. Nodeward's last word: %s", waited, reason)
			}
			time.Sleep(waitForControlPlanePoll)
			attempt-- // a wait is not a retry
			continue
		}

		// Non-retryable error or max retries exceeded. Return a contextual error
		// (never panic) so the install exits non-zero with an actionable message
		// instead of dumping a Go stack trace under the systemd service.
		roslog.E("Error executing GetInstallCommands", err, "attempt", attempt)
		reportInstallError()
		return fmt.Errorf("could not fetch install commands from Nodeward after %d attempts: %w (check connectivity to Nodeward operations channel on TCP 9192 and that the node is registered)", maxRetries, err)
	}
	if !waitStarted.IsZero() {
		roslog.InstallInfo(fmt.Sprintf("A control plane is ready; joining after waiting %s", time.Since(waitStarted).Round(time.Second)))
	}

	return commons.ProcessInstallCommandsStatusAware(res)
}
