package commons

import (
	"strings"
	"testing"
	"time"
)

// FCR 1661: a command that exits fast must always return all of its output.
// The old runners lost it at random ("read |0: file already closed", empty
// string) because Wait closed the pipes while another goroutine still read them.

type runnerFn func(command string, inactivity time.Duration, retries int) (string, error)

func runners() map[string]runnerFn {
	return map[string]runnerFn{
		"retryable": executeCommandWithActivityTimeout,
		"install":   executeCommandWithActivityTimeoutInstall,
	}
}

func TestFastCommandKeepsAllOutput(t *testing.T) {
	cases := []struct {
		name    string
		command string
		want    string
	}{
		// A version query: a short value with no trailing newline.
		{"no trailing newline", "printf 1.2.3", "1.2.3\n"},
		{"one line", "echo 2.2.1-0ubuntu1~24.04.4", "2.2.1-0ubuntu1~24.04.4\n"},
		{"true then echo", "true; echo x", "x\n"},
		{"stderr only", "echo oops 1>&2", "oops\n"},
	}
	const rounds = 500
	for rname, run := range runners() {
		for _, tc := range cases {
			t.Run(rname+"/"+tc.name, func(t *testing.T) {
				empty := 0
				for i := 0; i < rounds; i++ {
					out, err := run(tc.command, 5*time.Second, 1)
					if err != nil {
						t.Fatalf("round %d: unexpected error %v", i, err)
					}
					if out != tc.want {
						empty++
						if empty == 1 {
							t.Errorf("round %d: output %q, want %q", i, out, tc.want)
						}
					}
				}
				if empty > 0 {
					t.Fatalf("%d of %d runs returned the wrong output", empty, rounds)
				}
			})
		}
	}
}

func TestOutputKeepsStdoutAndStderrOrder(t *testing.T) {
	for rname, run := range runners() {
		t.Run(rname, func(t *testing.T) {
			out, err := run("echo one; echo two 1>&2; echo three", 5*time.Second, 1)
			if err != nil {
				t.Fatalf("unexpected error %v", err)
			}
			if out != "one\ntwo\nthree\n" {
				t.Fatalf("got %q", out)
			}
		})
	}
}

func TestFailedCommandKeepsOutputAndExitCode(t *testing.T) {
	for rname, run := range runners() {
		t.Run(rname, func(t *testing.T) {
			out, err := run("echo before; exit 3", 5*time.Second, 1)
			if err == nil {
				t.Fatal("expected an error for exit 3")
			}
			if !strings.Contains(err.Error(), "exit status 3") {
				t.Fatalf("expected exit status 3 in %q", err.Error())
			}
			if out != "before\n" {
				t.Fatalf("got %q", out)
			}
		})
	}
}

func TestVeryLongLineIsNotTruncated(t *testing.T) {
	// 200 KB on one line: the old scanner stopped at 64 KB and dropped the rest.
	for rname, run := range runners() {
		t.Run(rname, func(t *testing.T) {
			out, err := run("head -c 200000 /dev/zero | tr '\\0' a; echo; echo end", 10*time.Second, 1)
			if err != nil {
				t.Fatalf("unexpected error %v", err)
			}
			if !strings.HasSuffix(out, "\nend\n") || len(out) != 200000+1+4 {
				t.Fatalf("output length %d, want %d with trailing end line", len(out), 200000+1+4)
			}
		})
	}
}

func TestSteadyOutputKeepsACommandAlive(t *testing.T) {
	// 6 lines, 100 ms apart, with a 300 ms inactivity window: runs 600 ms in
	// total and must not be killed, because every line is activity.
	for rname, run := range runners() {
		t.Run(rname, func(t *testing.T) {
			out, err := run("for i in 1 2 3 4 5 6; do echo line$i; sleep 0.1; done", 300*time.Millisecond, 1)
			if err != nil {
				t.Fatalf("unexpected error %v", err)
			}
			if got := strings.Count(out, "line"); got != 6 {
				t.Fatalf("got %d lines in %q", got, out)
			}
		})
	}
}

func TestTimedOutCommandReturnsWhatItPrinted(t *testing.T) {
	for rname, run := range runners() {
		t.Run(rname, func(t *testing.T) {
			out, err := run("echo started; sleep 5", 200*time.Millisecond, 1)
			if err == nil {
				t.Fatal("expected a hang error")
			}
			if out != "started\n" {
				t.Fatalf("got %q", out)
			}
		})
	}
}

func TestBackgroundChildHoldingThePipeDoesNotHangTheRunner(t *testing.T) {
	// The shell exits at once, but a background child still holds stdout. The
	// command succeeded: the runner must return its output after the short
	// WaitDelay, not wait for the child.
	for rname, run := range runners() {
		t.Run(rname, func(t *testing.T) {
			start := time.Now()
			out, err := run("sleep 30 & echo started", 20*time.Second, 1)
			elapsed := time.Since(start)
			if err != nil {
				t.Fatalf("unexpected error %v", err)
			}
			if out != "started\n" {
				t.Fatalf("got %q", out)
			}
			if elapsed > attemptWaitDelay+5*time.Second {
				t.Fatalf("took %v, expected about %v", elapsed, attemptWaitDelay)
			}
		})
	}
}

func TestLineCollectorHandlesSplitWrites(t *testing.T) {
	var calls int
	c := &lineCollector{onLine: func() { calls++ }}
	for _, part := range []string{"ab", "c\nde", "f\r\n", "tail"} {
		if n, err := c.Write([]byte(part)); err != nil || n != len(part) {
			t.Fatalf("write %q: n=%d err=%v", part, n, err)
		}
	}
	if got, want := c.String(), "abc\ndef\ntail\n"; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
	if calls != 2 {
		t.Fatalf("onLine called %d times, want 2", calls)
	}
	// String is repeatable.
	if got := c.String(); got != "abc\ndef\ntail\n" {
		t.Fatalf("second String(): %q", got)
	}
}
