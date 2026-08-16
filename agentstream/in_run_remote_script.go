package agentstream

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"syscall"
	"time"

	"github.com/runos-official/nodeagent/commons"
	"github.com/runos-official/nodeagent/config"
	pb "github.com/runos-official/nodeagent/l2sec"
	"github.com/runos-official/nodeagent/roslog"
)

// RunRemoteScriptRequestType is the instruction type that fetches and runs a remote script.
const RunRemoteScriptRequestType = "RUN_REMOTE_SCRIPT"

// remoteScriptFetchTimeout bounds fetching the script body so a slow/hung
// endpoint cannot tie up a worker.
const remoteScriptFetchTimeout = 30 * time.Second

// remoteScriptMaxBytes caps how large a fetched script may be (defends against a
// hostile endpoint streaming an unbounded body to exhaust disk/memory).
const remoteScriptMaxBytes = 8 << 20 // 8 MiB

// scriptParamRe constrains request.Script to a template id / path token. It is a
// strict allowlist: a leading "/t/" template path, or a bare token, made up of
// word chars, dots, dashes and slashes only. Anything with shell metacharacters,
// spaces, quotes, $(), ;, |, &, backticks, etc. is rejected. This is the primary
// defense against the old `curl ... | bash` shell-injection sink.
var scriptParamRe = regexp.MustCompile(`^/?(?:t/)?[\w.][\w.\-/]*$`)

// validateScriptParam returns nil if script is an acceptable template-id/path
// token, else an error explaining the rejection. It additionally rejects path
// traversal ("..") so a token can't escape the intended namespace.
func validateScriptParam(script string) error {
	if script == "" {
		return fmt.Errorf("script parameter is empty")
	}
	if len(script) > 512 {
		return fmt.Errorf("script parameter too long")
	}
	if !scriptParamRe.MatchString(script) {
		return fmt.Errorf("script parameter %q contains disallowed characters; expected a template id/path token", script)
	}
	if strings.Contains(script, "..") {
		return fmt.Errorf("script parameter %q must not contain '..'", script)
	}
	return nil
}

type runRemoteScriptRequest struct {
	Script          string            `json:"script"`
	Params          map[string]string `json:"params"`
	RunInBackground bool              `json:"runInBackground"`
	// How long the script may run, in seconds. Absent or non-positive means the
	// default below. Conductor sends the same budget it gives the gRPC call, so
	// the agent gives up before the caller does and the verdict is the agent's
	// rather than a transport timeout with no detail.
	TimeoutSeconds int `json:"timeoutSeconds"`
}

// runRemoteScriptResponse is deliberately BACKWARD COMPATIBLE: `response` still
// carries the script's output and remains the only field an older conductor
// reads. What changed is that it is now stdout ALONE, with stderr and the exit
// code as separate fields. Merging the two (CombinedOutput) meant any script
// that wrote a diagnostic to stderr corrupted its own JSON verdict, and the exit
// code was discarded entirely, so a `set -e` script that aborted before printing
// anything reported success with an empty body.
type runRemoteScriptResponse struct {
	Response string `json:"response"`
	Stderr   string `json:"stderr"`
	ExitCode int    `json:"exitCode"`
	// True only when the agent killed the script for exceeding its budget, which
	// is a different fact from a script that chose to exit non-zero.
	TimedOut bool `json:"timedOut"`
}

// defaultScriptBudget bounds a script whose request names no timeout. Before
// this existed a hung script held one of the five instruction workers forever,
// and five of them wedged the whole instruction path for the node.
const defaultScriptBudget = 15 * time.Minute

// maxScriptBudget caps what a request may ask for, so a bad budget cannot
// reintroduce the unbounded case by another route.
const maxScriptBudget = 60 * time.Minute

// scriptBudget turns the requested seconds into the window the run gets.
func scriptBudget(seconds int) time.Duration {
	if seconds <= 0 {
		return defaultScriptBudget
	}
	budget := time.Duration(seconds) * time.Second
	if budget > maxScriptBudget {
		return maxScriptBudget
	}
	return budget
}

// scriptStreamCap bounds each captured stream. Nothing bounded them before, so
// a script that dumped a log held all of it in the agent's memory and then built
// a response nodeward could not accept: its gRPC limit is 16 MB, and exceeding
// it loses the WHOLE reply, verdict included, rather than the excess.
//
// 2 MiB per stream leaves room for what the reply does to the bytes on the way:
// 4 MiB of output, JSON-escaped, then base64-encoded at 4/3. No script's verdict
// needs anywhere near it; a script that produces more is producing a log, and a
// log belongs in the node log, not in an instruction reply.
const scriptStreamCap = 2 << 20

// scriptRun is what one foreground script run produced.
type scriptRun struct {
	Stdout   string
	Stderr   string
	ExitCode int
	TimedOut bool
}

