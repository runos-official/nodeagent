package agentstream

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// writeScript drops body into a temp file and returns its path.
func writeScript(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "script.sh")
	if err := os.WriteFile(path, []byte(body), 0o700); err != nil {
		t.Fatalf("could not write the fixture script: %v", err)
	}
	return path
}

// The verdict a script's caller acts on has three parts and every one of them was lost before
// this fix: stdout and stderr arrived merged (so a stray log line corrupted the JSON verdict),
// the exit code was discarded, and nothing bounded the run.

func TestRunScriptFile_SeparatesStdoutFromStderr(t *testing.T) {
	path := writeScript(t, "#!/bin/bash\nprintf '{\"ok\":true}\\n'\nprintf 'a warning\\n' >&2\n")

	run := runScriptFile(path, 10*time.Second)

	if run.Stdout != "{\"ok\":true}\n" {
		t.Errorf("stdout must carry the verdict alone, got %q", run.Stdout)
	}
	if run.Stderr != "a warning\n" {
		t.Errorf("stderr must be reported separately, got %q", run.Stderr)
	}
	if run.ExitCode != 0 {
		t.Errorf("expected exit code 0, got %d", run.ExitCode)
	}
	if run.TimedOut {
		t.Error("a script that finished must not be reported as timed out")
	}
}

func TestRunScriptFile_ReportsTheExitCode(t *testing.T) {
	// `set -e` scripts abort mid-way with no output at all. Without the code the caller cannot
	// tell an empty verdict from a script that never reached its print.
	path := writeScript(t, "#!/bin/bash\nset -e\nfalse\necho never\n")

	run := runScriptFile(path, 10*time.Second)

	if run.ExitCode != 1 {
		t.Errorf("expected exit code 1, got %d", run.ExitCode)
	}
	if run.Stdout != "" {
		t.Errorf("expected no stdout, got %q", run.Stdout)
	}
}

func TestRunScriptFile_TimesOutAndKillsTheWholeProcessGroup(t *testing.T) {
	// A hung script wedges one of five workers forever, and killing only the bash parent leaves
	// the grandchild holding the pipe. Both halves are proven here: the verdict says timedOut,
	// and the grandchild whose pid the script reports is gone afterwards.
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "child.pid")
	path := writeScript(t, "#!/bin/bash\nsleep 60 &\necho $! > "+pidFile+"\nwait\n")

	started := time.Now()
	run := runScriptFile(path, 1*time.Second)
	elapsed := time.Since(started)

	if !run.TimedOut {
		t.Fatalf("expected a timeout verdict, got exit=%d stderr=%q", run.ExitCode, run.Stderr)
	}
	if elapsed > 20*time.Second {
		t.Errorf("the run should end near the budget, took %s", elapsed)
	}

	raw, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatalf("the fixture script did not record its child pid: %v", err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil {
		t.Fatalf("unreadable child pid %q: %v", raw, err)
	}
	// The grandchild is not our child, so Wait cannot reap it; signal 0 asks whether it exists.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if err := syscall.Kill(pid, 0); err != nil {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Errorf("the grandchild pid %d survived the timeout, so the process group was not killed", pid)
}

// TestScriptVerdict_AScriptThatFinishedIsNotATimeout is the reported defect. A
// script that printed its verdict and exited 0 microseconds before its budget
// ran out was reported as exitCode -1, timedOut true, with the good verdict
// sitting in stdout. Conductor reads timedOut and throws the verdict away, so a
// successful migration or provisioning step read as a hung node.
//
// The context ALWAYS expires in that window, so the context alone cannot say
// what happened. Only the run error can: cmd.Run returns nil when the process
// exited on its own with status 0, whatever the clock says.
func TestScriptVerdict_AScriptThatFinishedIsNotATimeout(t *testing.T) {
	budget := 30 * time.Second
	deadline := context.DeadlineExceeded

	// Real errors from real processes, so the exit codes are the ones the kernel
	// reports rather than a hand-built ProcessState that reads as "exited 0".
	exitedSeven := exec.Command("/bin/sh", "-c", "exit 7").Run()
	if exitedSeven == nil {
		t.Fatal("the fixture process should have exited 7")
	}
	wasKilled := exec.Command("/bin/sh", "-c", "kill -9 $$").Run()
	if wasKilled == nil {
		t.Fatal("the fixture process should have been killed")
	}

	tests := []struct {
		name         string
		runErr       error
		ctxErr       error
		wantTimedOut bool
		wantExitCode int
	}{
		{
			name:         "finished right at the budget",
			runErr:       nil,
			ctxErr:       deadline,
			wantTimedOut: false,
			wantExitCode: 0,
		},
		{
			name:         "finished well inside the budget",
			runErr:       nil,
			ctxErr:       nil,
			wantTimedOut: false,
			wantExitCode: 0,
		},
		{
			name:         "killed for running past the budget",
			runErr:       wasKilled,
			ctxErr:       deadline,
			wantTimedOut: true,
			wantExitCode: -1, // a signalled process has no exit status
		},
		{
			name:         "chose to exit non-zero, no timeout",
			runErr:       exitedSeven,
			ctxErr:       nil,
			wantTimedOut: false,
			wantExitCode: 7,
		},
		{
			name:         "could not be started at all",
			runErr:       errors.New("fork/exec /bin/bash: no such file or directory"),
			ctxErr:       nil,
			wantTimedOut: false,
			wantExitCode: -1,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			run := scriptVerdict(tc.runErr, tc.ctxErr, budget, "{\"ok\":true}\n", "")

			if run.TimedOut != tc.wantTimedOut {
				t.Errorf("timedOut = %v, want %v", run.TimedOut, tc.wantTimedOut)
			}
			if run.ExitCode != tc.wantExitCode {
				t.Errorf("exitCode = %d, want %d", run.ExitCode, tc.wantExitCode)
			}
			if run.Stdout != "{\"ok\":true}\n" {
				t.Errorf("the verdict must survive whatever the clock said, got %q", run.Stdout)
			}
			if !tc.wantTimedOut && strings.Contains(run.Stderr, "killed this script") {
				t.Errorf("a script that was not killed must not be told it was, got %q", run.Stderr)
			}
		})
	}
}

