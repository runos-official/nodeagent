package commons

import (
	"fmt"
	"strings"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/runos-official/nodeagent/roslog"
)

// initialisingNewClusterStatus is the status the first-node command list reports seconds before
// `kubeadm init`. Nodeward answers it with a refusal when this node no longer holds the cluster's
// first-node role (goal 30, G30-F1); see confirmFirstNodeClaim.
const initialisingNewClusterStatus = "INITIALIZING_NEW_CLUSTER"

// firstNodeClaimReleasedPrefix opens nodeward's refusal. Nodeward's copy lives in
// persist/node/first_node_claim.go; the two must stay identical.
const firstNodeClaimReleasedPrefix = "FIRST_NODE_CLAIM_RELEASED: "

// How long an unreachable nodeward is retried before the init is abandoned rather than run on a
// guess. Long enough to ride out a nodeward rolling restart, short enough that an operator watching
// the install sees the failure rather than a hang.
const (
	confirmClaimAttempts = 24
	confirmClaimInterval = 5 * time.Second
)

// claimVerdict is what one answer from nodeward means for the init.
type claimVerdict int

const (
	claimConfirmed claimVerdict = iota
	claimReleased
	claimUnknown
)

// classifyClaimAnswer reads nodeward's answer to INITIALIZING_NEW_CLUSTER. Pure, so the three
// outcomes are testable without a network: confirmed (nodeward stored the status), released
// (nodeward refused because another machine holds the role), unknown (nodeward could not be
// asked, or answered with something else).
func classifyClaimAnswer(err error) (claimVerdict, string) {
	if err == nil {
		return claimConfirmed, ""
	}
	st, ok := status.FromError(err)
	if ok && st.Code() == codes.FailedPrecondition && strings.HasPrefix(st.Message(), firstNodeClaimReleasedPrefix) {
		return claimReleased, strings.TrimPrefix(st.Message(), firstNodeClaimReleasedPrefix)
	}
	return claimUnknown, err.Error()
}

// confirmFirstNodeClaim reports INITIALIZING_NEW_CLUSTER through `report` (backend.UpdateStatus in
// production) and returns nil only when nodeward accepted it. A released claim returns nodeward's
// reason at once. An unreachable nodeward is retried, and if it stays unreachable the init is
// abandoned: fail closed, because the alternative is a possible second cluster.
func confirmFirstNodeClaim(report func(status string) error) error {
	var last string
	for attempt := 1; attempt <= confirmClaimAttempts; attempt++ {
		verdict, detail := classifyClaimAnswer(report(initialisingNewClusterStatus))
		switch verdict {
		case claimConfirmed:
			return nil
		case claimReleased:
			return fmt.Errorf("not initialising a cluster: %s", detail)
		}
		last = detail
		if attempt < confirmClaimAttempts {
			roslog.InstallWarning(fmt.Sprintf("Could not confirm the first-node role with nodeward (attempt %d of %d): %s", attempt, confirmClaimAttempts, detail))
			time.Sleep(confirmClaimInterval)
		}
	}
	return fmt.Errorf("not initialising a cluster: nodeward could not confirm that this node still holds the cluster's first-node role after %d attempts over %s (%s). Run `sudo runos install` again once nodeward is reachable",
		confirmClaimAttempts, time.Duration(confirmClaimAttempts)*confirmClaimInterval, last)
}
