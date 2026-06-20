package k8s

import (
	"context"
	"fmt"
	"os/exec"
	"time"

	"github.com/runos-official/nodeagent/roslog"
)

const (
	// Default timeout for kubectl commands in heartbeat path
	defaultCmdTimeout = 3 * time.Second
	// Number of retries for timed out commands
	defaultRetries = 2
)

// execWithTimeout runs a command with timeout and retries.
// Returns output and error. On timeout, retries up to maxRetries times.
func execWithTimeout(timeout time.Duration, retries int, name string, args ...string) ([]byte, error) {
	var lastErr error
	var lastOutput []byte

	fullCmd := append([]string{name}, args...)

	for attempt := 0; attempt <= retries; attempt++ {
		if attempt > 0 {
			roslog.W("Retrying command after timeout", nil, "attempt", attempt, "maxRetries", retries, "command", fullCmd)
		}

		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		cmd := exec.CommandContext(ctx, name, args...)
		output, err := cmd.CombinedOutput()
		cancel()

		if err == nil {
			return output, nil
		}

		lastErr = err
		lastOutput = output

		// Check if it was a timeout
		if ctx.Err() == context.DeadlineExceeded {
			roslog.W("Command timed out", nil, "command", fullCmd, "timeout", timeout, "attempt", attempt+1, "maxAttempts", retries+1)
			continue // retry on timeout
		}

		// Non-timeout error, return immediately
		return output, err
	}

	roslog.E("Command failed after all retries", lastErr, "command", fullCmd, "attempts", retries+1)
	return lastOutput, fmt.Errorf("command timed out after %d retries: %w", retries, lastErr)
}

// kubectlWithTimeout runs kubectl with timeout and retries
func kubectlWithTimeout(args ...string) ([]byte, error) {
	return execWithTimeout(defaultCmdTimeout, defaultRetries, "kubectl", args...)
}
