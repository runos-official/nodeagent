package agentstream

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"regexp"
	"strings"
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
}

type runRemoteScriptResponse struct {
	Response string `json:"response"`
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

	var commandResponse string
	if request.RunInBackground {
		// Run argv-style in a detached scope. The temp file is intentionally NOT
		// removed here: the background bash reads it after this handler returns.
		// It lives in the OS temp dir (0600) and is cleaned by tmp reaping.
		if err := commons.ExecuteDetachedSystemdScopeArgv("/bin/bash", tmpPath); err != nil {
			os.Remove(tmpPath)
			return nil, err
		}
		commandResponse = "Script is running in the background."
	} else {
		out, runErr := exec.Command("/bin/bash", tmpPath).CombinedOutput()
		os.Remove(tmpPath)
		commandResponse = string(out)
		if runErr != nil {
			roslog.E("Remote script execution failed", runErr, "bytes", len(commandResponse))
		}
	}

	// Log only metadata; the script's captured output may contain secrets and
	// must not be persisted to the on-disk log.
	roslog.I("Script result", "bytes", len(commandResponse))

	response := runRemoteScriptResponse{
		Response: commandResponse,
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
