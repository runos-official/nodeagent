package commons

import (
	"errors"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// G30-F1: INITIALIZING_NEW_CLUSTER is the last question before `kubeadm init`. Two control planes
// that registered together on an empty cluster both ran init on 2026-08-17 (two CAs under one
// cluster id). Nodeward now hands the role to one; this is the agent's half of not initialising
// without it.

func TestAnAcceptedReportIsAConfirmedClaim(t *testing.T) {
	if v, _ := classifyClaimAnswer(nil); v != claimConfirmed {
		t.Fatalf("nil error is confirmed, got %v", v)
	}
}

func TestNodewardsRefusalIsARelease(t *testing.T) {
	err := status.Error(codes.FailedPrecondition, firstNodeClaimReleasedPrefix+"this node no longer holds cluster cl1's first-node role, n02-cl1-acct1 does")
	v, detail := classifyClaimAnswer(err)
	if v != claimReleased {
		t.Fatalf("FailedPrecondition with the prefix is a release, got %v", v)
	}
	if !strings.Contains(detail, "n02-cl1-acct1") || strings.HasPrefix(detail, firstNodeClaimReleasedPrefix) {
		t.Fatalf("the detail should be nodeward's reason without the prefix: %q", detail)
	}
}

func TestAnyOtherFailureIsUnknownNotARelease(t *testing.T) {
	// A FailedPrecondition WITHOUT the prefix must not read as a release: an unrelated refusal
	// would otherwise abort every install with the wrong story. And an unreachable nodeward is
	// unknown, which is retried and then fails closed.
	for _, err := range []error{
		status.Error(codes.FailedPrecondition, "something else entirely"),
		status.Error(codes.Unavailable, "connection refused"),
		errors.New("plain error"),
	} {
		if v, _ := classifyClaimAnswer(err); v != claimUnknown {
			t.Fatalf("%v should be unknown, got %v", err, v)
		}
	}
}

func TestConfirmAbortsAtOnceOnARelease(t *testing.T) {
	calls := 0
	err := confirmFirstNodeClaim(func(s string) error {
		calls++
		if s != initialisingNewClusterStatus {
			t.Fatalf("must report %s, reported %s", initialisingNewClusterStatus, s)
		}
		return status.Error(codes.FailedPrecondition, firstNodeClaimReleasedPrefix+"released")
	})
	if err == nil || calls != 1 {
		t.Fatalf("a release aborts on the first answer without retrying: err=%v calls=%d", err, calls)
	}
	if !strings.Contains(err.Error(), "not initialising") {
		t.Fatalf("the error must say the init is not happening: %q", err)
	}
}

func TestConfirmSucceedsWhenNodewardAccepts(t *testing.T) {
	if err := confirmFirstNodeClaim(func(string) error { return nil }); err != nil {
		t.Fatalf("an accepted report confirms the claim: %v", err)
	}
}
