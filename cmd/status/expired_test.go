package status

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/runos-official/nodeagent/backend"
	"github.com/runos-official/nodeagent/roslog"
	"github.com/runos-official/nodeagent/uc/certpolicy"
)

// captureStd runs f with os.Stdout and os.Stderr sent to one buffer.
func captureStd(t *testing.T, f func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	oldOut, oldErr := os.Stdout, os.Stderr
	os.Stdout, os.Stderr = w, w
	done := make(chan string)
	go func() {
		b, _ := io.ReadAll(r)
		done <- string(b)
	}()
	defer func() {
		os.Stdout, os.Stderr = oldOut, oldErr
	}()
	f()
	w.Close()
	os.Stdout, os.Stderr = oldOut, oldErr
	return <-done
}

// noHeldCertificateStep fails when text gives advice that needs the held
// certificate or a connection (criterion 7).
func noHeldCertificateStep(t *testing.T, what, text string) {
	t.Helper()
	low := strings.ToLower(text)
	for _, bad := range []string{"certificate renew", "retry", "check connectivity", "renewal recommended", "runos test", "0 days"} {
		if strings.Contains(low, bad) {
			t.Errorf("%s contains %q:\n%s", what, bad, text)
		}
	}
}

// TestStatusExpiredFifteenMinutesAgo (criterion 9): a certificate that expired
// 15 minutes ago is expired in the report and in the JSON, and the human
// line and the failure cause give the expiry time with the one remedy.
func TestStatusExpiredFifteenMinutesAgo(t *testing.T) {
	now := time.Date(2031, 2, 3, 4, 20, 6, 0, time.UTC)
	notAfter := now.Add(-15 * time.Minute)
	stamp := notAfter.Format(time.RFC3339)

	c := certReport("/etc/runos/mtls.crt", notAfter, now)
	if !c.Expired || c.DaysRemaining != 0 {
		t.Fatalf("report = %+v, want expired with 0 whole days", c)
	}
	r := statusReport{
		NodewardHost:    "nodeward.example.test",
		Cert:            c,
		Degraded:        true,
		ConnectionError: "connection failed: " + (&backend.CertificateExpiredError{NotAfter: notAfter}).Error(),
	}

	b, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	cert := m["cert"].(map[string]any)
	if cert["expired"] != true || cert["daysRemaining"] != float64(0) {
		t.Fatalf("JSON cert = %v, want expired true", cert)
	}
	for _, k := range []string{"path", "expires", "daysRemaining", "expired"} {
		if _, ok := cert[k]; !ok {
			t.Fatalf("JSON cert lost the member %s: %v", k, cert)
		}
	}

	var logBuf bytes.Buffer
	old := roslog.L
	roslog.L = slog.New(slog.NewJSONHandler(&logBuf, nil))
	defer func() { roslog.L = old }()

	var failErr error
	out := captureStd(t, func() {
		renderHuman(r)
		failErr = failForDegraded(r)
	})
	if !strings.Contains(out, "Certificate EXPIRED on "+stamp) {
		t.Fatalf("human output lacks the expiry time %s:\n%s", stamp, out)
	}
	wantCause := "certificate at /etc/runos/mtls.crt expired on " + stamp
	if failErr == nil || !strings.Contains(failErr.Error(), wantCause) {
		t.Fatalf("failure = %v, want the cause %q", failErr, wantCause)
	}
	var failLine map[string]any
	for _, l := range strings.Split(strings.TrimSpace(logBuf.String()), "\n") {
		var e map[string]any
		if json.Unmarshal([]byte(l), &e) == nil && e["step"] != nil {
			failLine = e
		}
	}
	if failLine == nil || failLine["remedy"] != certpolicy.ExpiredRemedy(notAfter) || failLine["cause"] != wantCause {
		t.Fatalf("failure line = %v, want the cause and the expired remedy", failLine)
	}
	noHeldCertificateStep(t, "runos status output", out+"\n"+logBuf.String())
}

// TestStatusNotExpiredOneSecondBefore: the instant rule does not mark a
// certificate expired before its NotAfter.
func TestStatusNotExpiredOneSecondBefore(t *testing.T) {
	now := time.Date(2031, 2, 3, 4, 20, 6, 0, time.UTC)
	if c := certReport("p", now.Add(time.Second), now); c.Expired {
		t.Fatal("a certificate one second before NotAfter is expired")
	}
	if c := certReport("p", now, now); !c.Expired {
		t.Fatal("a certificate at NotAfter is not expired")
	}
}

// TestConnectionSummaryNamesARefusedCertificate: a TLS refusal of the client
// certificate names the reason and does not offer the connection test.
func TestConnectionSummaryNamesARefusedCertificate(t *testing.T) {
	r := statusReport{NodewardHost: "nodeward.example.test", ConnectionError: "x", certificateRefused: "bad certificate"}
	cause, remedy := connectionSummary(r)
	if !strings.Contains(cause, "bad certificate") {
		t.Fatalf("cause = %q", cause)
	}
	noHeldCertificateStep(t, "remedy", remedy)
}
