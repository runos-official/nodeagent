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

// isTransientFetchError reports whether a GetInstallCommands error is worth another attempt: the
// agent stream not yet registered on nodeward ("node agent ... is not connected", the cloud-init
// timing case), a nodeward that is restarting (Unavailable), or a call that ran out of deadline
// (DeadlineExceeded). Pure, so the policy is testable without a network.
func isTransientFetchError(err error) bool {
	if err == nil {
		return false
	}
	if strings.Contains(err.Error(), "node agent") && strings.Contains(err.Error(), "is not connected") {
		return true
	}
	if st, ok := status.FromError(err); ok {
		return st.Code() == codes.Unavailable || st.Code() == codes.DeadlineExceeded
	}
	return false
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

	res, err := fetchInstallCommands(func() (*pb.InstallCommandList, error) {
		// One deadline per call: a wait can outlive any single deadline by design. 90 s, not 30:
		// the FIRST call after a wait ends is the expensive one, because nodeward then fetches the
		// join command from the just-ready control plane, and a 30 s deadline there failed the
		// install on the happy path the wait exists to serve (review defect 8).
		ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
		defer cancel()
		return c.GetInstallCommands(ctx, request)
	}, newFetchRetryBackoff(), time.Sleep)
	if err != nil {
		return err
	}

	return commons.ProcessInstallCommandsStatusAware(res)
}

// fetchMaxAttempts bounds the transient-error retries of the install command fetch. The waits use
// equal jitter (commons.NewRetryBackoff): random in [0.5 s, 1 s], [1 s, 2 s], and so on up to
// [15 s, 30 s]. So 8 attempts always wait between 45.5 s and 91 s in total, never less than the
// old fixed 30 s schedule, and installers that lost nodeward together still spread out. A full
// jitter wait could add up to almost nothing and end an install the old schedule rode out
// (FPL315 review).
const fetchMaxAttempts = 8

// fetchRetryFloor is the old fixed schedule's total wait (2+4+8+16 s). The retries never give up
// sooner.
const fetchRetryFloor = 30 * time.Second

// newFetchRetryBackoff returns the install fetch's retry backoff.
func newFetchRetryBackoff() *commons.Backoff { return commons.NewRetryBackoff() }

// fetchInstallCommands calls fetch until it returns a command list. A transient error (see
// isTransientFetchError) waits retry's next delay and retries, up to fetchMaxAttempts calls. The
// wait-for-control-plane answer polls on its own bound and does not use the retry budget. Any other
// error, or a spent budget, reports INSTALL_ERROR and returns an error. sleep is time.Sleep outside
// tests.
func fetchInstallCommands(fetch func() (*pb.InstallCommandList, error), retry *commons.Backoff, sleep func(time.Duration)) (*pb.InstallCommandList, error) {
	waitStarted := time.Time{}
	lastWaitReason := ""
	waitAnnounced := false

	for attempt := 1; attempt <= fetchMaxAttempts; attempt++ {
		res, err := fetch()
		if err == nil {
			if !waitStarted.IsZero() {
				roslog.InstallInfo(fmt.Sprintf("A control plane is ready; joining after waiting %s", time.Since(waitStarted).Round(time.Second)))
			}
			return res, nil
		}

		// A transient "not connected" error (the cloud-init timing case: the stream is not yet
		// registered on nodeward), or a transient transport error: a nodeward rollout (Unavailable)
		// or a slow first call after a wait (DeadlineExceeded) is not a verdict on this install, and
		// since the fetch failure reports INSTALL_ERROR (G30-F3) it must not be reached by a blip
		// that a retry would have carried through.
		if isTransientFetchError(err) && attempt < fetchMaxAttempts {
			delay := retry.Next()
			roslog.W("Install command fetch hit a transient error, retrying", err, "attempt", attempt, "delay", delay)
			sleep(delay)
			continue
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
				return nil, fmt.Errorf("waited %s for a control plane to join and none became ready. Nodeward's last word: %s", waited, reason)
			}
			sleep(waitForControlPlanePoll)
			attempt-- // a wait is not a retry
			continue
		}

		// Non-retryable error or max retries exceeded. Return a contextual error
		// (never panic) so the install exits non-zero with an actionable message
		// instead of dumping a Go stack trace under the systemd service.
		roslog.E("Error executing GetInstallCommands", err, "attempt", attempt)
		reportInstallError()
		return nil, fmt.Errorf("could not fetch install commands from Nodeward after %d attempts: %w (check connectivity to Nodeward operations channel on TCP 9192 and that the node is registered)", attempt, err)
	}
	// Unreachable: the last attempt always returns above.
	return nil, fmt.Errorf("could not fetch install commands from Nodeward after %d attempts", fetchMaxAttempts)
}
