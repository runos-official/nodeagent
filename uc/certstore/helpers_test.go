package certstore

import (
	"bytes"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/runos-official/nodeagent/roslog"
	"github.com/runos-official/nodeagent/uc/csrcert/csrtest"
)

// errCrash is what the crash hook returns to stop a function at a point.
var errCrash = errors.New("test crash")

// v2Leaf is the shape of a Nodeward v2 node leaf.
var v2Leaf = csrtest.LeafOptions{DNSNames: []string{"acct-a.example.test"}, Version: 2, Type: 1}

// fixture is one credential directory with a parent on disk and a child
// ready to adopt.
type fixture struct {
	dir    string
	paths  Paths
	ca     *csrtest.CA
	parent Material
	child  Material
}

func newECKey(t testing.TB) (crypto.Signer, []byte) {
	t.Helper()
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(k)
	if err != nil {
		t.Fatal(err)
	}
	return k, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
}

func newRSAKey(t testing.TB) (crypto.Signer, []byte) {
	t.Helper()
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	return k, pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(k)})
}

// material makes a key and a leaf of the given shape, signed by ca.
func material(t testing.TB, ca *csrtest.CA, opts csrtest.LeafOptions) Material {
	t.Helper()
	k, keyPEM := newECKey(t)
	return Material{KeyPEM: keyPEM, CertPEM: []byte(ca.LeafWith(t, k.Public(), opts)), CAPEM: []byte(ca.PEM)}
}

// newFixture writes a v2 parent with a legacy 0644 key mode when keyMode
// says so, and makes a valid v2 child.
func newFixture(t testing.TB, ca *csrtest.CA, parentOpts csrtest.LeafOptions, keyMode fs.FileMode) *fixture {
	t.Helper()
	dir := t.TempDir()
	f := &fixture{
		dir: dir,
		paths: Paths{
			Cert: filepath.Join(dir, "mtls.crt"),
			Key:  filepath.Join(dir, "mtls.key"),
			CA:   filepath.Join(dir, "ca.crt"),
		},
		ca:     ca,
		parent: material(t, ca, parentOpts),
		child:  material(t, ca, v2Leaf),
	}
	writeLive(t, f.paths, f.parent, keyMode)
	return f
}

func writeLive(t testing.TB, p Paths, m Material, keyMode fs.FileMode) {
	t.Helper()
	for _, w := range []struct {
		path string
		data []byte
		mode fs.FileMode
	}{{p.CA, m.CAPEM, 0o644}, {p.Key, m.KeyPEM, keyMode}, {p.Cert, m.CertPEM, 0o644}} {
		if err := os.WriteFile(w.path, w.data, w.mode); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(w.path, w.mode); err != nil {
			t.Fatal(err)
		}
	}
}

func sum(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

// liveIs reports whether all three live files equal m byte for byte.
func liveIs(t testing.TB, p Paths, m Material) bool {
	t.Helper()
	for _, c := range []struct {
		path string
		want []byte
	}{{p.CA, m.CAPEM}, {p.Key, m.KeyPEM}, {p.Cert, m.CertPEM}} {
		got, err := os.ReadFile(c.path)
		if err != nil || !bytes.Equal(got, c.want) {
			return false
		}
	}
	return true
}

// assertLive checks that the live key matches the live certificate and
// that all three live files are want.
func assertLive(t testing.TB, p Paths, want Material, label string) {
	t.Helper()
	if _, err := tls.LoadX509KeyPair(p.Cert, p.Key); err != nil {
		t.Fatalf("%s: live key does not match live certificate: %v", label, err)
	}
	if !liveIs(t, p, want) {
		t.Fatalf("%s: live files are not the expected material", label)
	}
}

// strayFiles lists every staged, previous or temporary file in dir.
func strayFiles(t testing.TB, dir string) []string {
	t.Helper()
	var out []string
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range ents {
		n := e.Name()
		if strings.HasSuffix(n, ".next") || strings.HasSuffix(n, ".prev") || strings.HasSuffix(n, ".tmp") {
			out = append(out, n)
		}
	}
	return out
}

// snapshot records name, mode and digest of every file in dir.
func snapshot(t testing.TB, dir string) string {
	t.Helper()
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var lines []string
	for _, e := range ents {
		info, err := e.Info()
		if err != nil {
			t.Fatal(err)
		}
		b, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		lines = append(lines, e.Name()+" "+info.Mode().String()+" "+sum(b))
	}
	sort.Strings(lines)
	return strings.Join(lines, "\n")
}

func modeOf(t testing.TB, path string) fs.FileMode {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return info.Mode().Perm()
}

// crashAt returns a store whose hook stops at point and records every
// point that it passes.
func crashAt(p Paths, point string, reached map[string]bool) *Store {
	s := Open(p)
	s.hook = func(at string) error {
		if reached != nil {
			reached[at] = true
		}
		if at == point {
			return errCrash
		}
		return nil
	}
	return s
}

// captureLog sends roslog output to a buffer at debug level.
func captureLog(t testing.TB) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	old := roslog.L
	roslog.L = slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	t.Cleanup(func() { roslog.L = old })
	return &buf
}

func journalPath(p Paths) string { return filepath.Join(filepath.Dir(p.Cert), JournalName) }

func pointsWith(prefix bool) []string {
	var out []string
	for _, p := range CrashPoints {
		if strings.HasPrefix(p, "recover-") == prefix {
			out = append(out, p)
		}
	}
	return out
}
