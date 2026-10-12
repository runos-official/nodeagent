package install

import (
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	pb "github.com/runos-official/nodeagent/l2sec"
)

// FPL315 phase 8: the install command fetch retries a transient error with the
// jittered reconnect backoff, so installers that lost nodeward together do not
// call it again in the same second.

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

func TestFetchRetriesTransientErrorsWithJitteredWaits(t *testing.T) {
	stubInstallErrorReport(t)
	caps := []time.Duration{1, 2, 4, 8, 16, 30, 30}
	for run := 0; run < 50; run++ {
		f := &scriptedFetch{errs: unavailable(len(caps))}
		var sleeps []time.Duration
		res, err := fetchInstallCommands(f.fetch, func(d time.Duration) { sleeps = append(sleeps, d) })
		if err != nil || res == nil {
			t.Fatalf("want success after %d transient errors, got %v", len(caps), err)
		}
		if len(sleeps) != len(caps) {
			t.Fatalf("got %d waits, want %d", len(sleeps), len(caps))
		}
		for i, d := range sleeps {
			if d < 0 || d > caps[i]*time.Second {
				t.Fatalf("wait %d = %s, outside [0, %s]", i+1, d, caps[i]*time.Second)
			}
		}
	}
}

func TestFetchGivesUpAfterTheRetryBudget(t *testing.T) {
	reports := stubInstallErrorReport(t)
	f := &scriptedFetch{errs: unavailable(100)}
	_, err := fetchInstallCommands(f.fetch, func(time.Duration) {})
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
	if _, err := fetchInstallCommands(f.fetch, func(time.Duration) { sleeps++ }); err == nil {
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
	if _, err := fetchInstallCommands(f.fetch, func(d time.Duration) { sleeps = append(sleeps, d) }); err != nil {
		t.Fatal(err)
	}
	if len(sleeps) != 1 || sleeps[0] != waitForControlPlanePoll {
		t.Fatalf("waits = %v, want one poll of %s", sleeps, waitForControlPlanePoll)
	}
}
