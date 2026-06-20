package preflight

import (
	"errors"
	"net"
	"runtime"
	"strings"
	"testing"

	"github.com/runos-official/nodeagent/commons"
)

func TestVerifyOSSupported(t *testing.T) {
	cases := []struct {
		name    string
		rel     commons.OSRelease
		wantErr bool
		// substring expected in the error message, when wantErr is true.
		errContains string
	}{
		{"ubuntu 22.04", commons.OSRelease{ID: "ubuntu", VersionID: "22.04"}, false, ""},
		{"ubuntu 24.04", commons.OSRelease{ID: "ubuntu", VersionID: "24.04"}, false, ""},
		{"ubuntu 26.04", commons.OSRelease{ID: "ubuntu", VersionID: "26.04"}, false, ""},
		{"ubuntu derivative via ID_LIKE 24.04", commons.OSRelease{ID: "pop", IDLike: "ubuntu debian", VersionID: "24.04"}, false, ""},
		{"ubuntu interim 25.04 rejected", commons.OSRelease{ID: "ubuntu", VersionID: "25.04"}, true, "unsupported Ubuntu version"},
		{"ubuntu old 20.04 rejected", commons.OSRelease{ID: "ubuntu", VersionID: "20.04"}, true, "unsupported Ubuntu version"},
		{"non-ubuntu rhel", commons.OSRelease{ID: "rhel", Name: "Red Hat Enterprise Linux", VersionID: "9.3"}, true, "unsupported OS"},
		{"empty (unreadable)", commons.OSRelease{}, true, "unsupported OS"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Ensure the override env is off for these cases.
			t.Setenv("RUNOS_ALLOW_UNTESTED_OS", "")
			err := verifyOSSupported(tc.rel)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error, got nil")
				}
				if tc.errContains != "" && !strings.Contains(err.Error(), tc.errContains) {
					t.Fatalf("error %q does not contain %q", err.Error(), tc.errContains)
				}
				// Every failure message must name the supported set.
				if !strings.Contains(err.Error(), supportedUbuntuList) {
					t.Fatalf("error %q does not list supported versions %q", err.Error(), supportedUbuntuList)
				}
			} else if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

func TestVerifyOSSupported_OverrideAllowsUntested(t *testing.T) {
	t.Setenv("RUNOS_ALLOW_UNTESTED_OS", "1")
	// An interim release that is normally rejected.
	if err := verifyOSSupported(commons.OSRelease{ID: "ubuntu", VersionID: "26.10"}); err != nil {
		t.Fatalf("override should allow untested OS, got: %v", err)
	}
	// Even a non-ubuntu OS is forced through with the override.
	if err := verifyOSSupported(commons.OSRelease{ID: "debian", VersionID: "12"}); err != nil {
		t.Fatalf("override should allow non-ubuntu OS, got: %v", err)
	}
}

func TestClassifyDialError(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want string // substring
	}{
		{"dns", &net.DNSError{Err: "no such host", Name: "nodeward.example"}, "DNS resolution failed"},
		{"refused", errors.New("dial tcp 1.2.3.4:9191: connect: connection refused"), "connection refused"},
		{"timeout", errors.New("dial tcp 1.2.3.4:9191: i/o timeout"), "timed out"},
		{"other", errors.New("dial tcp: some weird error"), "some weird error"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := classifyDialError(tc.err, "nodeward.example")
			if !strings.Contains(got, tc.want) {
				t.Fatalf("classifyDialError = %q, want substring %q", got, tc.want)
			}
		})
	}
}

func TestCheckArch(t *testing.T) {
	// checkArch reflects the build's GOARCH. The test binary builds for amd64 or
	// arm64 in CI/dev, so it must pass; on any other arch it must fail clearly.
	err := checkArch()
	switch runtime.GOARCH {
	case "amd64", "arm64":
		if err != nil {
			t.Fatalf("checkArch() on %s = %v, want nil", runtime.GOARCH, err)
		}
	default:
		if err == nil {
			t.Fatalf("checkArch() on %s = nil, want error", runtime.GOARCH)
		}
		if !strings.Contains(err.Error(), runtime.GOARCH) {
			t.Fatalf("checkArch() error %q should name arch %q", err.Error(), runtime.GOARCH)
		}
	}
}
