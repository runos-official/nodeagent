package certificate

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/spf13/viper"

	"github.com/runos-official/nodeagent/backend"
	"github.com/runos-official/nodeagent/roslog"
	"github.com/runos-official/nodeagent/uc/certificate"
	"github.com/runos-official/nodeagent/uc/certpolicy"
	"github.com/runos-official/nodeagent/uc/csrcert/csrtest"
)

// noHeldCertificateStep fails when text gives advice that needs the held
// certificate or a connection (criterion 7).
func noHeldCertificateStep(t *testing.T, what, text string) {
	t.Helper()
	low := strings.ToLower(text)
	for _, bad := range []string{"certificate renew", "retry", "check connectivity", "renewal recommended"} {
		if strings.Contains(low, bad) {
			t.Errorf("%s contains %q:\n%s", what, bad, text)
		}
	}
}

// useExpiredPair writes a matching pair that expired at notAfter as the
// configured credential files.
func useExpiredPair(t *testing.T, notAfter time.Time) {
	t.Helper()
	ca := csrtest.NewCA(t)
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(k)
	if err != nil {
		t.Fatal(err)
	}
	opts := csrtest.LeafOptions{DNSNames: []string{"acct-a.example.test"}, Version: 2, Type: 1,
		NotBefore: notAfter.Add(-48 * time.Hour), NotAfter: notAfter}
	dir := t.TempDir()
	files := map[string][]byte{
		"mtls.crt": []byte(ca.LeafWith(t, k.Public(), opts)),
		"mtls.key": pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}),
		"ca.crt":   []byte(ca.PEM),
	}
	for name, b := range files {
		if err := os.WriteFile(filepath.Join(dir, name), b, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for k, v := range map[string]string{
		"mtls.crt": filepath.Join(dir, "mtls.crt"), "mtls.key": filepath.Join(dir, "mtls.key"),
		"mtls.ca": filepath.Join(dir, "ca.crt"), "client.server.nodeward": "127.0.0.1",
	} {
		old := viper.Get(k)
		viper.Set(k, v)
		t.Cleanup(func() { viper.Set(k, old) })
	}
}

func captureStdout(t *testing.T, f func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stdout
	os.Stdout = w
	done := make(chan string)
	go func() {
		b, _ := io.ReadAll(r)
		done <- string(b)
	}()
	defer func() { os.Stdout = old }()
	f()
	w.Close()
	os.Stdout = old
	return <-done
}

func captureLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	old := roslog.L
	roslog.L = slog.New(slog.NewJSONHandler(&buf, nil))
	t.Cleanup(func() { roslog.L = old })
	return &buf
}

// TestCertificateStatusReportsExpired (criterion 7): the human and the JSON
// output of an expired certificate, with exit code 0.
func TestCertificateStatusReportsExpired(t *testing.T) {
	now := time.Date(2031, 2, 3, 4, 20, 6, 0, time.UTC)
	notAfter := now.Add(-15 * time.Minute)
	timing := certpolicy.Evaluate(notAfter.Add(-48*time.Hour), notAfter, "node-under-test", now)

	var human bytes.Buffer
	if err := writeStatus(&human, false, timing, nil, now); err != nil {
		t.Fatalf("human status: exit error %v, want 0", err)
	}
	want := "Certificate EXPIRED on " + notAfter.Format(time.RFC3339)
	if !strings.Contains(human.String(), want) || !strings.Contains(human.String(), certpolicy.ExpiredRemedy(notAfter)) {
		t.Fatalf("human output = %q, want %q and the remedy", human.String(), want)
	}
	noHeldCertificateStep(t, "certificate status", human.String())

	var js bytes.Buffer
	if err := writeStatus(&js, true, timing, nil, now); err != nil {
		t.Fatalf("JSON status: exit error %v, want 0", err)
	}
	var m map[string]any
	if err := json.Unmarshal(js.Bytes(), &m); err != nil {
		t.Fatal(err)
	}
	if m["status"] != "ok" || m["notAfter"] != notAfter.Format(time.RFC3339) || m["daysRemaining"] != float64(0) ||
		m["renewRecommended"] != false || m["expired"] != true || m["result"] != "expired" ||
		m["nextStep"] != certpolicy.ExpiredRemedy(notAfter) {
		t.Fatalf("JSON = %v", m)
	}

	// A valid certificate keeps its output and gains expired false.
	valid := certpolicy.Evaluate(now.Add(-time.Hour), now.Add(47*time.Hour), "node-under-test", now)
	js.Reset()
	if err := writeStatus(&js, true, valid, nil, now); err != nil {
		t.Fatal(err)
	}
	m = nil
	_ = json.Unmarshal(js.Bytes(), &m)
	if m["expired"] != false || m["result"] != "valid" || m["nextStep"] != nil {
		t.Fatalf("valid JSON = %v", m)
	}
}

// TestExpiredRemedyNamesNoHeldCertificateStep (criterion 7): the remedy text,
// and the expired output of `runos certificate status` and `runos
// certificate renew`, name no step that needs the held certificate. `runos
// status` is checked by TestStatusExpiredFifteenMinutesAgo in cmd/status with
// the same rule. The renew command stops before any request.
func TestExpiredRemedyNamesNoHeldCertificateStep(t *testing.T) {
	notAfter := time.Now().UTC().Truncate(time.Second).Add(-15 * time.Minute)
	noHeldCertificateStep(t, "the remedy text", certpolicy.ExpiredRemedy(notAfter))
	useExpiredPair(t, notAfter)
	dials := backend.DialAttempts()
	cause := (&backend.CertificateExpiredError{NotAfter: notAfter}).Error()

	logs := captureLog(t)
	var err error
	out := captureStdout(t, func() { err = certificate.RenewCertificate(false) })
	if err == nil || !strings.Contains(err.Error(), cause) {
		t.Fatalf("renew: err = %v, want the cause %q", err, cause)
	}
	var fail map[string]any
	for _, l := range strings.Split(strings.TrimSpace(logs.String()), "\n") {
		var e map[string]any
		if json.Unmarshal([]byte(l), &e) == nil && e["step"] != nil {
			fail = e
		}
	}
	if fail == nil || fail["cause"] != cause || fail["remedy"] != certpolicy.ExpiredRemedy(notAfter) {
		t.Fatalf("renew failure = %v, want the cause and the remedy text", fail)
	}
	noHeldCertificateStep(t, "certificate renew", out+"\n"+fail["cause"].(string)+"\n"+fail["remedy"].(string))

	logs.Reset()
	out = captureStdout(t, func() { err = certificate.RenewCertificate(true) })
	if err == nil {
		t.Fatal("renew --json: want a non-zero exit")
	}
	var m map[string]any
	if jerr := json.Unmarshal([]byte(out), &m); jerr != nil {
		t.Fatalf("renew --json output is not one JSON object: %q", out)
	}
	if len(m) != 2 || m["status"] != "error" || m["error"] != cause+". "+certpolicy.ExpiredRemedy(notAfter) {
		t.Fatalf("renew --json = %v", m)
	}
	noHeldCertificateStep(t, "certificate renew --json", out)
	if backend.DialAttempts() != dials {
		t.Fatal("certificate renew dialed with an expired certificate")
	}
	if strings.Contains(logs.String(), "Requesting") {
		t.Fatal("certificate renew sent a request")
	}
}
