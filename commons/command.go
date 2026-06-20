package commons

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/runos-official/nodeagent/backend"
	"github.com/runos-official/nodeagent/l2sec"
	"github.com/runos-official/nodeagent/roslog"
)

// ErrCommandHung is the sentinel returned (wrapped) when a command is killed
// because it produced no output for the inactivity window. Callers gate retries
// on errors.Is(err, ErrCommandHung) rather than matching on message text.
var ErrCommandHung = errors.New("command hung")

// secretAssignRe matches secret-bearing assignments in a command line:
//   - shell/env style:  PASSWORD=foo, MY_TOKEN='bar', api_secret="baz"
//   - flag style:       --password foo, --token=bar, -p baz
// The value (group 2) is replaced with a placeholder so the command name and
// structure stay legible for debugging while the secret never reaches the log.
var secretAssignRe = regexp.MustCompile(
	`(?i)((?:[A-Za-z_][A-Za-z0-9_]*)?(?:password|passwd|secret|token|apikey|api_key|access_key|auth|credential|bearer|private_key)[A-Za-z0-9_]*\s*[=:]\s*|--?(?:password|passwd|secret|token|apikey|api-key|api_key|auth|credential|bearer)(?:[= ])\s*)(['"]?[^'"\s]+['"]?)`)

// redactSecrets returns command with the values of any secret-bearing
// assignments masked, so command lines can be logged without leaking passwords,
// tokens or keys passed inline (a live log previously contained PASSWORD="...").
// It is conservative: it keeps the command and flag NAMES, redacting only the
// matched value.
func redactSecrets(command string) string {
	return secretAssignRe.ReplaceAllString(command, "${1}[REDACTED]")
}

// ExecuteCommandStreaming runs a shell command and invokes onLine for every
// line of combined stdout+stderr as it appears, then returns the full captured
// output and the command's exit error. Use this when a caller wants to forward
// progress (e.g. kubeadm phase markers) to the user in real time instead of
// waiting for the whole command to finish.
func ExecuteCommandStreaming(command string, onLine func(string)) (string, error) {
	roslog.I("Executing command (streaming)", "command", redactSecrets(command))

	cmd := exec.Command("/bin/sh", "-c", command)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		return "", fmt.Errorf("failed to create stdout pipe: %w", err)
	}
	stderrPipe, err := cmd.StderrPipe()
	if err != nil {
		return "", fmt.Errorf("failed to create stderr pipe: %w", err)
	}

	if err := cmd.Start(); err != nil {
		return "", fmt.Errorf("failed to start command: %w", err)
	}

	var collected strings.Builder
	var mu sync.Mutex

	scan := func(r io.Reader) {
		scanner := bufio.NewScanner(r)
		scanner.Buffer(make([]byte, 64*1024), 1024*1024)
		for scanner.Scan() {
			line := scanner.Text()
			mu.Lock()
			collected.WriteString(line)
			collected.WriteByte('\n')
			mu.Unlock()
			if onLine != nil {
				onLine(line)
			}
		}
	}

	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); scan(stdoutPipe) }()
	go func() { defer wg.Done(); scan(stderrPipe) }()
	wg.Wait()

	waitErr := cmd.Wait()
	output := collected.String()
	roslog.I("Command output", "output", output)

	return output, waitErr
}

