package agentstream

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Goal 23 F5. `kubeadm init phase upload-certs --upload-certs` generates a FRESH key on every
// call and re-encrypts the kubeadm-certs Secret with it, so a second control-plane join starting
// while the first was still downloading invalidated the key the first had been handed:
//
//	error execution phase control-plane-prepare/download-certs: error downloading certs:
//	  error decoding secret data with provided key: cipher: message authentication failed
//
// Reproduced twice on live hardware, forty seconds apart on the second occasion. Passing an
// explicit --certificate-key makes the upload idempotent, which removes the race instead of
// narrowing its window. These cover the key's own contract: kubeadm accepts nothing but 32 bytes
// of hex, and a key that has aged out must not be handed to a joiner whose Secret has expired.

func TestNewCertKeyIsThirtyTwoBytesOfHex(t *testing.T) {
	key, err := newCertKey()
	if err != nil {
		t.Fatalf("generating a key failed: %v", err)
	}
	if len(key) != 64 {
		t.Fatalf("kubeadm needs 32 bytes as hex (64 characters), got %d: %q", len(key), key)
	}
	if strings.Trim(key, "0123456789abcdef") != "" {
		t.Fatalf("key must be lowercase hex, got %q", key)
	}
	second, _ := newCertKey()
	if key == second {
		t.Fatal("two generated keys must differ")
	}
}

func TestReadCachedCertKeyRejectsAnythingKubeadmWouldRefuse(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cert-upload-key.txt")
	restore := swapCertKeyPath(t, path)
	defer restore()

	// Absent.
	if got := readCachedCertKey(); got != "" {
		t.Fatalf("no file should read as no key, got %q", got)
	}

	// Truncated: handing this on fails the join with a confusing error instead of an obvious one.
	if err := os.WriteFile(path, []byte("deadbeef"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := readCachedCertKey(); got != "" {
		t.Fatalf("a short key must read as absent, got %q", got)
	}

	valid := strings.Repeat("ab", 32)
	if err := os.WriteFile(path, []byte(valid+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := readCachedCertKey(); got != valid {
		t.Fatalf("a valid key should be reused verbatim, got %q", got)
	}
}

func TestReadCachedCertKeyExpiresWithTheUploadedSecret(t *testing.T) {
	// kubeadm expires the uploaded Secret after two hours. A cached key older than the reuse
	// window must not be handed out, or the joiner decrypts a Secret that is no longer there.
	dir := t.TempDir()
	path := filepath.Join(dir, "cert-upload-key.txt")
	restore := swapCertKeyPath(t, path)
	defer restore()

	valid := strings.Repeat("cd", 32)
	if err := os.WriteFile(path, []byte(valid), 0o600); err != nil {
		t.Fatal(err)
	}
	stale := time.Now().Add(-certKeyReuseWindow - time.Minute)
	if err := os.Chtimes(path, stale, stale); err != nil {
		t.Fatal(err)
	}
	if got := readCachedCertKey(); got != "" {
		t.Fatalf("a key past the reuse window must read as absent, got %q", got)
	}
}

// swapCertKeyPath points the cache at a temporary file and returns the restore.
func swapCertKeyPath(t *testing.T, path string) func() {
	t.Helper()
	previous := certKeyPath
	certKeyPath = path
	return func() { certKeyPath = previous }
}
