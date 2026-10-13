package agentstream

import (
	"context"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/runos-official/nodeagent/l2sec"
)

type reconnectStream struct {
	l2sec.Nodeward_NodeAgentStreamClient
	incoming chan *l2sec.ToNodeAgent
	sent     chan *l2sec.FromNodeAgent
}

func (s *reconnectStream) Recv() (*l2sec.ToNodeAgent, error) {
	msg, ok := <-s.incoming
	if !ok {
		return nil, io.EOF
	}
	return msg, nil
}
func (s *reconnectStream) Send(msg *l2sec.FromNodeAgent) error { s.sent <- msg; return nil }
func (s *reconnectStream) CloseSend() error                    { return nil }

func TestStreamDisconnectDoesNotWaitForActiveInstruction(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	old := &reconnectStream{incoming: make(chan *l2sec.ToNodeAgent), sent: make(chan *l2sec.FromNodeAgent, 10)}
	entered, release, finished := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var once sync.Once
	defer once.Do(func() { close(release) })
	done := startInstructionStreamHandler(ctx, old, func(in *l2sec.ToNodeAgent, send func(*l2sec.FromNodeAgent) error) *l2sec.FromNodeAgent {
		close(entered)
		<-release
		defer close(finished)
		if err := send(&l2sec.FromNodeAgent{Type: "PROGRESS", Tag: in.Tag + "/1"}); err == nil {
			t.Error("expired instruction sent progress")
		}
		return &l2sec.FromNodeAgent{Type: "DONE", Tag: in.Tag}
	})
	old.incoming <- &l2sec.ToNodeAgent{Type: "BLOCK", Tag: "old-request"}
	<-entered
	close(old.incoming)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("stream disconnect waits for an active instruction")
	}
	next := &reconnectStream{incoming: make(chan *l2sec.ToNodeAgent), sent: make(chan *l2sec.FromNodeAgent, 10)}
	nextDone := StartInstructionStreamHandler(ctx, next)
	next.incoming <- &l2sec.ToNodeAgent{Type: "UNKNOWN", Tag: "new-request"}
	select {
	case msg := <-next.sent:
		if msg.Tag != "new-request" {
			t.Fatalf("unexpected reply: %v", msg)
		}
	case <-time.After(time.Second):
		t.Fatal("replacement stream cannot process instructions")
	}
	once.Do(func() { close(release) })
	<-finished
	select {
	case msg := <-next.sent:
		t.Fatalf("expired instruction replied on replacement stream: %v", msg)
	case <-time.After(50 * time.Millisecond):
	}
	cancel()
	close(next.incoming)
	<-nextDone
	SetGlobalStream(nil)
}

func TestDisconnectDiscardsQueuedInstructionsAndKeepsExecutionLimit(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	old := &reconnectStream{incoming: make(chan *l2sec.ToNodeAgent), sent: make(chan *l2sec.FromNodeAgent, 100)}
	entered := make(chan string, 100)
	release := make(chan struct{})
	var once sync.Once
	defer once.Do(func() { close(release) })
	handle := func(in *l2sec.ToNodeAgent, send func(*l2sec.FromNodeAgent) error) *l2sec.FromNodeAgent {
		entered <- in.Tag
		<-release
		return &l2sec.FromNodeAgent{Type: "DONE", Tag: in.Tag}
	}
	done := startInstructionStreamHandler(ctx, old, handle)
	for i := 0; i < numWorkers; i++ {
		old.incoming <- &l2sec.ToNodeAgent{Type: "BLOCK", Tag: "active"}
		select {
		case <-entered:
		case <-time.After(time.Second):
			t.Fatal("worker did not start")
		}
	}
	for i := 0; i < 100; i++ {
		old.incoming <- &l2sec.ToNodeAgent{Type: "BLOCK", Tag: "queued"}
	}
	close(old.incoming)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("full queue blocked disconnect")
	}
	next := &reconnectStream{incoming: make(chan *l2sec.ToNodeAgent), sent: make(chan *l2sec.FromNodeAgent, 100)}
	nextDone := startInstructionStreamHandler(ctx, next, handle)
	next.incoming <- &l2sec.ToNodeAgent{Type: "BLOCK", Tag: "replacement"}
	select {
	case tag := <-entered:
		t.Fatalf("execution limit exceeded: %s", tag)
	case <-time.After(50 * time.Millisecond):
	}
	once.Do(func() { close(release) })
	select {
	case tag := <-entered:
		if tag != "replacement" {
			t.Fatalf("expired queued instruction executed: %s", tag)
		}
	case <-time.After(time.Second):
		t.Fatal("replacement instruction did not start")
	}
	select {
	case msg := <-next.sent:
		if msg.Tag != "replacement" {
			t.Fatalf("unexpected reply: %v", msg)
		}
	case <-time.After(time.Second):
		t.Fatal("replacement instruction did not finish")
	}
	cancel()
	close(next.incoming)
	<-nextDone
	select {
	case tag := <-entered:
		t.Fatalf("expired queued instruction executed: %s", tag)
	case <-time.After(50 * time.Millisecond):
	}
	SetGlobalStream(nil)
}

func TestUpgradeProgressUsesInstructionSender(t *testing.T) {
	var messages []*l2sec.FromNodeAgent
	response := handleInstructionWithSend(&l2sec.ToNodeAgent{Type: UpgradeNodeK8sRequestType, Tag: "upgrade", JsonB64: "not-base64"}, func(msg *l2sec.FromNodeAgent) error {
		messages = append(messages, msg)
		return nil
	})
	if response == nil || len(messages) == 0 {
		t.Fatal("upgrade failure must send progress through the instruction sender")
	}
}
