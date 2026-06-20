package agentstream

import (
	"testing"

	"github.com/runos-official/nodeagent/l2sec"
)

// TestSafeHandleInstruction_RecoversFromPanic is the core guard for the key
// fix: a panic in the instruction-handling path must NOT escape the worker
// goroutine (which would crash the process). Passing a nil instruction makes
// handleInstruction panic on the first field access; safeHandleInstruction must
// recover and return an ERROR response instead of propagating the panic.
//
// goleak is not a dependency of this module, so the full "drive the stream with
// a fake blocking Recv and assert no goroutine leak" test is intentionally
// omitted. This unit test covers the panic-recovery boundary directly.
func TestSafeHandleInstruction_RecoversFromPanic(t *testing.T) {
	var resp *l2sec.FromNodeAgent

	// If the recover boundary is missing, this call panics and fails the test
	// via the test runner; with the boundary it returns a normal ERROR value.
	func() {
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("panic escaped safeHandleInstruction: %v", r)
			}
		}()
		resp = safeHandleInstruction(0, nil)
	}()

	if resp == nil {
		t.Fatal("expected an ERROR response, got nil")
	}
	if resp.Type != "ERROR" {
		t.Fatalf("expected response type ERROR, got %q", resp.Type)
	}
}

// TestSafeHandleInstruction_PassesThroughNormalResponse confirms the boundary
// does not alter the normal (non-panicking) path: an unknown instruction type
// flows through handleInstruction and returns a tagged ERROR response.
func TestSafeHandleInstruction_PassesThroughNormalResponse(t *testing.T) {
	in := &l2sec.ToNodeAgent{Type: "DEFINITELY_NOT_A_REAL_TYPE", Tag: "tag-123"}
	resp := safeHandleInstruction(1, in)
	if resp == nil {
		t.Fatal("expected a response, got nil")
	}
	if resp.Type != "ERROR" {
		t.Fatalf("expected ERROR for unknown type, got %q", resp.Type)
	}
	if resp.Tag != "tag-123" {
		t.Fatalf("expected tag to be preserved, got %q", resp.Tag)
	}
}
