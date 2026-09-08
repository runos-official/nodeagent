package agentstream

import (
	"sync"
	"testing"

	"github.com/runos-official/nodeagent/l2sec"
	"google.golang.org/protobuf/proto"
)

type blockingReplyStream struct {
	l2sec.Nodeward_NodeAgentStreamClient
	firstEntered chan struct{}
	releaseFirst chan struct{}

	mu   sync.Mutex
	sent []*l2sec.FromNodeAgent
}

func (s *blockingReplyStream) Send(message *l2sec.FromNodeAgent) error {
	s.mu.Lock()
	isFirst := len(s.sent) == 0
	s.mu.Unlock()
	if isFirst {
		close(s.firstEntered)
		<-s.releaseFirst
	}

	snapshot := proto.Clone(message).(*l2sec.FromNodeAgent)
	s.mu.Lock()
	s.sent = append(s.sent, snapshot)
	s.mu.Unlock()
	return nil
}

func TestInstructionRepliesKeepTheirOriginatingTagsAtSendBoundary(t *testing.T) {
	stream := &blockingReplyStream{
		firstEntered: make(chan struct{}),
		releaseFirst: make(chan struct{}),
	}

	streamMutex.Lock()
	previousStream := globalStream
	globalStream = stream
	streamMutex.Unlock()
	previousTag := NoContentResponse.Tag
	NoContentResponse.Tag = ""
	t.Cleanup(func() {
		streamMutex.Lock()
		globalStream = previousStream
		streamMutex.Unlock()
		NoContentResponse.Tag = previousTag
	})

	first := finalizeInstructionResponse(
		&l2sec.ToNodeAgent{Tag: "first-request"},
		NoContentResponse,
		nil,
	)
	firstDone := make(chan error, 1)
	go func() {
		firstDone <- SendToNodeward(first)
	}()
	<-stream.firstEntered

	second := finalizeInstructionResponse(
		&l2sec.ToNodeAgent{Tag: "second-request"},
		NoContentResponse,
		nil,
	)
	secondDone := make(chan error, 1)
	go func() {
		secondDone <- SendToNodeward(second)
	}()

	close(stream.releaseFirst)
	if err := <-firstDone; err != nil {
		t.Fatalf("send first response: %v", err)
	}
	if err := <-secondDone; err != nil {
		t.Fatalf("send second response: %v", err)
	}

	stream.mu.Lock()
	sent := append([]*l2sec.FromNodeAgent(nil), stream.sent...)
	stream.mu.Unlock()
	if len(sent) != 2 {
		t.Fatalf("expected two replies, got %d", len(sent))
	}
	if sent[0].Tag != "first-request" {
		t.Fatalf("first reply used tag %q", sent[0].Tag)
	}
	if sent[1].Tag != "second-request" {
		t.Fatalf("second reply used tag %q", sent[1].Tag)
	}
	for i, reply := range sent {
		if reply.Type != "NO_CONTENT" || reply.JsonB64 != "e30=" {
			t.Fatalf("reply %d changed wire payload: type=%q jsonB64=%q", i, reply.Type, reply.JsonB64)
		}
	}
}

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
