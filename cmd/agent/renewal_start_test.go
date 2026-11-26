package agent

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/runos-official/nodeagent/uc/certpolicy"
	"github.com/runos-official/nodeagent/uc/certstore"
	"github.com/runos-official/nodeagent/uc/csrcert/csrtest"
)

// timedMaterial makes a key and a v2 leaf valid from notBefore to notAfter.
func timedMaterial(t *testing.T, notBefore, notAfter time.Time) certstore.Material {
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
		NotBefore: notBefore, NotAfter: notAfter}
	return certstore.Material{
		KeyPEM:  pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}),
		CertPEM: []byte(ca.LeafWith(t, k.Public(), opts)),
		CAPEM:   []byte(ca.PEM),
	}
}

// logMessages decodes the JSON log lines.
func logMessages(t *testing.T, logs *bytes.Buffer) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, l := range strings.Split(strings.TrimSpace(logs.String()), "\n") {
		if l == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(l), &m); err != nil {
			t.Fatalf("log line is not JSON: %s", l)
		}
		out = append(out, m)
	}
	return out
}

// TestStartDoesNotRenewBeforeConnection: a certificate that is due at start
// gets no renewal request and no dial from the start path. The renewal loop
// asks only after runConnection signals a stream.
func TestStartDoesNotRenewBeforeConnection(t *testing.T) {
	dir := t.TempDir()
	p := useCredDir(t, dir)
	now := time.Now()
	m := timedMaterial(t, now.Add(-23*time.Hour), now.Add(time.Hour))
	writeMaterial(t, p, m)
	if timing := certpolicy.Evaluate(now.Add(-23*time.Hour), now.Add(time.Hour), "", now); !timing.Due || timing.Expired {
		t.Fatal("the test leaf is not due")
	}
	logs := captureStartLog(t)

	ran := false
	if err := start(filepath.Join(t.TempDir(), "agent.lock"), func() { ran = true }); err != nil {
		t.Fatalf("start: %v", err)
	}
	if !ran {
		t.Fatal("start did not reach the agent")
	}
	for _, l := range logMessages(t, logs) {
		msg, _ := l["msg"].(string)
		if strings.Contains(msg, "Connecting to nodeward") || strings.Contains(msg, "Requesting a renewed certificate") ||
			strings.Contains(strings.ToLower(msg), "renewal") {
			t.Fatalf("the start path renewed or dialed: %v", l)
		}
	}
	if got, _ := os.ReadFile(p.Cert); !bytes.Equal(got, m.CertPEM) {
		t.Fatal("the start path changed the certificate")
	}
}

// TestCheckAndAutoRenewGone: the start-time renewal functions are deleted
// from the tree. This file names them in the test name and is skipped.
func TestCheckAndAutoRenewGone(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Fatalf("no module root at %s", root)
	}
	names := []string{"CheckAnd" + "AutoRenew", "perform" + "AutoRenewal"}
	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && (d.Name() == ".git" || d.Name() == "vendor") {
			return filepath.SkipDir
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") || d.Name() == "renewal_start_test.go" {
			return nil
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for _, n := range names {
			if bytes.Contains(b, []byte(n)) {
				t.Errorf("%s names %s", path, n)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
