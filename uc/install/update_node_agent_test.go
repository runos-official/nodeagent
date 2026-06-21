package install

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestExpectedChecksum(t *testing.T) {
	const name = "nodeagent-linux-amd64"
	const want = "abc123def456"

	cases := []struct {
		name      string
		checksums string
		asset     string
		want      string
		wantErr   bool
	}{
		{
			name:      "plain sha256sum line",
			checksums: want + "  " + name + "\n",
			asset:     name,
			want:      want,
		},
		{
			name:      "binary-mode asterisk marker",
			checksums: want + " *" + name + "\n",
			asset:     name,
			want:      want,
		},
		{
			name: "picks the matching asset among many",
			checksums: "0000  nodeagent-linux-arm64\n" +
				want + "  " + name + "\n" +
				"1111  checksums.txt\n",
			asset: name,
			want:  want,
		},
		{
			name:      "uppercase digest is lowercased",
			checksums: "ABC123  " + name + "\n",
			asset:     name,
			want:      "abc123",
		},
		{
			name:      "missing asset fails closed",
			checksums: "0000  some-other-file\n",
			asset:     name,
			wantErr:   true,
		},
		{
			name:      "empty checksums fails closed",
			checksums: "",
			asset:     name,
			wantErr:   true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := expectedChecksum(tc.checksums, tc.asset)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expectedChecksum(%q, %q) = %q, want error", tc.checksums, tc.asset, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("expectedChecksum(%q, %q) unexpected error: %v", tc.checksums, tc.asset, err)
			}
			if got != tc.want {
				t.Fatalf("expectedChecksum(%q, %q) = %q, want %q", tc.checksums, tc.asset, got, tc.want)
			}
		})
	}
}

// Pins the fail-closed version gate: the updater installs only an EXACT,
// attested semver tag. Floating/garbage values ("latest", "banana") and partial
// versions ("1.2") must be rejected before any release URL is built — accepting
// them would let `runos update` fetch an unattested or non-existent binary.
func TestSemverRe(t *testing.T) {
	valid := []string{
		"v0.24.0", "0.24.0", "v1.2.3", "1.2.3-rc.1", "v2.0.0-beta.2", "10.20.30",
	}
	for _, v := range valid {
		if !semverRe.MatchString(v) {
			t.Errorf("semverRe rejected valid exact tag %q", v)
		}
	}

	invalid := []string{
		"latest",  // floating alias, the prior root-RCE-ish fallback
		"banana",  // garbage
		"1.2",     // partial (missing patch)
		"1",       // partial
		"v1.2",    // partial with v
		"",        // empty
		"v",       // just the prefix
		"1.2.3.4", // too many segments
		"main",    // branch name
		"vlatest", // v-prefixed alias
		" 1.2.3",  // leading space (anchored regex must reject; caller trims, but pin anchoring)
	}
	for _, v := range invalid {
		if semverRe.MatchString(v) {
			t.Errorf("semverRe accepted invalid version %q (must be exact semver only)", v)
		}
	}
}

