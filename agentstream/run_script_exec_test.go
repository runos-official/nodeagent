package agentstream

import (
	"os"
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