// TestKillProcessGroup_ReportsAnAlreadyDeadGroupAsDone: exec calls Cancel when
// the deadline fires, and whatever Cancel returns replaces the process's own
// result unless it is os.ErrProcessDone. A group that already exited is not a
// kill failure, it is the normal end of a script that finished on time, so
// returning the raw ESRCH turned a clean run into an error.
func TestKillProcessGroup_ReportsAnAlreadyDeadGroupAsDone(t *testing.T) {
	cmd := exec.Command("/bin/sh", "-c", "exit 0")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatalf("could not start the fixture process: %v", err)
	}
	pid := cmd.Process.Pid
	if err := cmd.Wait(); err != nil {
		t.Fatalf("the fixture process should have exited 0: %v", err)
	}

	err := killProcessGroup(pid)
	if !errors.Is(err, os.ErrProcessDone) {
		t.Errorf("killing a group that already exited must report ErrProcessDone, got %v", err)
	}
}

// TestRunScriptFile_CapsHugeOutput: both buffers grew without limit, so a script
// that dumped a log held all of it in the agent's memory and then produced a
// response too large for nodeward's 16 MB gRPC limit, which loses the whole
// reply rather than the excess. The output is capped and says it was cut.
func TestRunScriptFile_CapsHugeOutput(t *testing.T) {
	// 64 MiB on each stream, far past the cap.
	path := writeScript(t, "#!/bin/bash\n"+
		"head -c 67108864 /dev/zero | tr '\\0' 'o'\n"+
		"head -c 67108864 /dev/zero | tr '\\0' 'e' >&2\n")

	run := runScriptFile(path, 120*time.Second)

	if run.ExitCode != 0 {
		t.Errorf("capping the output must not fail the script, got exit %d stderr tail %q", run.ExitCode, tail(run.Stderr))
	}
	if len(run.Stdout) > scriptStreamCap+1024 {
		t.Errorf("stdout is %d bytes, cap is %d", len(run.Stdout), scriptStreamCap)
	}
	if len(run.Stderr) > scriptStreamCap+1024 {
		t.Errorf("stderr is %d bytes, cap is %d", len(run.Stderr), scriptStreamCap)
	}
	if !strings.Contains(run.Stdout, "truncated") {
		t.Errorf("truncated stdout must say so, tail: %q", tail(run.Stdout))
	}
	if !strings.Contains(run.Stderr, "truncated") {
		t.Errorf("truncated stderr must say so, tail: %q", tail(run.Stderr))
	}
}

func tail(s string) string {
	if len(s) > 200 {
		return s[len(s)-200:]
	}
	return s
}

func TestScriptBudget_FallsBackWhenTheRequestNamesNoTimeout(t *testing.T) {
	if got := scriptBudget(45); got != 45*time.Second {
		t.Errorf("expected the requested budget, got %s", got)
	}
	if got := scriptBudget(0); got != defaultScriptBudget {
		t.Errorf("expected the default budget, got %s", got)
	}
	if got := scriptBudget(-5); got != defaultScriptBudget {
		t.Errorf("a negative budget must fall back, got %s", got)
	}
	if got := scriptBudget(999999); got != maxScriptBudget {
		t.Errorf("expected the cap, got %s", got)
	}
}
