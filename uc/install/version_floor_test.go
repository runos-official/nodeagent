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
	"sync/atomic"
	"testing"

	"github.com/runos-official/nodeagent/version"
)

// FCR 1650: an explicit --version below the running version must not install.
// Before the floor, the updater installed any published tag after a checksum
// check, so a node on a fixed release could be moved back to a vulnerable one.
func TestUpdateRefusesExplicitDowngrade(t *testing.T) {
	arch := goarchToReleaseArch(runtime.GOARCH)
	if arch == "" {
		t.Skipf("unsupported test arch %q", runtime.GOARCH)
	}
	binaryName := "nodeagent-linux-" + arch
	payload := []byte("older-release-binary")
	sum := sha256.Sum256(payload)
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		switch r.URL.Path {
		case "/v1.8.4/" + binaryName:
			_, _ = w.Write(payload)
		case "/v1.8.4/checksums.txt":
			_, _ = fmt.Fprintf(w, "%s  %s\n", hex.EncodeToString(sum[:]), binaryName)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	dst := filepath.Join(t.TempDir(), "runos")
	if err := os.WriteFile(dst, []byte("running"), 0o644); err != nil {
		t.Fatalf("seed dst: %v", err)
	}
	withGlobals(t, srv.URL, dst)

	origVersion := version.Version
	version.Version = "1.8.5"
	t.Cleanup(func() { version.Version = origVersion })
	logs := stubUpdateSideEffects(t)

	err := UpdateNodeAgent("v1.8.4", false, true)
	if err == nil {
		t.Fatal("an explicit downgrade from 1.8.5 to v1.8.4 was accepted")
	}
	got, _ := os.ReadFile(dst)
	if string(got) != "running" {
		t.Fatalf("the binary was replaced by a downgrade: %q", got)
	}
	if n := hits.Load(); n != 0 {
		t.Fatalf("the updater fetched %d release files before refusing", n)
	}
	if len(*logs) != 1 || (*logs)[0] != "AgentDowngradeRefused" {
		t.Fatalf("node logs = %v, want one AgentDowngradeRefused", *logs)
	}
}

// stubUpdateSideEffects stubs the service restart and the control-plane node
// log, and records the node log types that the updater sends.
func stubUpdateSideEffects(t *testing.T) *[]string {
	t.Helper()
	var logs []string
	origRestart, origLog := restartService, addNodelog
	restartService = func() error { return nil }
	addNodelog = func(_ int, logType, _ string) error {
		logs = append(logs, logType)
		return nil
	}
	t.Cleanup(func() { restartService, addNodelog = origRestart, origLog })
	return &logs
}

// releaseFixture serves one release tag and counts every request.
func releaseFixture(t *testing.T, tag string, payload []byte) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	arch := goarchToReleaseArch(runtime.GOARCH)
	if arch == "" {
		t.Skipf("unsupported test arch %q", runtime.GOARCH)
	}
	binaryName := "nodeagent-linux-" + arch
	sum := sha256.Sum256(payload)
	hits := &atomic.Int32{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		switch r.URL.Path {
		case "/" + tag + "/" + binaryName:
			_, _ = w.Write(payload)
		case "/" + tag + "/checksums.txt":
			_, _ = fmt.Fprintf(w, "%s  %s\n", hex.EncodeToString(sum[:]), binaryName)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, hits
}

// runUpdate seeds the installed binary, points the updater at a fixture for
// tag, sets the running version and runs one update in JSON mode.
func runUpdate(t *testing.T, running, tag, pin string, allowDowngrade bool) (installed string, hits int32, logs []string, err error) {
	t.Helper()
	srv, counter := releaseFixture(t, tag, []byte("binary-"+tag))
	dst := filepath.Join(t.TempDir(), "runos")
	if werr := os.WriteFile(dst, []byte("running"), 0o644); werr != nil {
		t.Fatalf("seed dst: %v", werr)
	}
	withGlobals(t, srv.URL, dst)
	origRunning := runningVersion
	runningVersion = func() string { return running }
	t.Cleanup(func() { runningVersion = origRunning })
	recorded := stubUpdateSideEffects(t)
	if pin == "" {
		ad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = fmt.Fprintf(w, `{"version":%q}`, tag)
		}))
		t.Cleanup(ad.Close)
		withAdvertisedSource(t, ad.URL, "acct1")
	}
	err = UpdateNodeAgent(pin, allowDowngrade, true)
	got, _ := os.ReadFile(dst)
	return string(got), counter.Load(), *recorded, err
}