// ProcessInstallCommandsStatusAware executes an install command list, handling
// STATUS_UPDATE/PROGRESS_UPDATE markers and forwarding warnings and failures to
// Nodeward. It returns an error on the first non-ignorable command failure.
func ProcessInstallCommandsStatusAware(commandList *l2sec.InstallCommandList) error {
	// Create install context for tagging all logs
	ctx := roslog.CreateInstallContext(context.Background())

	// Start the installation progress bar
	roslog.StartInstallProgress()

	for _, cmd := range commandList.Commands {
		// Handle STATUS_UPDATE: echo commands
		if strings.HasPrefix(cmd.Command, "echo STATUS_UPDATE:") {
			statusMsg := strings.TrimPrefix(cmd.Command, "echo STATUS_UPDATE:")

			// Update the progress bar status
			roslog.UpdateInstallStatus(statusMsg)

			// Update status back to Nodeward
			if err := backend.UpdateStatus(statusMsg); err != nil {
				roslog.InstallWarning(fmt.Sprintf("Failed to update status to Nodeward: %v", err))
			}

			continue
		}

		// Handle PROGRESS_UPDATE: echo commands
		if strings.HasPrefix(cmd.Command, "echo PROGRESS_UPDATE:") {
			progressStr := strings.TrimPrefix(cmd.Command, "echo PROGRESS_UPDATE:")
			progressStr = strings.TrimSuffix(progressStr, "%")

			var percentage int
			if _, err := fmt.Sscanf(progressStr, "%d", &percentage); err == nil {
				roslog.UpdateInstallProgress(percentage)
			}

			continue
		}

		// Execute the actual command
		res, err := ExecuteCommandGetResponse2Install(ctx, cmd.Command, cmd.IgnoreFailure)

		// Forward any NODELOG_WARN: lines from the command output as user-facing
		// warnings on nodeward, so the operator sees a clear, actionable message
		// (e.g. "jq was not installed; install manually") rather than just a raw
		// command-failure dump. Runs regardless of err so post-step check commands
		// that produce these markers are honored.
		for _, line := range strings.Split(res, "\n") {
			line = strings.TrimSpace(line)
			if !strings.HasPrefix(line, "NODELOG_WARN:") {
				continue
			}
			msg := strings.TrimPrefix(line, "NODELOG_WARN:")
			roslog.InstallWarning(msg)
			if err2 := backend.AddNodelog(2, "NodeInstallationWarning", msg); err2 != nil {
				roslog.InstallError(fmt.Sprintf("Failed to log warning to Nodeward: %v", err2))
			}
		}

		if err != nil {
			logMessage := fmt.Sprintf("Command failed: %s\nError: %v\nOutput: %s", redactSecrets(cmd.Command), err, res)

			if cmd.IgnoreFailure {
				roslog.InstallWarning(logMessage)
				if err2 := backend.AddNodelog(2, "NodeInstallationWarning", logMessage); err2 != nil {
					roslog.InstallError(fmt.Sprintf("Failed to log warning to Nodeward: %v", err2))
				}
				continue
			}

			// Mark progress as failed
			roslog.FinishInstallProgress(false)

			if err2 := backend.UpdateStatus("INSTALL_ERROR"); err2 != nil {
				roslog.InstallError(fmt.Sprintf("Failed to update error status: %v", err2))
			}

			roslog.InstallError(logMessage)

			if err2 := backend.AddNodelog(1, "NodeInstallationFailure", logMessage); err2 != nil {
				roslog.InstallError(fmt.Sprintf("Failed to log to Nodeward: %v", err2))
			}
			return fmt.Errorf("%s", logMessage)
		}
	}

	roslog.FinishInstallProgress(true)
	return nil
}

// ExecuteCommandGetResponse runs command via /bin/sh and returns its combined
// output, returning an empty string on error.
func ExecuteCommandGetResponse(command string) string {
	roslog.I("Executing command", "command", redactSecrets(command))

	// Create an *exec.Cmd
	systemCmd := exec.Command("/bin/sh", "-c", command)

	// Run the command and capture its output
	output, err := systemCmd.CombinedOutput()
	if err != nil {
		roslog.E("Error in ExecuteCommandGetResponse", err)
		return ""
	}

	roslog.I("Command output", "output", string(output))

	return string(output)
}

// ExecuteCommandInDetachedSystemdScope launches command in a detached
// systemd-run scope so it survives the agent process exiting.
func ExecuteCommandInDetachedSystemdScope(command string) error {
	cmdStr := fmt.Sprintf("systemd-run --scope sh -c 'nohup %s > /dev/null 2>&1 &'", command)
	cmd := exec.Command("sh", "-c", cmdStr)

	err := cmd.Start()
	if err != nil {
		roslog.E("Error starting detached command via systemd-run", err)
		return err
	}

	roslog.I("Detached command started", "pid", cmd.Process.Pid)
	return nil
}

