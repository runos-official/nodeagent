package roslog

import (
	"bytes"
	"errors"
	"fmt"
	"strings"
	"testing"
)

func TestFail_WritesBlockAndReturnsError(t *testing.T) {
	var buf bytes.Buffer
	orig := stderr
	stderr = &buf
	defer func() { stderr = orig }()

	err := Fail("Register node", "token expired", "copy a fresh join command")
	if err == nil {
		t.Fatal("Fail returned nil error")
	}
	if !strings.Contains(err.Error(), "Register node") || !strings.Contains(err.Error(), "token expired") {
		t.Fatalf("error %q missing step/cause", err.Error())
	}

	out := buf.String()
	for _, want := range []string{"FAILED:", "Register node", "Cause: token expired", "Try:   copy a fresh join command", "Full log:"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q\n---\n%s", want, out)
		}
	}
}

func TestFail_OmitsEmptyCauseAndRemedy(t *testing.T) {
	var buf bytes.Buffer
	orig := stderr
	stderr = &buf
	defer func() { stderr = orig }()

	_ = Fail("Some step", "", "")
	out := buf.String()
	if strings.Contains(out, "Cause:") {
		t.Errorf("unexpected Cause line for empty cause:\n%s", out)
	}
	if strings.Contains(out, "Try:") {
		t.Errorf("unexpected Try line for empty remedy:\n%s", out)
	}
}

// Pins the "exactly one failure block" contract the whole CLI relies on: an
// error from Fail (or AlreadyReported) is detectable as already-reported, even
// after %w wrapping, so main.go never prints a second generic error line on top
// of the canonical block. A plain error must NOT be flagged, and nil stays nil.
func TestAlreadyReported(t *testing.T) {
	// Quiet Fail's stderr block; we only care about the returned error here.
	orig := stderr
	stderr = &bytes.Buffer{}
	defer func() { stderr = orig }()

	// Fail's returned error is already-reported.
	failErr := Fail("step", "cause", "remedy")
	if !IsAlreadyReported(failErr) {
		t.Fatal("Fail() error should be already-reported")
	}

	// AlreadyReported survives %w wrapping (errors.As walks the chain).
	wrapped := fmt.Errorf("outer context: %w", AlreadyReported(errors.New("boom")))
	if !IsAlreadyReported(wrapped) {
		t.Fatal("already-reported error wrapped with %w should still be detected")
	}

	// A plain error is NOT already-reported (so main.go DOES print its block).
	if IsAlreadyReported(errors.New("unreported boom")) {
		t.Fatal("a plain error must not be reported as already-reported")
	}

	// nil in, nil out (no spurious wrapper around a nil error).
	if AlreadyReported(nil) != nil {
		t.Fatal("AlreadyReported(nil) must be nil")
	}
	if IsAlreadyReported(nil) {
		t.Fatal("IsAlreadyReported(nil) must be false")
	}

	// The wrapper preserves the underlying message (Error()/Unwrap()).
	inner := errors.New("inner message")
	if got := AlreadyReported(inner).Error(); got != "inner message" {
		t.Fatalf("AlreadyReported wrapper Error() = %q, want %q", got, "inner message")
	}
	if !errors.Is(AlreadyReported(inner), inner) {
		t.Fatal("AlreadyReported must unwrap to the original error")
	}
}
