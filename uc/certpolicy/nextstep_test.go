package certpolicy

import (
	"strings"
	"testing"
	"time"
)

func TestNextStepTexts(t *testing.T) {
	at := time.Date(2031, 2, 3, 4, 5, 6, 0, time.UTC)
	for _, k := range AllKinds() {
		text := NextStep(k, at)
		if strings.TrimSpace(text) == "" {
			t.Errorf("%s: empty next step", k)
		}
		hasRetry := strings.Contains(strings.ToLower(text), "retry") || strings.Contains(strings.ToLower(text), "retries")
		if !k.Retries() && hasRetry {
			t.Errorf("%s does not retry, but its text says so: %q", k, text)
		}
		if k.Retries() && !hasRetry {
			t.Errorf("%s retries, and its text must say so: %q", k, text)
		}
		if strings.Contains(text, "runos certificate renew") {
			t.Errorf("%s names the manual renew command: %q", k, text)
		}
	}
	if NextStep(CertificateExpired, at) != ExpiredRemedy(at) {
		t.Error("the CertificateExpired text must be ExpiredRemedy")
	}
	if !strings.Contains(ExpiredRemedy(at), "2031-02-03T04:05:06Z") {
		t.Errorf("remedy must name the expiry time: %q", ExpiredRemedy(at))
	}
	if !strings.Contains(NextStep(WaitUntil, at), "2031-02-03T04:05:06Z") {
		t.Errorf("WaitUntil must name the time: %q", NextStep(WaitUntil, at))
	}
	// A zero time gives a text with no year-one date.
	for _, k := range []Kind{CertificateExpired, WaitUntil} {
		if strings.Contains(NextStep(k, time.Time{}), "0001") {
			t.Errorf("%s with no time: %q", k, NextStep(k, time.Time{}))
		}
	}
	remedy := strings.ToLower(ExpiredRemedy(at))
	for _, part := range []string{"expired", "re-issue", "remove the node", "register", "new token", "console", "new identity"} {
		if !strings.Contains(remedy, part) {
			t.Errorf("remedy lacks %q: %q", part, ExpiredRemedy(at))
		}
	}
}
