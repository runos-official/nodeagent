package certrenew

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// DefaultStatePath is the state file of the renewal loop. /run is a tmpfs,
// so a reboot removes the file; the loop writes it again at its first cycle.
const DefaultStatePath = "/run/runos/certificate-renewal.json"

const (
	stateVersion = 1
	stateMode    = 0o644
	// maxErrorMessage is the longest lastErrorMessage, in bytes.
	maxErrorMessage = 512
)

// Values of lastResult and of the result member of the attempt log line.
const (
	ResultOK               = "ok"
	ResultWaiting          = "waiting"
	ResultTransientFailure = "transient_failure"
	ResultRefused          = "refused"
	ResultExpired          = "expired"
)

// State is the content of the state file. It holds no secret. Times are
// RFC 3339 UTC; a member with no value is omitted, except the two counts.
type State struct {
	Version             int    `json:"version"`
	PID                 int    `json:"pid"`
	UpdatedAt           string `json:"updatedAt"`
	HeldSerial          string `json:"heldSerial,omitempty"`
	NotAfter            string `json:"notAfter,omitempty"`
	RenewalDueAt        string `json:"renewalDueAt,omitempty"`
	LastAttemptAt       string `json:"lastAttemptAt,omitempty"`
	LastResult          string `json:"lastResult"`
	Outcome             string `json:"outcome,omitempty"`
	LastErrorCode       string `json:"lastErrorCode,omitempty"`
	LastErrorMessage    string `json:"lastErrorMessage,omitempty"`
	ConsecutiveFailures int    `json:"consecutiveFailures"`
	NextAttemptAt       string `json:"nextAttemptAt,omitempty"`
	Attempts            int    `json:"attempts"`
}

// stamp formats t for the state file; the zero time gives "".
func stamp(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

// writeState writes s to path through a temporary file in the same
// directory and a rename, so a reader never sees a partial file.
func writeState(path string, s State) error {
	if len(s.LastErrorMessage) > maxErrorMessage {
		s.LastErrorMessage = cutMessage(s.LastErrorMessage)
	}
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return fmt.Errorf("encode the renewal state: %w", err)
	}
	b = append(b, '\n')
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create %s: %w", dir, err)
	}
	f, err := os.CreateTemp(dir, ".certificate-renewal-*.tmp")
	if err != nil {
		return fmt.Errorf("create the renewal state file: %w", err)
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if _, err := f.Write(b); err != nil {
		f.Close()
		return fmt.Errorf("write the renewal state file: %w", err)
	}
	if err := f.Chmod(stateMode); err != nil {
		f.Close()
		return fmt.Errorf("set the mode of the renewal state file: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("close the renewal state file: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("replace the renewal state file: %w", err)
	}
	return nil
}

// ReadState reads the state file at path.
func ReadState(path string) (State, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return State{}, err
	}
	var s State
	if err := json.Unmarshal(b, &s); err != nil {
		return State{}, fmt.Errorf("decode %s: %w", path, err)
	}
	return s, nil
}

// cutMessage shortens s to maxErrorMessage bytes on a rune boundary.
func cutMessage(s string) string {
	if len(s) <= maxErrorMessage {
		return s
	}
	r := []rune(s)
	for len(string(r)) > maxErrorMessage {
		r = r[:len(r)-1]
	}
	return string(r)
}
