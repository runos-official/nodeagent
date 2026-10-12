package install

import (
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/runos-official/nodeagent/commons"
	pb "github.com/runos-official/nodeagent/l2sec"
)

// FPL315 phase 8: the install command fetch retries a transient error with a
// jittered backoff, so installers that lost nodeward together do not call it
// again in the same second. The jitter has a floor (equal jitter), so the
// retry window is never shorter than the old fixed 2+4+8+16 = 30 s.

func topOfRange(n int64) int64  { return n - 1 }
func bottomOfRange(int64) int64 { return 0 }

func sum(ds []time.Duration) time.Duration {
	var t time.Duration
	for _, d := range ds {
		t += d
	}
	return t
}

type scriptedFetch struct {
	errs  []error
	calls int
}

func (s *scriptedFetch) fetch() (*pb.InstallCommandList, error) {
	s.calls++
	if s.calls <= len(s.errs) {
		return nil, s.errs[s.calls-1]
	}
	return &pb.InstallCommandList{}, nil
}

func unavailable(n int) []error {
	errs := make([]error, n)
	for i := range errs {
		errs[i] = status.Error(codes.Unavailable, "nodeward restarting")
	}
	return errs
}

func stubInstallErrorReport(t *testing.T) *int {
	t.Helper()
	reports := 0
	prev := reportInstallError
	reportInstallError = func() { reports++ }
	t.Cleanup(func() { reportInstallError = prev })
	return &reports
}

func TestFetchRetryWaitsDoubleUpToThirtySeconds(t *testing.T) {
	stubInstallErrorReport(t)
	want := []time.Duration{1, 2, 4, 8, 16, 30, 30}
	f := &scriptedFetch{errs: unavailable(len(want))}
	var sleeps []time.Duration
	res, err := fetchInstallCommands(f.fetch, commons.NewRetryBackoffWith(topOfRange), func(d time.Duration) { sleeps = append(sleeps, d) })
	if err != nil || res == nil {
		t.Fatalf("want success after %d transient errors, got %v", len(want), err)
	}
	if len(sleeps) != len(want) {
		t.Fatalf("waits = %v, want %d of them", sleeps, len(want))
	}
	for i, w := range want {
		if sleeps[i] != w*time.Second {
			t.Fatalf("waits = %v, want 1, 2, 4, 8, 16, 30, 30 s", sleeps)
		}
	}
}

// Even when every draw is at the bottom of its range, the fetch keeps retrying
// for at least the old fixed 30 s before it reports INSTALL_ERROR.
func TestFetchDoesNotGiveUpBeforeTheTimeFloor(t *testing.T) {
	reports := stubInstallErrorReport(t)
	f := &scriptedFetch{errs: unavailable(100)}
	var sleeps []time.Duration
	_, err := fetchInstallCommands(f.fetch, commons.NewRetryBackoffWith(bottomOfRange), func(d time.Duration) { sleeps = append(sleeps, d) })
	if err == nil {
		t.Fatal("want an error once the budget is spent")
	}
	if got := sum(sleeps); got < fetchRetryFloor {
		t.Fatalf("gave up after waiting %s in total (%v), want at least %s", got, sleeps, fetchRetryFloor)
	}
	if *reports != 1 {
		t.Errorf("INSTALL_ERROR reports = %d, want 1", *reports)
	}
}

// The backoff the install path really uses has that floor too.
func TestFetchInstallBackoffHasAFloor(t *testing.T) {
	stubInstallErrorReport(t)
	for run := 0; run < 200; run++ {
		f := &scriptedFetch{errs: unavailable(100)}
		var sleeps []time.Duration
		if _, err := fetchInstallCommands(f.fetch, newFetchRetryBackoff(), func(d time.Duration) { sleeps = append(sleeps, d) }); err == nil {
			t.Fatal("want an error once the budget is spent")
		}
		if got := sum(sleeps); got < fetchRetryFloor {
			t.Fatalf("run %d gave up after waiting %s in total, want at least %s", run, got, fetchRetryFloor)
		}
	}
}

func TestFetchGivesUpAfterTheRetryBudget(t *testing.T) {
	reports := stubInstallErrorReport(t)
	f := &scriptedFetch{errs: unavailable(100)}
	_, err := fetchInstallCommands(f.fetch, newFetchRetryBackoff(), func(time.Duration) {})
	if err == nil {
		t.Fatal("want an error once the budget is spent")
	}
	if f.calls != fetchMaxAttempts {
		t.Errorf("calls = %d, want %d", f.calls, fetchMaxAttempts)
	}
	if *reports != 1 {
		t.Errorf("INSTALL_ERROR reports = %d, want 1", *reports)
	}
}

func TestFetchDoesNotRetryARealRefusal(t *testing.T) {
	stubInstallErrorReport(t)
	f := &scriptedFetch{errs: []error{status.Error(codes.PermissionDenied, "no")}}
	var sleeps int
	if _, err := fetchInstallCommands(f.fetch, newFetchRetryBackoff(), func(time.Duration) { sleeps++ }); err == nil {
		t.Fatal("want the refusal returned")
	}
	if f.calls != 1 || sleeps != 0 {
		t.Errorf("calls = %d, waits = %d; want 1 and 0", f.calls, sleeps)
	}
}

func TestFetchWaitForControlPlaneKeepsItsPoll(t *testing.T) {
	stubInstallErrorReport(t)
	f := &scriptedFetch{errs: []error{status.Error(codes.FailedPrecondition, waitForControlPlanePrefix+"first node installing")}}
	var sleeps []time.Duration
	if _, err := fetchInstallCommands(f.fetch, newFetchRetryBackoff(), func(d time.Duration) { sleeps = append(sleeps, d) }); err != nil {
		t.Fatal(err)
	}
	if len(sleeps) != 1 || sleeps[0] != waitForControlPlanePoll {
		t.Fatalf("waits = %v, want one poll of %s", sleeps, waitForControlPlanePoll)
	}
}