func TestGoarchToReleaseArch(t *testing.T) {
	cases := map[string]string{
		"amd64": "amd64",
		"arm64": "arm64",
		"386":   "",
		"ppc64": "",
		"":      "",
	}
	for in, want := range cases {
		if got := goarchToReleaseArch(in); got != want {
			t.Fatalf("goarchToReleaseArch(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestAtomicInstall_ReplacesAtomicallyWithMode0755(t *testing.T) {
	dst := filepath.Join(t.TempDir(), "runos")
	if err := os.WriteFile(dst, []byte("old"), 0o600); err != nil {
		t.Fatalf("seed dst: %v", err)
	}

	want := []byte("new-binary-bytes")
	if err := atomicInstall(dst, want); err != nil {
		t.Fatalf("atomicInstall: %v", err)
	}

	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatalf("read dst: %v", err)
	}
	if string(got) != string(want) {
		t.Fatalf("dst content = %q, want %q", got, want)
	}
	info, err := os.Stat(dst)
	if err != nil {
		t.Fatalf("stat dst: %v", err)
	}
	if info.Mode().Perm() != 0o755 {
		t.Fatalf("dst mode = %v, want 0755", info.Mode().Perm())
	}

	// No leftover temp files in the directory.
	entries, err := os.ReadDir(filepath.Dir(dst))
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected only the installed binary, got %d entries", len(entries))
	}
}

// TestInstallVerifiedBinary_VerifiesAndInstalls drives the full download +
// sha256 verify + atomic install path against a fake release server, asserting
// that a matching checksum installs and a mismatch fails closed without
// touching the existing binary.
func TestInstallVerifiedBinary_VerifiesAndInstalls(t *testing.T) {
	arch := goarchToReleaseArch(runtime.GOARCH)
	if arch == "" {
		t.Skipf("unsupported test arch %q", runtime.GOARCH)
	}
	binaryName := "nodeagent-linux-" + arch
	payload := []byte("verified-binary-payload")
	sum := sha256.Sum256(payload)
	goodHex := hex.EncodeToString(sum[:])

	t.Run("matching checksum installs", func(t *testing.T) {
		srv := newReleaseServer(t, binaryName, payload, goodHex)
		defer srv.Close()

		dst := filepath.Join(t.TempDir(), "runos")
		if err := os.WriteFile(dst, []byte("old"), 0o755); err != nil {
			t.Fatalf("seed dst: %v", err)
		}
		withGlobals(t, srv.URL, dst)

		if err := installVerifiedBinary("v1.2.3"); err != nil {
			t.Fatalf("installVerifiedBinary: %v", err)
		}
		got, _ := os.ReadFile(dst)
		if string(got) != string(payload) {
			t.Fatalf("installed content = %q, want %q", got, payload)
		}
	})

	t.Run("checksum mismatch fails closed", func(t *testing.T) {
		srv := newReleaseServer(t, binaryName, payload, "deadbeef")
		defer srv.Close()

		dst := filepath.Join(t.TempDir(), "runos")
		if err := os.WriteFile(dst, []byte("old"), 0o755); err != nil {
			t.Fatalf("seed dst: %v", err)
		}
		withGlobals(t, srv.URL, dst)

		if err := installVerifiedBinary("v1.2.3"); err == nil {
			t.Fatal("expected checksum mismatch error, got nil")
		}
		got, _ := os.ReadFile(dst)
		if string(got) != "old" {
			t.Fatalf("dst was modified on mismatch: %q", got)
		}
	})

	t.Run("missing asset fails closed", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.NotFound(w, r)
		}))
		defer srv.Close()

		dst := filepath.Join(t.TempDir(), "runos")
		if err := os.WriteFile(dst, []byte("old"), 0o755); err != nil {
			t.Fatalf("seed dst: %v", err)
		}
		withGlobals(t, srv.URL, dst)

		if err := installVerifiedBinary("v1.2.3"); err == nil {
			t.Fatal("expected download error for missing asset, got nil")
		}
		got, _ := os.ReadFile(dst)
		if string(got) != "old" {
			t.Fatalf("dst was modified on missing asset: %q", got)
		}
	})
}

// newReleaseServer serves a single tag's binary asset and a checksums.txt that
// lists binaryName with the given hex digest.
func newReleaseServer(t *testing.T, binaryName string, payload []byte, digest string) *httptest.Server {
	t.Helper()
	checksums := fmt.Sprintf("%s  %s\n", digest, binaryName)
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/v1.2.3/"+binaryName:
			w.Write(payload)
		case r.URL.Path == "/v1.2.3/checksums.txt":
			w.Write([]byte(checksums))
		default:
			http.NotFound(w, r)
		}
	}))
}

// withGlobals points the package-level release base + binary path at the test
// fixtures and restores them when the test ends.
func withGlobals(t *testing.T, base, dst string) {
	t.Helper()
	origBase, origPath := releaseBaseURL, binaryPath
	releaseBaseURL = base
	binaryPath = dst
	t.Cleanup(func() {
		releaseBaseURL = origBase
		binaryPath = origPath
	})
}