// runScriptFile runs `/bin/bash <path>` with stdout and stderr captured
// SEPARATELY and the whole run bounded by budget.
//
// Setpgid plus a group kill, not a plain Cancel: bash's own death leaves its
// children running, and a grandchild still holding the output pipes keeps Wait
// blocked long after the budget is spent. The kill goes to -pid, which is the
// whole process group.
func runScriptFile(path string, budget time.Duration) scriptRun {
	ctx, cancel := context.WithTimeout(context.Background(), budget)
	defer cancel()

	cmd := exec.CommandContext(ctx, "/bin/bash", path)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		return killProcessGroup(cmd.Process.Pid)
	}
	// A grandchild that survived the group kill must not hold Wait open.
	cmd.WaitDelay = 5 * time.Second

	stdout := &cappedBuffer{limit: scriptStreamCap}
	stderr := &cappedBuffer{limit: scriptStreamCap}
	cmd.Stdout = stdout
	cmd.Stderr = stderr

	runErr := cmd.Run()

	return scriptVerdict(runErr, ctx.Err(), budget, stdout.String(), stderr.String())
}

// killProcessGroup SIGKILLs the whole group led by pid.
//
// ESRCH means the group is already gone, which is the normal end of a script
// that finished on its own right as the deadline fired. exec replaces the
// process's own result with whatever Cancel returns UNLESS that is
// os.ErrProcessDone, so returning the raw ESRCH turned a clean run into an
// error: the caller saw exit code -1 with the good verdict sitting in stdout.
func killProcessGroup(pid int) error {
	if err := syscall.Kill(-pid, syscall.SIGKILL); err != nil {
		if errors.Is(err, syscall.ESRCH) {
			return os.ErrProcessDone
		}
		return err
	}
	return nil
}

// scriptVerdict turns what cmd.Run produced into the verdict the caller acts on.
// Separate from runScriptFile so the three-way relationship between the run
// error, the context and the reported exit code can be tested directly: the
// timing window that gets it wrong is microseconds wide and cannot be driven
// from a real process.
func scriptVerdict(runErr, ctxErr error, budget time.Duration, stdout, stderr string) scriptRun {
	run := scriptRun{
		Stdout: stdout,
		Stderr: stderr,
		// The context is not enough on its own. It expires whenever a script runs
		// to the end of its budget, INCLUDING when the script finished at that
		// moment and exited 0. Only the run error says which of the two happened,
		// so a timeout is a failed run whose deadline also passed, never a clean
		// run that happened to end on the buzzer.
		TimedOut: runErr != nil && errors.Is(ctxErr, context.DeadlineExceeded),
	}
	if runErr != nil {
		var exitErr *exec.ExitError
		if errors.As(runErr, &exitErr) {
			run.ExitCode = exitErr.ExitCode()
		} else {
			run.ExitCode = -1
		}
		// A killed process reports -1, which on its own reads like "could not
		// run". The timeout flag is what separates the two, and the reason is
		// appended to stderr so an operator reading only the text still sees it.
		if run.TimedOut {
			run.Stderr += fmt.Sprintf("\nrunos: the node agent killed this script after %s\n", budget)
		}
	}
	return run
}

// cappedBuffer collects at most limit bytes and counts what it dropped. Writes
// past the limit always report success: reporting a short write would make the
// copier close the pipe, and the script would die of SIGPIPE for the crime of
// being verbose.
type cappedBuffer struct {
	buf     bytes.Buffer
	limit   int
	dropped int64
}

func (c *cappedBuffer) Write(p []byte) (int, error) {
	written := len(p)
	if room := c.limit - c.buf.Len(); room > 0 {
		if room > len(p) {
			room = len(p)
		}
		c.buf.Write(p[:room])
		p = p[room:]
	}
	c.dropped += int64(len(p))
	return written, nil
}

// String is what the buffer collected, with a marker when it dropped anything.
// Silently truncated output is worse than none: the reader cannot tell a script
// that printed half a verdict from one that was cut off.
func (c *cappedBuffer) String() string {
	if c.dropped == 0 {
		return c.buf.String()
	}
	return c.buf.String() + fmt.Sprintf(
		"\n[runos: output truncated at %d bytes, %d further bytes were dropped]\n",
		c.limit, c.dropped)
}

// buildRemoteScriptURL assembles the fetch URL from the validated script token
// and params, using net/url so the path/query are properly escaped (no string
// concatenation into a shell). It mirrors the original routing: a "/t/..."
// template path is served from the conductor; any other token is served from the
// installer's /scripts endpoint with the params passed as a base64 "i" query arg.
func buildRemoteScriptURL(script string, params map[string]string) (string, error) {
	if strings.HasPrefix(script, "/t/") {
		base, err := url.Parse(config.GetConductorURL())
		if err != nil {
			return "", fmt.Errorf("invalid conductor URL: %w", err)
		}
		ref, err := url.Parse(script)
		if err != nil {
			return "", fmt.Errorf("invalid script path: %w", err)
		}
		return base.ResolveReference(ref).String(), nil
	}

	base, err := url.Parse(config.GetROSInstallerURL())
	if err != nil {
		return "", fmt.Errorf("invalid installer URL: %w", err)
	}
	jsonBytes, err := json.Marshal(params)
	if err != nil {
		return "", fmt.Errorf("error marshalling params: %w", err)
	}
	paramsB64 := base64.StdEncoding.EncodeToString(jsonBytes)

	ref := &url.URL{Path: "/scripts"}
	resolved := base.ResolveReference(ref)
	q := resolved.Query()
	q.Set("t", script)
	q.Set("i", paramsB64)
	resolved.RawQuery = q.Encode()
	return resolved.String(), nil
}

