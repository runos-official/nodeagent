package commons

import (
	"strings"
	"testing"
)

func TestClassifyCommandFailureSignatures(t *testing.T) {
	cases := []struct {
		name       string
		output     string
		wantCode   string // exact code expected
		wantCause  string // substring expected in cause
		wantRemedy string // substring expected in remedy
	}{
		{
			name:       "apt dpkg lock",
			output:     "E: Could not get lock /var/lib/dpkg/lock-frontend. It is held by process 1234",
			wantCode:   "NA_APT_LOCK",
			wantCause:  "package lock",
			wantRemedy: "lsof",
		},
		{
			name:       "no space left",
			output:     "tar: write error: No space left on device",
			wantCode:   "NA_DISK_FULL",
			wantCause:  "disk space",
			wantRemedy: "df -h",
		},
		{
			name:       "dns failure",
			output:     "Temporary failure resolving 'archive.ubuntu.com'",
			wantCode:   "NA_NET_UNREACH",
			wantCause:  "network",
			wantRemedy: "DNS",
		},
		{
			name:       "package not found",
			output:     "E: Unable to locate package kubeadm",
			wantCode:   "NA_PKG_NOTFOUND",
			wantCause:  "not available",
			wantRemedy: "apt-get update",
		},
		{
			name:       "broken packages",
			output:     "The following packages have unmet dependencies:\n containerd : Depends: runc",
			wantCode:   "NA_HELD_PKGS",
			wantCause:  "broken, or conflicting",
			wantRemedy: "-f install",
		},
		{
			name:       "permission denied",
			output:     "mkdir: cannot create directory '/etc/runos': Permission denied",
			wantCode:   "NA_PERMISSION",
			wantCause:  "privileges",
			wantRemedy: "root",
		},
		{
			name:       "kubeadm preflight",
			output:     "[preflight] Some fatal errors occurred:\n\t[ERROR Swap]: running with swap on is not supported",
			wantCode:   "NA_KUBEADM",
			wantCause:  "kubeadm",
			wantRemedy: "swap",
		},
		{
			name:       "containerd pull",
			output:     "failed to pull image \"registry.k8s.io/pause:3.9\": rpc error",
			wantCode:   "NA_CONTAINERD",
			wantCause:  "container runtime",
			wantRemedy: "containerd",
		},
		{
			name:       "gpg no pubkey",
			output:     "W: GPG error: ... NO_PUBKEY 1234ABCD",
			wantCode:   "NA_REPO_GPG",
			wantCause:  "signing key is missing",
			wantRemedy: "GPG key",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			code, cause, remedy := classifyCommandFailure("apt-get install -y kubeadm", "apt-get install -y kubeadm", tc.output)
			if code != tc.wantCode {
				t.Errorf("code = %q, want %q", code, tc.wantCode)
			}
			if cause == "" {
				t.Fatal("cause must never be empty")
			}
			if !strings.Contains(cause, tc.wantCause) {
				t.Errorf("cause = %q, want substring %q", cause, tc.wantCause)
			}
			if !strings.Contains(remedy, tc.wantRemedy) {
				t.Errorf("remedy = %q, want substring %q", remedy, tc.wantRemedy)
			}
		})
	}
}

func TestClassifyCommandFailureCaseInsensitive(t *testing.T) {
	// Upper-cased signature still matches (matching is case-insensitive).
	code, cause, _ := classifyCommandFailure("step", "cmd", "NO SPACE LEFT ON DEVICE")
	if code != "NA_DISK_FULL" {
		t.Errorf("case-insensitive code = %q, want NA_DISK_FULL", code)
	}
	if !strings.Contains(cause, "disk space") {
		t.Errorf("case-insensitive match failed, cause = %q", cause)
	}
}

func TestClassifyCommandFailureGenericFallback(t *testing.T) {
	// Unrecognized output falls back to the step-named generic message.
	code, cause, remedy := classifyCommandFailure("apt-get install foo", "apt-get install foo", "some totally unrecognized error blob")
	if code != "NA_GENERIC" {
		t.Errorf("generic fallback code = %q, want NA_GENERIC", code)
	}
	if cause == "" {
		t.Fatal("generic fallback cause must never be empty")
	}
	if !strings.Contains(cause, "apt-get install foo") {
		t.Errorf("generic cause should name the step, got %q", cause)
	}
	if !strings.Contains(remedy, "/var/log/runos.log") {
		t.Errorf("generic remedy should point at the full log, got %q", remedy)
	}
}

func TestClassifyCommandFailureEmptyOutput(t *testing.T) {
	// Empty output: must not panic, must fall back to a non-empty generic cause.
	code, cause, remedy := classifyCommandFailure("install kubernetes", "kubeadm init", "")
	if code != "NA_GENERIC" {
		t.Errorf("empty-output code = %q, want NA_GENERIC", code)
	}
	if cause == "" {
		t.Fatal("empty-output cause must never be empty")
	}
	if !strings.Contains(cause, "install kubernetes") {
		t.Errorf("empty-output cause should name the step, got %q", cause)
	}
	if remedy == "" {
		t.Error("empty-output remedy should not be empty")
	}
}

func TestGenericCauseEmptyStep(t *testing.T) {
	if got := genericCause(""); got == "" {
		t.Error("genericCause(\"\") must not be empty")
	}
	if got := genericCause("   "); got == "" {
		t.Error("genericCause(whitespace) must not be empty")
	}
}
