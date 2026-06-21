package commons

import (
	"strings"
	"testing"
)

func TestClassifyCommandFailureSignatures(t *testing.T) {
	cases := []struct {
		name       string
		output     string
		wantCause  string // substring expected in cause
		wantRemedy string // substring expected in remedy
	}{
		{
			name:       "apt dpkg lock",
			output:     "E: Could not get lock /var/lib/dpkg/lock-frontend. It is held by process 1234",
			wantCause:  "package lock",
			wantRemedy: "lsof",
		},
		{
			name:       "no space left",
			output:     "tar: write error: No space left on device",
			wantCause:  "disk space",
			wantRemedy: "df -h",
		},
		{
			name:       "dns failure",
			output:     "Temporary failure resolving 'archive.ubuntu.com'",
			wantCause:  "network",
			wantRemedy: "DNS",
		},
		{
			name:       "package not found",
			output:     "E: Unable to locate package kubeadm",
			wantCause:  "not available",
			wantRemedy: "apt-get update",
		},
		{
			name:       "broken packages",
			output:     "The following packages have unmet dependencies:\n containerd : Depends: runc",
			wantCause:  "broken, or conflicting",
			wantRemedy: "-f install",
		},
		{
			name:       "permission denied",
			output:     "mkdir: cannot create directory '/etc/runos': Permission denied",
			wantCause:  "privileges",
			wantRemedy: "root",
		},
		{
			name:       "kubeadm preflight",
			output:     "[preflight] Some fatal errors occurred:\n\t[ERROR Swap]: running with swap on is not supported",
			wantCause:  "kubeadm",
			wantRemedy: "swap",
		},
		{
			name:       "containerd pull",
			output:     "failed to pull image \"registry.k8s.io/pause:3.9\": rpc error",
			wantCause:  "container runtime",
			wantRemedy: "containerd",
		},
		{
			name:       "gpg no pubkey",
			output:     "W: GPG error: ... NO_PUBKEY 1234ABCD",
			wantCause:  "signing key is missing",
			wantRemedy: "GPG key",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cause, remedy := classifyCommandFailure("apt-get install -y kubeadm", "apt-get install -y kubeadm", tc.output)
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
	cause, _ := classifyCommandFailure("step", "cmd", "NO SPACE LEFT ON DEVICE")
	if !strings.Contains(cause, "disk space") {
		t.Errorf("case-insensitive match failed, cause = %q", cause)
	}
}

func TestClassifyCommandFailureGenericFallback(t *testing.T) {
	// Unrecognized output falls back to the step-named generic message.
	cause, remedy := classifyCommandFailure("apt-get install foo", "apt-get install foo", "some totally unrecognized error blob")
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
	cause, remedy := classifyCommandFailure("install kubernetes", "kubeadm init", "")
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
