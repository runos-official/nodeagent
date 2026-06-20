package agentstream

import (
	"strings"
	"testing"
)

func TestQueueReinstallCommand_RejectsEmpty(t *testing.T) {
	// Empty / whitespace-only commands are rejected before any filesystem write,
	// so this is safe to run without root.
	for _, in := range []string{"", "   ", "\t\n"} {
		if err := QueueReinstallCommand(in); err == nil {
			t.Errorf("expected empty reinstall command %q to be rejected", in)
		} else if !strings.Contains(err.Error(), "empty") {
			t.Errorf("expected 'empty' error for %q, got %v", in, err)
		}
	}
}
