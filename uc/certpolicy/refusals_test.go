package certpolicy

import (
	"fmt"
	"sort"
	"strings"
	"testing"

	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// exampleText is a full refusal text that starts with the row prefix, as
// Nodeward writes it. Rows with a time carry one.
func exampleText(r Refusal) string {
	switch r.Reason {
	case ReasonRenewalNotOpen:
		return r.Prefix + "2031-02-03T04:05:06Z; retrying earlier will not help"
	case ReasonRepeatBlocked:
		return r.Prefix + "2031-02-03T04:05:06Z"
	case ReasonAlreadyRegenerated:
		return r.Prefix + "; the agent must use the certificate from that renewal; retrying with this certificate will not help"
	}
	switch {
	case r.Prefix == "invalid certificate signing request: ":
		return r.Prefix + "key is not ECDSA P-256"
	case strings.HasPrefix(r.Prefix, "mint proof time is outside"):
		return r.Prefix + "; server time 1924920306"
	case r.Prefix == "":
		return "too many calls in flight"
	}
	return r.Prefix
}

// withInfo attaches one ErrorInfo to the status of code and text.
func withInfo(t *testing.T, code codes.Code, text, domain, reason string, meta map[string]string) error {
	t.Helper()
	st, err := status.New(code, text).WithDetails(&errdetails.ErrorInfo{Domain: domain, Reason: reason, Metadata: meta})
	if err != nil {
		t.Fatalf("attach ErrorInfo: %v", err)
	}
	return st.Err()
}

func TestRefusalTableCoversFinding360(t *testing.T) {
	items := map[int]bool{}
	for _, r := range Refusals() {
		if r.Item != 0 {
			items[r.Item] = true
		}
		name := fmt.Sprintf("item%d/%s/%q", r.Item, r.Code, r.Prefix)
		t.Run(name, func(t *testing.T) {
			text := exampleText(r)
			plain := Classify(status.Error(r.Code, text), Prior{})
			if plain.Kind != r.Kind {
				t.Fatalf("text path: kind %s, want %s", plain.Kind, r.Kind)
			}
			if plain.Unclassified {
				t.Fatalf("text path: a table row must not be unclassified")
			}
			// The second form: with the K2 reason where the row has one,
			// else with an ErrorInfo whose reason no agent knows.
			reason := r.Reason
			if reason == "" {
				reason = "CERT_SOMETHING_NEW"
			}
			var meta map[string]string
			if r.Kind == WaitUntil {
				meta = map[string]string{"retry_after": "2031-02-03T04:05:06Z"}
			}
			typed := Classify(withInfo(t, r.Code, text, Domain, reason, meta), Prior{})
			if typed.Kind != plain.Kind {
				t.Fatalf("H2: typed form gives %s, text form gives %s", typed.Kind, plain.Kind)
			}
			if typed.Code != plain.Code {
				t.Fatalf("H2: typed code %q, text code %q", typed.Code, plain.Code)
			}
			if typed.ResponseMayBeLost != plain.ResponseMayBeLost {
				t.Fatalf("H2: typed lost %v, text lost %v", typed.ResponseMayBeLost, plain.ResponseMayBeLost)
			}
			if !typed.RetryAt.Equal(plain.RetryAt) {
				t.Fatalf("H2: typed retry %v, text retry %v", typed.RetryAt, plain.RetryAt)
			}
		})
	}
	var got []int
	for i := range items {
		got = append(got, i)
	}
	sort.Ints(got)
	if len(got) != 16 {
		t.Fatalf("items in the table: %v, want 1 to 16", got)
	}
	for i, n := range got {
		if n != i+1 {
			t.Fatalf("items in the table: %v, want 1 to 16", got)
		}
	}

	// The K5 test refusal row.
	k5 := Classify(status.Error(codes.FailedPrecondition, "certificate renewal is refused by a test setting"), Prior{})
	if k5.Kind != Transient || k5.Unclassified {
		t.Fatalf("K5 row: %+v, want Transient and classified", k5)
	}
	if k5.Code != ReasonTestRenewalRefused {
		t.Fatalf("K5 row code %q", k5.Code)
	}
}

// TestRefusalTableKinds pins the kind of each item, so a row edit that
// changes a kind fails here and not only in review.
func TestRefusalTableKinds(t *testing.T) {
	want := map[string]Kind{
		"certificate is not inside its renewal window; renewal opens at 2031-02-03T04:05:06Z":  WaitUntil,
		"certificate was regenerated too recently; renew again after 2031-02-03T04:05:06Z":     WaitUntil,
		"certificate was already regenerated; the agent must use ...":                          TerminalForThisCertificate,
		"legacy key minting through nodehub is turned off; send a certificate signing request": NeedsAgentUpgrade,
		"this agent must renew with a certificate signing request; run an agent build":         NeedsAgentUpgrade,
	}
	for text, kind := range want {
		if got := Classify(status.Error(codes.FailedPrecondition, text), Prior{}).Kind; got != kind {
			t.Errorf("%q: kind %s, want %s", text, got, kind)
		}
	}
	cases := []struct {
		code codes.Code
		text string
		kind Kind
	}{
		{codes.InvalidArgument, "mint proof sent without a certificate signing request", BadRequest},
		{codes.InvalidArgument, "invalid certificate signing request: does not parse", BadRequest},
		{codes.InvalidArgument, "invalid certificate signing request: key is the presented certificate key", BadRequest},
		{codes.InvalidArgument, "mint proof is missing or malformed", BadRequest},
		{codes.PermissionDenied, "mint proof signature does not verify", BadRequest},
		{codes.FailedPrecondition, "mint proof time is outside the 5 minute window; server time 1", Transient},
		{codes.Unavailable, "could not check the mint proof", Transient},
		{codes.PermissionDenied, "mint proof nonce was already used", Transient},
		{codes.Unauthenticated, "certificate has expired", CertificateExpired},
		{codes.Unauthenticated, "certificate not found in database", IdentityRepair},
		{codes.Unauthenticated, "certificate has been revoked", IdentityRepair},
		{codes.Unauthenticated, "unknown certificate version", IdentityRepair},
		{codes.Unauthenticated, "invalid legacy certificate format", IdentityRepair},
		{codes.NotFound, "node not found", IdentityRepair},
		{codes.Unavailable, "authentication check failed", Transient},
		{codes.ResourceExhausted, "", Transient},
		{codes.Unavailable, "", Transient},
	}
	for _, c := range cases {
		o := Classify(status.Error(c.code, c.text), Prior{})
		if o.Kind != c.kind || o.Unclassified {
			t.Errorf("%s %q: %s unclassified=%v, want %s", c.code, c.text, o.Kind, o.Unclassified, c.kind)
		}
	}
}

// TestLongerPrefixFirst keeps item 9 ahead of item 8.
func TestLongerPrefixFirst(t *testing.T) {
	rows := Refusals()
	at := func(prefix string) int {
		for i, r := range rows {
			if r.Prefix == prefix {
				return i
			}
		}
		t.Fatalf("no row %q", prefix)
		return -1
	}
	if at("invalid certificate signing request: key is the presented certificate key") > at("invalid certificate signing request: ") {
		t.Fatal("item 9 must come before item 8")
	}
	// Rows with an empty prefix match by code only, so they come last.
	seenEmpty := false
	for _, r := range rows {
		if r.Prefix == "" {
			seenEmpty = true
		} else if seenEmpty {
			t.Fatalf("row %q follows an empty-prefix row", r.Prefix)
		}
	}
}
