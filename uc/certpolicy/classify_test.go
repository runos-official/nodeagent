package certpolicy

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	spb "google.golang.org/genproto/googleapis/rpc/status"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/anypb"
)

func TestMintCheckFailureIsTransientUnderBothCodes(t *testing.T) {
	for _, code := range []codes.Code{codes.FailedPrecondition, codes.Unavailable} {
		o := Classify(status.Error(code, "could not check the minting rules"), Prior{})
		if o.Kind != Transient || o.Unclassified {
			t.Errorf("%s: %+v, want Transient and classified", code, o)
		}
		if o.Code != ReasonMintCheckUnavailable {
			t.Errorf("%s: code %q", code, o.Code)
		}
	}
}

func TestReasonAloneDecides(t *testing.T) {
	retry := map[string]string{"retry_after": "2031-02-03T04:05:06Z"}
	cases := []struct {
		reason string
		meta   map[string]string
		kind   Kind
	}{
		{ReasonRenewalNotOpen, retry, WaitUntil},
		{ReasonRepeatBlocked, retry, WaitUntil},
		{ReasonAlreadyRegenerated, nil, TerminalForThisCertificate},
		{ReasonMintCheckUnavailable, nil, Transient},
		{ReasonLegacyMintOff, nil, NeedsAgentUpgrade},
		{ReasonCSRRequired, nil, NeedsAgentUpgrade},
		{ReasonTestRenewalRefused, nil, Transient},
	}
	for _, c := range cases {
		// The code is one that alone would give Transient Unclassified,
		// so only the reason can give the kind.
		o := Classify(withInfo(t, codes.Aborted, "", Domain, c.reason, c.meta), Prior{})
		if o.Kind != c.kind || o.Unclassified {
			t.Errorf("%s: kind %s unclassified=%v, want %s", c.reason, o.Kind, o.Unclassified, c.kind)
		}
		if o.Code != c.reason {
			t.Errorf("%s: code %q", c.reason, o.Code)
		}
		if o.ResponseMayBeLost {
			t.Errorf("%s: a typed refusal proves Nodeward answered before the mint", c.reason)
		}
	}
	if len(cases) != len(knownReasons) {
		t.Fatalf("test covers %d reasons, package knows %d", len(cases), len(knownReasons))
	}
}

func TestOtherDomainIgnored(t *testing.T) {
	o := Classify(withInfo(t, codes.Aborted, "", "example.com", ReasonLegacyMintOff, nil), Prior{})
	if o.Kind != Transient || !o.Unclassified {
		t.Fatalf("another domain must be ignored: %+v", o)
	}
	// With a text, the text row decides.
	o = Classify(withInfo(t, codes.FailedPrecondition, "certificate was already regenerated", "example.com", ReasonLegacyMintOff, nil), Prior{})
	if o.Kind != TerminalForThisCertificate {
		t.Fatalf("another domain must not override the text: %+v", o)
	}
}

func TestMalformedDetailDoesNotPanic(t *testing.T) {
	p := &spb.Status{
		Code:    int32(codes.FailedPrecondition),
		Message: "legacy key minting through nodehub is turned off; upgrade",
		Details: []*anypb.Any{
			{TypeUrl: "type.googleapis.com/google.rpc.ErrorInfo", Value: []byte{0xff, 0xff, 0xff}},
			{TypeUrl: "type.googleapis.com/no.such.Type", Value: []byte{1, 2, 3}},
			nil,
		},
	}
	var o Outcome
	func() {
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("panic: %v", r)
			}
		}()
		o = Classify(status.FromProto(p).Err(), Prior{})
	}()
	if o.Kind != NeedsAgentUpgrade {
		t.Fatalf("text path must still decide: %+v", o)
	}
}

