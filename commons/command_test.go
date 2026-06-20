package commons

import (
	"errors"
	"os/exec"
	"strings"
	"testing"
	"time"
)

func TestLastNLines(t *testing.T) {
	tests := []struct {
		name string
		in   string
		n    int
		want string
	}{
		{"empty", "", 5, ""},
		{"single line", "only\n", 5, "only"},
		{"fewer than n", "a\nb\nc\n", 5, "a\n  b\n  c"},
		{"exactly n", "a\nb\nc\n", 3, "a\n  b\n  c"},
		{"more than n keeps tail", "a\nb\nc\nd\ne\n", 3, "c\n  d\n  e"},
		{"no trailing newline", "a\nb", 5, "a\n  b"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := lastNLines(tc.in, tc.n)
			if got != tc.want {
				t.Fatalf("lastNLines(%q, %d) = %q, want %q", tc.in, tc.n, got, tc.want)
			}
		})
	}
}

func TestDescribeExitError_PassesThroughNonExitError(t *testing.T) {
	want := exec.ErrNotFound
	got := describeExitError(want)
	if got != want {
		t.Fatalf("non-exit error should pass through unchanged; got %v", got)
	}
}

func TestDescribeExitError_ExitCode(t *testing.T) {
	cmd := exec.Command("/bin/sh", "-c", "exit 42")
	err := cmd.Run()
	if err == nil {
		t.Fatal("expected non-nil error from exit 42")
	}
	got := describeExitError(err).Error()
	if got != "exit status 42" {
		t.Fatalf("expected %q, got %q", "exit status 42", got)
	}
}

func TestDescribeExitError_KilledBySignal(t *testing.T) {
	// Self-kill with SIGKILL so Wait() returns a Signaled() WaitStatus.
	cmd := exec.Command("/bin/sh", "-c", "kill -9 $$")
	err := cmd.Run()
	if err == nil {
		t.Fatal("expected non-nil error from self-kill")
	}
	got := describeExitError(err).Error()
	if !strings.HasPrefix(got, "killed by signal 9") {
		t.Fatalf("expected message to start with 'killed by signal 9', got %q", got)
	}
}

func TestExecuteCommandWithActivityTimeoutInstall_CleanSuccess(t *testing.T) {
	out, err := executeCommandWithActivityTimeoutInstall("echo hello", 500*time.Millisecond, 1)
	if err != nil {
		t.Fatalf("expected nil error, got %v", err)
	}
	if !strings.Contains(out, "hello") {
		t.Fatalf("expected output to contain 'hello', got %q", out)
	}
}

func TestExecuteCommandWithActivityTimeoutInstall_ExitCodeSurfaces(t *testing.T) {
	_, err := executeCommandWithActivityTimeoutInstall("exit 7", 500*time.Millisecond, 1)
	if err == nil {
		t.Fatal("expected non-nil error")
	}
	if err.Error() != "exit status 7" {
		t.Fatalf("expected 'exit status 7', got %q", err.Error())
	}
}

func TestExecuteCommandWithActivityTimeoutInstall_HangNoOutput(t *testing.T) {
	_, err := executeCommandWithActivityTimeoutInstall("sleep 5", 200*time.Millisecond, 1)
	if err == nil {
		t.Fatal("expected hang error")
	}
	msg := err.Error()
	if !strings.Contains(msg, "command hung") {
		t.Fatalf("expected 'command hung' in error, got %q", msg)
	}
	if !strings.Contains(msg, "produced no output before silence") {
		t.Fatalf("expected 'produced no output before silence', got %q", msg)
	}
}

func TestExecuteCommandWithActivityTimeoutInstall_HangWithTail(t *testing.T) {
	// Print three lines, then go silent. The tail should be in the error message.
	cmd := "echo a; echo b; echo c; sleep 5"
	_, err := executeCommandWithActivityTimeoutInstall(cmd, 300*time.Millisecond, 1)
	if err == nil {
		t.Fatal("expected hang error")
	}
	msg := err.Error()
	if !strings.Contains(msg, "command hung") {
		t.Fatalf("expected 'command hung' in error, got %q", msg)
	}
	if !strings.Contains(msg, "last lines before silence") {
		t.Fatalf("expected 'last lines before silence', got %q", msg)
	}
	for _, want := range []string{"a", "b", "c"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("expected output line %q in error, got %q", want, msg)
		}
	}
}

