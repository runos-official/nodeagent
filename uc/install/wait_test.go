package install

import (
	"errors"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// G30-F1: a node that finds no ready control plane WAITS for the first one instead of failing, and
// only when nodeward says so with the agreed pair (FailedPrecondition + prefix).

func TestTheWaitSignalIsCodePlusPrefix(t *testing.T) {
	reason, ok := isWaitForControlPlane(status.Error(codes.FailedPrecondition, waitForControlPlanePrefix+"n01 is still installing"))
	if !ok || reason != "n01 is still installing" {
		t.Fatalf("expected the wait with its reason, got ok=%v reason=%q", ok, reason)
	}
	for _, err := range []error{
		status.Error(codes.FailedPrecondition, "no peers in ready state"), // old prose, no prefix
		status.Error(codes.Unavailable, waitForControlPlanePrefix+"x"),    // right words, wrong code
		errors.New(waitForControlPlanePrefix + "not a status error"),
	} {
		if _, ok := isWaitForControlPlane(err); ok {
			t.Fatalf("%v must not read as a wait", err)
		}
	}
}

func TestWaitingGivesUpOnlyPastTheBound(t *testing.T) {
	if decideWait(29*time.Minute, waitForControlPlaneBound) != waitAgain {
		t.Fatal("29 minutes into a 30 minute bound the node keeps waiting")
	}
	if decideWait(30*time.Minute, waitForControlPlaneBound) != waitGiveUp {
		t.Fatal("at the bound the node gives up with nodeward's last reason")
	}
}