// ExecuteDirectCommandGetResponse runs target with args directly (no shell) and
// returns its combined output, or an error if the command fails.
func ExecuteDirectCommandGetResponse(target string, withLog bool, args ...string) (*string, error) {
	if withLog {
		// Redact each arg: secrets are sometimes passed as a single arg value.
		redactedArgs := make([]string, len(args))
		for i, a := range args {
			redactedArgs[i] = redactSecrets(a)
		}
		roslog.I("Executing direct command", "target", target, "args", redactedArgs)
	}

	// Create an *exec.Cmd
	systemCmd := exec.Command(target, args...)

	// Run the command and capture its output
	output, err := systemCmd.CombinedOutput()
	if err != nil {
		roslog.E("Command execution failed", err)
		return nil, err
	}

	outputString := string(output)

	if withLog {
		roslog.I("Command output", "output", outputString)
	}

	return &outputString, nil
}

// executeCommandWithActivityTimeout executes a command with activity-based timeout.
// It only times out if there's no output activity for inactivityTimeout duration.
// If the command keeps producing output, it can run indefinitely.
// Returns output string and error. Retries up to maxRetries times on timeout.
func executeCommandWithActivityTimeout(command string, inactivityTimeout time.Duration, maxRetries int) (string, error) {
	var lastOutput string
	var lastErr error

	for attempt := 1; attempt <= maxRetries; attempt++ {
		if attempt > 1 {
			roslog.I("Retrying command", "attempt", attempt, "maxRetries", maxRetries, "command", redactSecrets(command))
		}

		// Create context that we can cancel
		ctx, cancel := context.WithCancel(context.Background())

		// Create command with context
		systemCmd := exec.CommandContext(ctx, "/bin/sh", "-c", command)
		// Set process group so we can kill all child processes
		systemCmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

		// Get stdout and stderr pipes
		stdoutPipe, err := systemCmd.StdoutPipe()
		if err != nil {
			cancel()
			return "", fmt.Errorf("failed to create stdout pipe: %w", err)
		}
		stderrPipe, err := systemCmd.StderrPipe()
		if err != nil {
			cancel()
			return "", fmt.Errorf("failed to create stderr pipe: %w", err)
		}

		// Start the command
		if err := systemCmd.Start(); err != nil {
			cancel()
			return "", fmt.Errorf("failed to start command: %w", err)
		}

		// Channel to signal activity (any output received)
		activityCh := make(chan struct{}, 100)
		// Channel to collect output
		outputCh := make(chan string, 1)
		// Channel for errors
		errCh := make(chan error, 1)

		// Goroutine to read output and signal activity
		go func() {
			var output strings.Builder
			readers := io.MultiReader(stdoutPipe, stderrPipe)
			scanner := bufio.NewScanner(readers)

			for scanner.Scan() {
				line := scanner.Text()
				output.WriteString(line)
				output.WriteString("\n")
				// Signal activity
				select {
				case activityCh <- struct{}{}:
				default:
				}
			}

			if err := scanner.Err(); err != nil {
				roslog.E("Error reading command output", err)
			}

			outputCh <- output.String()
		}()

		// Goroutine to wait for command completion
		go func() {
			errCh <- systemCmd.Wait()
		}()

		// Monitor activity with timeout
		timer := time.NewTimer(inactivityTimeout)
		commandCompleted := false

		for !commandCompleted {
			select {
			case <-activityCh:
				// Reset timer on any activity
				if !timer.Stop() {
					<-timer.C
				}
				timer.Reset(inactivityTimeout)

			case <-timer.C:
				// No activity for inactivityTimeout duration - kill the process
				roslog.E("Command timed out due to inactivity", fmt.Errorf("no output for %v", inactivityTimeout))
				// Kill the entire process group (including child processes)
				if systemCmd.Process != nil {
					// Negative PID kills the entire process group
					syscall.Kill(-systemCmd.Process.Pid, syscall.SIGKILL)
				}
				cancel() // Also cancel the context
				// Wait for the process to actually terminate before reading output
				<-errCh
				lastOutput = <-outputCh
				lastErr = fmt.Errorf("%w (no output for %v)", ErrCommandHung, inactivityTimeout)
				commandCompleted = true

			case err := <-errCh:
				// Command completed
				timer.Stop()
				lastOutput = <-outputCh
				lastErr = err
				commandCompleted = true
			}
		}

		// Clean up context
		cancel()

		// If command succeeded, return immediately
		if lastErr == nil {
			return lastOutput, nil
		}

		// If this was a timeout error and we have retries left, continue loop
		if errors.Is(lastErr, ErrCommandHung) && attempt < maxRetries {
			continue
		}

		// Otherwise return the error
		break
	}

	return lastOutput, lastErr
}