func TestExecuteCommandWithActivityTimeout_HangIsErrCommandHung(t *testing.T) {
	// A command that produces no output past the inactivity window must be
	// killed and return an error matching the ErrCommandHung sentinel, so
	// retry gates can use errors.Is rather than string matching.
	_, err := executeCommandWithActivityTimeout("sleep 5", 150*time.Millisecond, 1)
	if err == nil {
		t.Fatal("expected hang error")
	}
	if !errors.Is(err, ErrCommandHung) {
		t.Fatalf("expected errors.Is(err, ErrCommandHung), got %v", err)
	}
}

func TestExecuteCommandWithActivityTimeoutInstall_HangIsErrCommandHung(t *testing.T) {
	_, err := executeCommandWithActivityTimeoutInstall("sleep 5", 150*time.Millisecond, 1)
	if err == nil {
		t.Fatal("expected hang error")
	}
	if !errors.Is(err, ErrCommandHung) {
		t.Fatalf("expected errors.Is(err, ErrCommandHung), got %v", err)
	}
}

func TestExecuteCommandWithActivityTimeout_CleanSuccessNotHung(t *testing.T) {
	// A successful command must not be classified as hung.
	out, err := executeCommandWithActivityTimeout("echo ok", 500*time.Millisecond, 1)
	if err != nil {
		t.Fatalf("expected nil error, got %v", err)
	}
	if errors.Is(err, ErrCommandHung) {
		t.Fatal("clean success must not match ErrCommandHung")
	}
	if !strings.Contains(out, "ok") {
		t.Fatalf("expected output to contain 'ok', got %q", out)
	}
}

func TestRedactSecrets(t *testing.T) {
	tests := []struct {
		name      string
		in        string
		mustMask  []string // substrings that must NOT appear in the output
		mustKeep  []string // substrings that must still appear (command/flag names)
	}{
		{
			name:     "env-style password assignment",
			in:       `PASSWORD="hunter2" apt-get install foo`,
			mustMask: []string{"hunter2"},
			mustKeep: []string{"PASSWORD=", "apt-get install foo"},
		},
		{
			name:     "prefixed token var",
			in:       `MY_TOKEN=abc123 do-thing`,
			mustMask: []string{"abc123"},
			mustKeep: []string{"MY_TOKEN=", "do-thing"},
		},
		{
			name:     "flag with equals",
			in:       `helm install --token=s3cr3t chart`,
			mustMask: []string{"s3cr3t"},
			mustKeep: []string{"helm install", "--token=", "chart"},
		},
		{
			name:     "flag with space",
			in:       `curl --password p@ss https://x`,
			mustMask: []string{"p@ss"},
			mustKeep: []string{"curl", "--password", "https://x"},
		},
		{
			name:     "secret colon assignment",
			in:       `api_secret: topsecret`,
			mustMask: []string{"topsecret"},
			mustKeep: []string{"api_secret"},
		},
		{
			name:     "no secret left untouched",
			in:       `kubectl get pods -n kube-system`,
			mustKeep: []string{"kubectl get pods -n kube-system"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := redactSecrets(tc.in)
			for _, m := range tc.mustMask {
				if strings.Contains(got, m) {
					t.Fatalf("redactSecrets(%q) = %q; must not contain secret %q", tc.in, got, m)
				}
			}
			for _, k := range tc.mustKeep {
				if !strings.Contains(got, k) {
					t.Fatalf("redactSecrets(%q) = %q; expected to keep %q", tc.in, got, k)
				}
			}
		})
	}
}

func TestExecuteCommandWithActivityTimeoutInstall_RetriesOnHang(t *testing.T) {
	// Two attempts at 150ms each: total wall time should be at least ~300ms.
	start := time.Now()
	_, err := executeCommandWithActivityTimeoutInstall("sleep 5", 150*time.Millisecond, 2)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected hang error after retries")
	}
	if !strings.Contains(err.Error(), "command hung") {
		t.Fatalf("expected 'command hung' after retries, got %q", err.Error())
	}
	// Two attempts × 150ms each = 300ms minimum. Allow generous slack for goroutine
	// scheduling and SIGKILL teardown.
	if elapsed < 300*time.Millisecond {
		t.Fatalf("expected at least 300ms elapsed across retries, got %v", elapsed)
	}
}