// fetchScriptToTempFile downloads rawURL with a verifying HTTP client (TLS verify
// ON) into a freshly created 0600 temp file and returns its path. The caller owns
// removing the file. The body is size-capped.
func fetchScriptToTempFile(rawURL string) (string, error) {
	client := &http.Client{Timeout: remoteScriptFetchTimeout}

	req, err := http.NewRequest(http.MethodGet, rawURL, nil)
	if err != nil {
		return "", fmt.Errorf("error building script request: %w", err)
	}

	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("error fetching script: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("script fetch returned HTTP %d", resp.StatusCode)
	}

	tmp, err := os.CreateTemp("", "runos-script-*.sh")
	if err != nil {
		return "", fmt.Errorf("error creating temp script file: %w", err)
	}
	// os.CreateTemp already creates with 0600; set explicitly to be safe.
	if err := tmp.Chmod(0600); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return "", fmt.Errorf("error setting temp script permissions: %w", err)
	}

	n, err := io.Copy(tmp, io.LimitReader(resp.Body, remoteScriptMaxBytes))
	if cerr := tmp.Close(); cerr != nil && err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(tmp.Name())
		return "", fmt.Errorf("error writing script to temp file: %w", err)
	}
	if n == 0 {
		os.Remove(tmp.Name())
		return "", fmt.Errorf("fetched script was empty")
	}

	return tmp.Name(), nil
}

// HandleRunRemoteScript fetches a script from the configured installer/conductor
// endpoint and runs it, returning the captured output (or a background marker).
//
// Security: request.Script is strictly validated as a template-id/path token,
// the fetch URL is built with net/url (no shell), the body is downloaded over a
// verifying HTTPS client into a 0600 temp file, and the script is executed
// argv-style as `/bin/bash <tmpfile>` (no `sh -c`, no `curl | bash` pipe).
func HandleRunRemoteScript(b64ScriptData *pb.ToNodeAgent) (*pb.FromNodeAgent, error) {
	roslog.I("Executing HandleRunRemoteScript")
	jsonData, err := base64.StdEncoding.DecodeString(b64ScriptData.JsonB64)
	if err != nil {
		roslog.E("Error decoding JSON payload", err)
		return nil, err
	}

	var request runRemoteScriptRequest
	if err := json.Unmarshal(jsonData, &request); err != nil {
		roslog.E("Error unmarshalling JSON payload", err)
		return nil, err
	}

	if err := validateScriptParam(request.Script); err != nil {
		roslog.E("Rejected RUN_REMOTE_SCRIPT: invalid script parameter", err)
		return nil, err
	}

	scriptURL, err := buildRemoteScriptURL(request.Script, request.Params)
	if err != nil {
		roslog.E("Error building remote script URL", err)
		return nil, err
	}

	tmpPath, err := fetchScriptToTempFile(scriptURL)
	if err != nil {
		roslog.E("Error fetching remote script", err)
		return nil, err
	}

	var run scriptRun
	if request.RunInBackground {
		// Run argv-style in a detached scope. The temp file is intentionally NOT
		// removed here: the background bash reads it after this handler returns.
		// It lives in the OS temp dir (0600) and is cleaned by tmp reaping.
		if err := commons.ExecuteDetachedSystemdScopeArgv("/bin/bash", tmpPath); err != nil {
			os.Remove(tmpPath)
			return nil, err
		}
		run = scriptRun{Stdout: "Script is running in the background."}
	} else {
		budget := scriptBudget(request.TimeoutSeconds)
		run = runScriptFile(tmpPath, budget)
		os.Remove(tmpPath)
		if run.ExitCode != 0 {
			roslog.E("Remote script execution failed", fmt.Errorf("exit code %d (timedOut=%v)", run.ExitCode, run.TimedOut),
				"stdoutBytes", len(run.Stdout), "stderrBytes", len(run.Stderr))
		}
	}

	// Log only metadata; the script's captured output may contain secrets and
	// must not be persisted to the on-disk log.
	roslog.I("Script result", "stdoutBytes", len(run.Stdout), "stderrBytes", len(run.Stderr),
		"exitCode", run.ExitCode, "timedOut", run.TimedOut)

	response := runRemoteScriptResponse{
		Response: run.Stdout,
		Stderr:   run.Stderr,
		ExitCode: run.ExitCode,
		TimedOut: run.TimedOut,
	}

	responseJson, err := json.Marshal(response)
	if err != nil {
		roslog.E("Error marshalling response JSON", err)
		return nil, err
	}
	responseJsonB64 := base64.StdEncoding.EncodeToString(responseJson)

	return &pb.FromNodeAgent{
		JsonB64: responseJsonB64,
		Type:    "RUN_REMOTE_SCRIPT_RESPONSE",
	}, nil
}