// shouldUseRetryableExecution checks if command contains keywords that indicate
// it might hang and should use retry logic with activity monitoring
func shouldUseRetryableExecution(command string) bool {
	keywords := []string{
		"apt-get",
		"apt",
		"dpkg",
		"yum",
		"dnf",
	}

	commandLower := strings.ToLower(command)
	for _, keyword := range keywords {
		if strings.Contains(commandLower, keyword) {
			return true
		}
	}
	return false
}

// ExecuteCommandGetResponse2 runs command and returns both its combined output
// and any error, using activity-based retries for package-manager commands.
func ExecuteCommandGetResponse2(command string) (string, error) {
	roslog.I("Executing command", "command", redactSecrets(command))

	// Check if this command should use retryable execution with activity monitoring
	if shouldUseRetryableExecution(command) {
		roslog.I("Using retryable execution with activity monitoring", "command", redactSecrets(command))
		output, err := executeCommandWithActivityTimeout(command, 60*time.Second, 3)

		// Log output regardless of error
		roslog.I("Command output", "output", output)

		if err != nil {
			roslog.E("Command execution failed after retries", err)
			return output, err
		}

		return output, nil
	}

	// Standard execution for non-whitelisted commands
	// Create an *exec.Cmd
	systemCmd := exec.Command("/bin/sh", "-c", command)

	// Run the command and capture its output
	output, err := systemCmd.CombinedOutput()
	outputString := string(output)

	// Log output regardless of error
	roslog.I("Command output", "output", outputString)

	if err != nil {
		roslog.E("Command execution failed", err)
		return outputString, err // Return both output and error
	}

	return outputString, nil
}

// ExecuteCommandGetResponse2Install is like ExecuteCommandGetResponse2 but uses
// the enhanced installation logger with less verbose output.
// When ignoreFailure is true, retryable commands run a single attempt instead of
// three so a hung non-fatal command costs ~60s instead of ~3 minutes.
func ExecuteCommandGetResponse2Install(ctx context.Context, command string, ignoreFailure bool) (string, error) {
	// Log to file for debugging/audit trail (with context for phase tagging)
	roslog.Ictx(ctx, "Executing install command", "command", redactSecrets(command))

	// Display the command under the progress bar
	roslog.SetCurrentCommand(command)
	defer roslog.SetCurrentCommand("") // Clear command display when done

	// Check if this command should use retryable execution with activity monitoring
	if shouldUseRetryableExecution(command) {
		// Strict commands get more headroom because apt's silent lock-wait can
		// legitimately go quiet for 30-90s between status messages. Best-effort
		// commands stay at 60s + 1 attempt so a hung non-fatal command costs ~1
		// minute, not several.
		retries := 3
		timeout := 120 * time.Second
		if ignoreFailure {
			retries = 1
			timeout = 60 * time.Second
		}
		output, err := executeCommandWithActivityTimeoutInstall(command, timeout, retries)

		// Log output to file (not stdout) with context
		roslog.Ictx(ctx, "Install command output", "output", output)

		if err != nil {
			roslog.Ectx(ctx, "Install command failed", err, "command", redactSecrets(command))
			// Also show on stdout for immediate visibility
			roslog.InstallError(fmt.Sprintf("Command failed: %s", redactSecrets(command)))
			if len(output) > 0 {
				roslog.InstallError(fmt.Sprintf("Output: %s", strings.TrimSpace(output)))
			}
			return output, err
		}

		// Success: output logged to file, user sees only progress bar
		return output, nil
	}

	// Standard execution for non-whitelisted commands
	systemCmd := exec.Command("/bin/sh", "-c", command)
	output, err := systemCmd.CombinedOutput()
	outputString := string(output)

	// Log output to file (not stdout) with context
	roslog.Ictx(ctx, "Install command output", "output", outputString)

	if err != nil {
		err = describeExitError(err)
		roslog.Ectx(ctx, "Install command failed", err, "command", redactSecrets(command))
		// Also show on stdout for immediate visibility
		roslog.InstallError(fmt.Sprintf("Command failed: %s", redactSecrets(command)))
		if len(outputString) > 0 {
			roslog.InstallError(fmt.Sprintf("Output: %s", strings.TrimSpace(outputString)))
		}
		return outputString, err
	}

	// Success: output logged to file, user sees only progress bar
	return outputString, nil
}