func TestWaitUntilTime(t *testing.T) {
	want := time.Date(2031, 2, 3, 4, 5, 6, 0, time.UTC)
	texts := []string{
		"certificate is not inside its renewal window; renewal opens at 2031-02-03T04:05:06Z; retrying earlier will not help",
		"certificate was regenerated too recently; renew again after 2031-02-03T04:05:06Z",
	}
	for _, text := range texts {
		o := Classify(status.Error(codes.FailedPrecondition, text), Prior{})
		if o.Kind != WaitUntil || !o.RetryAt.Equal(want) {
			t.Errorf("%q: %s at %v, want WaitUntil at %v", text, o.Kind, o.RetryAt, want)
		}
	}
	for _, reason := range []string{ReasonRenewalNotOpen, ReasonRepeatBlocked} {
		for _, v := range []string{"2031-02-03T04:05:06Z", fmt.Sprint(want.Unix())} {
			o := Classify(withInfo(t, codes.FailedPrecondition, "", Domain, reason, map[string]string{"retry_after": v}), Prior{})
			if o.Kind != WaitUntil || !o.RetryAt.Equal(want) {
				t.Errorf("%s retry_after %q: %s at %v", reason, v, o.Kind, o.RetryAt)
			}
			if o.RetryAt.Location() != time.UTC {
				t.Errorf("%s retry_after %q: time not in UTC", reason, v)
			}
		}
	}
	// retry_after wins over the text.
	o := Classify(withInfo(t, codes.FailedPrecondition, texts[0], Domain, ReasonRenewalNotOpen,
		map[string]string{"retry_after": "2032-01-01T00:00:00Z"}), Prior{})
	if !o.RetryAt.Equal(time.Date(2032, 1, 1, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("retry_after must win: %v", o.RetryAt)
	}

	// No time that parses: Transient Unclassified, never an invented time.
	bad := []error{
		status.Error(codes.FailedPrecondition, "certificate is not inside its renewal window; renewal opens at soon"),
		status.Error(codes.FailedPrecondition, "certificate was regenerated too recently"),
		withInfo(t, codes.FailedPrecondition, "", Domain, ReasonRenewalNotOpen, map[string]string{"retry_after": "tomorrow"}),
		withInfo(t, codes.FailedPrecondition, "", Domain, ReasonRepeatBlocked, nil),
		withInfo(t, codes.FailedPrecondition, "", Domain, ReasonRepeatBlocked, map[string]string{"retry_after": "-5"}),
	}
	for _, err := range bad {
		o := Classify(err, Prior{})
		if o.Kind != Transient || !o.Unclassified || !o.RetryAt.IsZero() {
			t.Errorf("%v: %+v, want Transient Unclassified with no time", err, o)
		}
	}
	// A broken retry_after falls back to the text time.
	o = Classify(withInfo(t, codes.FailedPrecondition, texts[1], Domain, ReasonRepeatBlocked,
		map[string]string{"retry_after": "garbage"}), Prior{})
	if o.Kind != WaitUntil || !o.RetryAt.Equal(want) {
		t.Errorf("text time fallback: %+v", o)
	}
}

func TestLostResponse(t *testing.T) {
	text := "certificate was already regenerated; the agent must use the certificate from that renewal"
	if o := Classify(status.Error(codes.FailedPrecondition, text), Prior{ResponseMayBeLost: true}); o.Kind != LostResponse {
		t.Errorf("text path with prior: %s", o.Kind)
	}
	if o := Classify(status.Error(codes.FailedPrecondition, text), Prior{}); o.Kind != TerminalForThisCertificate {
		t.Errorf("text path without prior: %s", o.Kind)
	}
	typed := withInfo(t, codes.FailedPrecondition, "", Domain, ReasonAlreadyRegenerated, nil)
	if o := Classify(typed, Prior{ResponseMayBeLost: true}); o.Kind != LostResponse {
		t.Errorf("reason path with prior: %s", o.Kind)
	}

	lost := []error{
		status.Error(codes.Unavailable, "connection reset by peer"),
		status.Error(codes.DeadlineExceeded, "context deadline exceeded"),
		status.Error(codes.Unknown, "something odd"),
		errors.New("not a status"),
		status.Error(codes.Unavailable, "could not check the minting rules"),
	}
	for _, err := range lost {
		if o := Classify(err, Prior{}); !o.ResponseMayBeLost || o.Kind != Transient {
			t.Errorf("%v: %+v, want Transient with ResponseMayBeLost", err, o)
		}
	}
	kept := []error{
		status.Error(codes.FailedPrecondition, "could not check the minting rules"),
		status.Error(codes.Unavailable, "could not check the mint proof"),
		status.Error(codes.Unavailable, "authentication check failed"),
		status.Error(codes.ResourceExhausted, "too many calls"),
		status.Error(codes.PermissionDenied, "mint proof nonce was already used"),
		status.Error(codes.Unauthenticated, "certificate has expired"),
	}
	for _, err := range kept {
		if o := Classify(err, Prior{}); o.ResponseMayBeLost {
			t.Errorf("%v: a refusal before the mint must not set ResponseMayBeLost", err)
		}
	}
}

func TestFallbackByCode(t *testing.T) {
	for _, code := range []codes.Code{codes.Unavailable, codes.ResourceExhausted, codes.DeadlineExceeded} {
		o := Classify(status.Error(code, "no row names this text"), Prior{})
		if o.Kind != Transient || o.Unclassified {
			t.Errorf("%s: %+v, want classified Transient", code, o)
		}
	}
	for _, code := range []codes.Code{codes.Unknown, codes.Internal, codes.Aborted, codes.Unauthenticated, codes.InvalidArgument, codes.FailedPrecondition} {
		o := Classify(status.Error(code, "no row names this text"), Prior{})
		if o.Kind != Transient || !o.Unclassified {
			t.Errorf("%s: %+v, want Transient Unclassified", code, o)
		}
		if o.Code != code.String() {
			t.Errorf("%s: code word %q", code, o.Code)
		}
	}
	o := Classify(errors.New("dial tcp: refused"), Prior{})
	if o.Kind != Transient || !o.Unclassified {
		t.Errorf("non-status error: %+v", o)
	}
}

func TestWrappedStatusKeepsText(t *testing.T) {
	err := fmt.Errorf("regenerate certificate RPC failed: %w", status.Error(codes.Unauthenticated, "certificate has expired"))
	o := Classify(err, Prior{})
	if o.Kind != CertificateExpired {
		t.Fatalf("wrapped status: %+v", o)
	}
	if o.Message != "certificate has expired" {
		t.Fatalf("message %q", o.Message)
	}
	if o.GRPCCode != codes.Unauthenticated {
		t.Fatalf("grpc code %s", o.GRPCCode)
	}
}

func TestMessageIsCut(t *testing.T) {
	long := strings.Repeat("é", 400) // 800 bytes
	o := Classify(status.Error(codes.Internal, long), Prior{})
	if len(o.Message) > 512 {
		t.Fatalf("message is %d bytes", len(o.Message))
	}
	if !strings.HasPrefix(long, o.Message) || len(o.Message) == 0 {
		t.Fatal("cut message is not a prefix on a rune boundary")
	}
}

func TestKindRetriesAndReportResult(t *testing.T) {
	want := map[Kind]struct {
		retries bool
		result  string
	}{
		WaitUntil:                  {true, "waiting"},
		Transient:                  {true, "transient_failure"},
		LostResponse:               {false, "refused"},
		TerminalForThisCertificate: {false, "refused"},
		NeedsAgentUpgrade:          {false, "refused"},
		CertificateExpired:         {false, "expired"},
		IdentityRepair:             {false, "refused"},
		BadRequest:                 {false, "refused"},
	}
	if len(want) != len(AllKinds()) {
		t.Fatalf("AllKinds has %d kinds", len(AllKinds()))
	}
	for _, k := range AllKinds() {
		w, ok := want[k]
		if !ok {
			t.Fatalf("kind %s not in the test", k)
		}
		if k.Retries() != w.retries {
			t.Errorf("%s.Retries() = %v", k, k.Retries())
		}
		if k.ReportResult() != w.result {
			t.Errorf("%s.ReportResult() = %q", k, k.ReportResult())
		}
		if k.String() == "" || strings.HasPrefix(k.String(), "Kind(") {
			t.Errorf("kind %d has no name", int(k))
		}
	}
}

func TestOutcomeCarriesNextStep(t *testing.T) {
	o := Classify(status.Error(codes.FailedPrecondition, "certificate was regenerated too recently; renew again after 2031-02-03T04:05:06Z"), Prior{})
	if o.NextStep != NextStep(WaitUntil, o.RetryAt) {
		t.Fatalf("next step %q", o.NextStep)
	}
	if !strings.Contains(o.NextStep, "2031-02-03T04:05:06Z") {
		t.Fatalf("WaitUntil text must name the time: %q", o.NextStep)
	}
}