func TestUpdateVersionFloor(t *testing.T) {
	cases := []struct {
		name          string
		running       string
		tag           string
		pin           bool
		allow         bool
		wantErr       bool
		wantInstalled bool
		wantLog       string
	}{
		{name: "explicit upgrade installs", running: "1.8.4", tag: "v1.8.5-rc.1", pin: true, wantInstalled: true},
		{name: "explicit same version reinstalls", running: "1.8.5-rc.1", tag: "v1.8.5-rc.1", pin: true, wantInstalled: true},
		{name: "explicit downgrade refused", running: "1.8.5-rc.1", tag: "v1.8.4", pin: true, wantErr: true, wantLog: "AgentDowngradeRefused"},
		{name: "rc below its release refused", running: "1.8.5", tag: "v1.8.5-rc.1", pin: true, wantErr: true, wantLog: "AgentDowngradeRefused"},
		{name: "allow-downgrade within the floor installs and logs", running: "1.8.5-rc.1", tag: "v1.8.4", pin: true, allow: true, wantInstalled: true, wantLog: "AgentDowngrade"},
		{name: "allow-downgrade below the minimum refused", running: "1.8.5-rc.1", tag: "v1.8.3", pin: true, allow: true, wantErr: true, wantLog: "AgentDowngradeRefused"},
		{name: "below the minimum refused on an unknown running version", running: "", tag: "v1.8.2-rc.6", pin: true, wantErr: true, wantLog: "AgentDowngradeRefused"},
		{name: "advertised older version keeps the running binary", running: "1.8.5-rc.1", tag: "v1.8.4", wantLog: "AgentDowngradeSkipped"},
		{name: "advertised version below the minimum refused", running: "1.8.5-rc.1", tag: "v1.8.2-rc.6", wantErr: true, wantLog: "AgentDowngradeRefused"},
		{name: "advertised upgrade installs", running: "1.8.4", tag: "v1.8.5-rc.1", wantInstalled: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pin := ""
			if tc.pin {
				pin = tc.tag
			}
			installed, hits, logs, err := runUpdate(t, tc.running, tc.tag, pin, tc.allow)
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tc.wantErr)
			}
			if tc.wantInstalled {
				if installed != "binary-"+tc.tag {
					t.Fatalf("installed = %q, want the %s binary", installed, tc.tag)
				}
			} else {
				if installed != "running" {
					t.Fatalf("the running binary was replaced: %q", installed)
				}
				if hits != 0 {
					t.Fatalf("the updater fetched %d release files before deciding not to install", hits)
				}
			}
			hasLog := false
			for _, l := range logs {
				if l == tc.wantLog {
					hasLog = true
				}
			}
			if tc.wantLog != "" && !hasLog {
				t.Fatalf("node logs = %v, want %s", logs, tc.wantLog)
			}
		})
	}
}

func TestCompareSemver(t *testing.T) {
	ordered := []string{
		"1.0.0-alpha", "1.0.0-alpha.1", "1.0.0-alpha.beta", "1.0.0-beta", "1.0.0-beta.2",
		"1.0.0-beta.11", "1.0.0-rc.1", "1.0.0", "1.8.2-rc.6", "1.8.3", "1.8.4", "v1.8.5-rc.1",
		"1.8.5-rc.2", "1.8.5-rc.10", "1.8.5", "1.10.0", "2.0.0",
	}
	for i := range ordered {
		for j := range ordered {
			got, err := compareSemver(ordered[i], ordered[j])
			if err != nil {
				t.Fatalf("compareSemver(%q, %q): %v", ordered[i], ordered[j], err)
			}
			want := 0
			if i < j {
				want = -1
			} else if i > j {
				want = 1
			}
			if got != want {
				t.Fatalf("compareSemver(%q, %q) = %d, want %d", ordered[i], ordered[j], got, want)
			}
		}
	}
	if c, err := compareSemver("1.8.5+build.7", "v1.8.5"); err != nil || c != 0 {
		t.Fatalf("build metadata changed precedence: %d, %v", c, err)
	}
	for _, bad := range []string{"", "dev", "1.8", "1.8.x", "1.8.5-", "1.8.5-rc..1"} {
		if _, err := compareSemver(bad, "1.8.5"); err == nil {
			t.Fatalf("compareSemver accepted %q", bad)
		}
	}
}

// The built-in minimum must itself be a valid release version, or every
// update would be refused.
func TestMinimumVersionIsValid(t *testing.T) {
	if _, _, _, err := parseSemver(MinimumVersion); err != nil {
		t.Fatalf("MinimumVersion %q: %v", MinimumVersion, err)
	}
	if !semverRe.MatchString(MinimumVersion) {
		t.Fatalf("MinimumVersion %q fails the release tag pattern", MinimumVersion)
	}
}