// describeExitError unwraps *exec.ExitError into a readable string with exit
// code and signal info, so failure logs say "exit status 100" or "killed by
// signal 9 (killed)" instead of an opaque Go error.
func describeExitError(err error) error {
	exitErr, ok := err.(*exec.ExitError)
	if !ok {
		return err
	}
	if ws, ok := exitErr.ProcessState.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
		return fmt.Errorf("killed by signal %d (%s)", ws.Signal(), ws.Signal())
	}
	return fmt.Errorf("exit status %d", exitErr.ExitCode())
}

// lastNLines returns the last n non-empty lines of s joined by newline+indent,
// for inclusion in a single log message.
func lastNLines(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n  ")
}

// executeCommandWithActivityTimeoutInstall is like executeCommandWithActivityTimeout
// but uses the install logger for less verbose output
func executeCommandWithActivityTimeoutInstall(command string, inactivityTimeout time.Duration, maxRetries int) (string, error) {
	var lastOutput string
	var lastErr error

	for attempt := 1; attempt <= maxRetries; attempt++ {
		if attempt > 1 {
			roslog.InstallWarning(fmt.Sprintf("Retrying command (attempt %d/%d)", attempt, maxRetries))
		}

		// Create context that we can cancel
		ctx, cancel := context.WithCancel(context.Background())

		// Create command with context
		systemCmd := exec.CommandContext(ctx, "/bin/sh", "-c", command)
		// Set process group so we can kill all child processes
		systemCmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

		// Get stdout and stderr pipes
		stdoutPipe, err := systemCmd.StdoutPipe()
		if err != nil {
			cancel()
			return "", fmt.Errorf("failed to create stdout pipe: %w", err)
		}
		stderrPipe, err := systemCmd.StderrPipe()
		if err != nil {
			cancel()
			return "", fmt.Errorf("failed to create stderr pipe: %w", err)
		}

		// Start the command
		if err := systemCmd.Start(); err != nil {
			cancel()
			return "", fmt.Errorf("failed to start command: %w", err)
		}

		// Channel to signal activity (any output received)
		activityCh := make(chan struct{}, 100)
		// Channel to collect output
		outputCh := make(chan string, 1)
		// Channel for errors
		errCh := make(chan error, 1)

		// Goroutine to read output and signal activity
		go func() {
			var output strings.Builder
			readers := io.MultiReader(stdoutPipe, stderrPipe)
			scanner := bufio.NewScanner(readers)

			for scanner.Scan() {
				line := scanner.Text()
				output.WriteString(line)
				output.WriteString("\n")

				// Show activity in progress bar (advance spinner)
				roslog.ShowInstallActivity()

				// Signal activity
				select {
				case activityCh <- struct{}{}:
				default:
				}
			}

			if err := scanner.Err(); err != nil {
				// Only log scanner errors in debug mode during install
				roslog.InstallDebug(fmt.Sprintf("Scanner error: %v", err))
			}

			outputCh <- output.String()
		}()

		// Goroutine to wait for command completion
		go func() {
			errCh <- systemCmd.Wait()
		}()

		// Monitor activity with timeout
		timer := time.NewTimer(inactivityTimeout)
		commandCompleted := false

		for !commandCompleted {
			select {
			case <-activityCh:
				// Reset timer on any activity
				if !timer.Stop() {
					<-timer.C
				}
				timer.Reset(inactivityTimeout)

			case <-timer.C:
				// No activity for inactivityTimeout duration - kill the process
				roslog.InstallWarning(fmt.Sprintf("Command timed out (no output for %v)", inactivityTimeout))
				// Kill the entire process group (including child processes)
				if systemCmd.Process != nil {
					// Negative PID kills the entire process group
					syscall.Kill(-systemCmd.Process.Pid, syscall.SIGKILL)
				}
				cancel() // Also cancel the context
				// Wait for the process to actually terminate before reading output
				<-errCh
				lastOutput = <-outputCh
				tail := lastNLines(lastOutput, 10)
				if tail == "" {
					lastErr = fmt.Errorf("%w (no output for %v); produced no output before silence", ErrCommandHung, inactivityTimeout)
				} else {
					lastErr = fmt.Errorf("%w (no output for %v); last lines before silence:\n  %s", ErrCommandHung, inactivityTimeout, tail)
				}
				commandCompleted = true

			case err := <-errCh:
				// Command completed
				timer.Stop()
				lastOutput = <-outputCh
				if err != nil {
					lastErr = describeExitError(err)
				} else {
					lastErr = nil
				}
				commandCompleted = true
			}
		}

		// Clean up context
		cancel()

		// If command succeeded, return immediately
		if lastErr == nil {
			return lastOutput, nil
		}

		// If this was a timeout error and we have retries left, continue loop
		if errors.Is(lastErr, ErrCommandHung) && attempt < maxRetries {
			continue
		}

		// Otherwise return the error
		break
	}

	return lastOutput, lastErr
}

