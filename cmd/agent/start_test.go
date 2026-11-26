package agent

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/spf13/viper"

	"github.com/runos-official/nodeagent/roslog"
	"github.com/runos-official/nodeagent/uc/certstore"
	"github.com/runos-official/nodeagent/uc/csrcert/csrtest"
)

var errStop = errors.New("test crash")

// startMaterial makes a key and a v2 leaf signed by ca.
func startMaterial(t *testing.T, ca *csrtest.CA) certstore.Material {
	t.Helper()
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(k)
	if err != nil {
		t.Fatal(err)
	}
	opts := csrtest.LeafOptions{DNSNames: []string{"acct-a.example.test"}, Version: 2, Type: 1}
	return certstore.Material{
		KeyPEM:  pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}),
		CertPEM: []byte(ca.LeafWith(t, k.Public(), opts)),
		CAPEM:   []byte(ca.PEM),
	}
}

// useCredDir points the configured credential paths at dir.
func useCredDir(t *testing.T, dir string) certstore.Paths {
	t.Helper()
	p := certstore.Paths{
		Cert: filepath.Join(dir, "mtls.crt"),
		Key:  filepath.Join(dir, "mtls.key"),
		CA:   filepath.Join(dir, "ca.crt"),
	}
	old := map[string]string{}
	for k, v := range map[string]string{"mtls.crt": p.Cert, "mtls.key": p.Key, "mtls.ca": p.CA} {
		old[k] = viper.GetString(k)
		viper.Set(k, v)
	}
	t.Cleanup(func() {
		for k, v := range old {
			viper.Set(k, v)
		}
	})
	return p
}

func writeMaterial(t *testing.T, p certstore.Paths, m certstore.Material) {
	t.Helper()
	for _, w := range []struct {
		path string
		data []byte
		mode os.FileMode
	}{{p.CA, m.CAPEM, 0o644}, {p.Key, m.KeyPEM, 0o600}, {p.Cert, m.CertPEM, 0o644}} {
		if err := os.WriteFile(w.path, w.data, w.mode); err != nil {
			t.Fatal(err)
		}
	}
}

func captureStartLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	old := roslog.L
	roslog.L = slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	t.Cleanup(func() { roslog.L = old })
	return &buf
}

func listDir(t *testing.T, dir string) []string {
	t.Helper()
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range ents {
		out = append(out, e.Name())
	}
	sort.Strings(out)
	return out
}

// TestStartRecoversBeforeRegistrationCheck drives the start path of the
// agent command up to the first dial (the run function is faked).
func TestStartRecoversBeforeRegistrationCheck(t *testing.T) {
	t.Run("crash after the key rename rolls forward", func(t *testing.T) {
		dir := t.TempDir()
		p := useCredDir(t, dir)
		ca := csrtest.NewCA(t)
		parent, child := startMaterial(t, ca), startMaterial(t, ca)
		writeMaterial(t, p, parent)

		// Leave the directory at the crash point after the key rename:
		// child key, parent certificate, journal committing.
		s := certstore.Open(p)
		s.SetCrashHook(func(at string) error {
			if at == "renamed-key" {
				return errStop
			}
			return nil
		})
		if err := s.Prepare(); err != nil {
			t.Fatal(err)
		}
		if err := s.Adopt(child); !errors.Is(err, errStop) {
			t.Fatalf("adopt: want the crash, got %v", err)
		}
		if rec, _, _ := certstore.Open(p).Adopted(); rec.State != certstore.StateCommitting {
			t.Fatalf("journal state = %q, want committing", rec.State)
		}

		ran := false
		err := start(filepath.Join(t.TempDir(), "agent.lock"), func() {
			ran = true
			pair, err := certstore.LoadKeyPair(p.Cert, p.Key)
			if err != nil {
				t.Fatalf("pair at the first dial does not load: %v", err)
			}
			if got := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: pair.Certificate[0]}); !bytes.Equal(got, child.CertPEM) {
				t.Fatal("the first dial does not use the child certificate")
			}
		})
		if err != nil {
			t.Fatalf("start: %v", err)
		}
		if !ran {
			t.Fatal("start did not reach the first dial")
		}
		for _, c := range []struct {
			path string
			want []byte
		}{{p.CA, child.CAPEM}, {p.Key, child.KeyPEM}, {p.Cert, child.CertPEM}} {
			if got, _ := os.ReadFile(c.path); !bytes.Equal(got, c.want) {
				t.Fatalf("%s is not the child after the start", c.path)
			}
		}
	})

	t.Run("unregistered directory", func(t *testing.T) {
		dir := t.TempDir()
		useCredDir(t, dir)
		logs := captureStartLog(t)
		ran := false
		err := start(filepath.Join(t.TempDir(), "agent.lock"), func() { ran = true })
		if err == nil || ran {
			t.Fatalf("want the not-registered refusal, got err=%v ran=%v", err, ran)
		}
		if !roslog.IsAlreadyReported(err) || !strings.Contains(logs.String(), "this node is not registered") {
			t.Fatalf("want the not-registered refusal, got %v / %s", err, logs.String())
		}
		if got := listDir(t, dir); len(got) != 0 {
			t.Fatalf("start created files in an unregistered directory: %v", got)
		}
		assertNoRecoveryLog(t, logs.String())
	})

	t.Run("missing certificate directory", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "absent")
		useCredDir(t, dir)
		logs := captureStartLog(t)
		err := start(filepath.Join(t.TempDir(), "agent.lock"), func() { t.Fatal("start ran the agent") })
		if err == nil || !strings.Contains(logs.String(), "this node is not registered") {
			t.Fatalf("want the not-registered refusal, got %v", err)
		}
		if _, statErr := os.Stat(dir); !errors.Is(statErr, os.ErrNotExist) {
			t.Fatal("start created the certificate directory")
		}
		assertNoRecoveryLog(t, logs.String())
	})
}

// assertNoRecoveryLog fails when the log holds any line other than the
// not-registered refusal.
func assertNoRecoveryLog(t *testing.T, logs string) {
	t.Helper()
	for _, line := range strings.Split(strings.TrimSpace(logs), "\n") {
		if line == "" || strings.Contains(line, "this node is not registered") {
			continue
		}
		t.Fatalf("unexpected log line on an unregistered node: %s", line)
	}
}