// RebootServer reboots the host, preferring systemctl and falling back to reboot.
func RebootServer() error {
	roslog.I("Rebooting server...")
	cmd := exec.Command("systemctl", "reboot")
	err := cmd.Run()

	// If systemctl fails for some reason, try the traditional reboot command
	if err != nil {
		cmd = exec.Command("reboot")
		err = cmd.Run()
	}

	return err
}

// GetHostname returns the host's name via the hostname command.
func GetHostname() (string, error) {
	cmd := exec.Command("hostname")
	output, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(output)), nil
}

// GetExternalIPAddress returns the node's public IPv4 address, trying several
// providers in turn and returning an error if none yield a valid address.
func GetExternalIPAddress() (string, error) {
	providers := [][]string{
		{"curl", "-fsS", "https://api64.ipify.org"},
		{"dig", "+short", "myip.opendns.com", "@resolver1.opendns.com"},
		{"curl", "-fsS", "https://checkip.amazonaws.com"},
	}

	ipv4Regex := regexp.MustCompile(`^([0-9]{1,3}\.){3}[0-9]{1,3}$`)

	for _, p := range providers {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		cmd := exec.CommandContext(ctx, p[0], p[1:]...)
		output, err := cmd.Output()
		cancel()

		if err != nil {
			continue
		}
		ip := strings.TrimSpace(string(output))
		// explicitly strip all non-ASCII just in case
		ip = strings.Map(func(r rune) rune {
			if r >= 32 && r <= 126 {
				return r
			}
			return -1
		}, ip)

		if ipv4Regex.MatchString(ip) {
			return ip, nil
		}
	}

	return "", fmt.Errorf("could not determine external IPv4 address")
}

// GetAllFilesInDirectory returns a map of file path to file contents for every
// regular file under directory, or an empty map if the directory does not exist.
func GetAllFilesInDirectory(directory string) (map[string]string, error) {
	// if the directory does not exist, return an empty map
	if _, err := os.Stat(directory); os.IsNotExist(err) {
		return map[string]string{}, nil
	}

	cmd := exec.Command("find", directory, "-type", "f")
	output, err := cmd.Output()
	if err != nil {
		return nil, err
	}

	files := strings.Split(strings.TrimSpace(string(output)), "\n")
	fileMap := make(map[string]string)

	for _, filePath := range files {
		content, err := os.ReadFile(filePath)
		if err != nil {
			return nil, err
		}
		fileMap[filePath] = string(content)
	}

	return fileMap, nil
}
