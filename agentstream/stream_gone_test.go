package agentstream

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/runos-official/nodeagent/l2sec"
)

// A request that can only be answered on the instruction stream must fail as
// soon as that stream ends. It must not wait for its own timeout, because the
// reconnect waits for the request to finish (FCR 1243).
func TestPendingRequestFailsWhenStreamEnds(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stream := &reconnectStream{incoming: make(chan *l2sec.ToNodeAgent), sent: make(chan *l2sec.FromNodeAgent, 10)}
	done := startInstructionStreamHandler(ctx, stream, handleInstructionWithSend)
	defer SetGlobalStream(nil)

	type result struct {
		err     error
		elapsed time.Duration
	}
	resCh := make(chan result, 1)
	go func() {
		start := time.Now()
		// A long timeout: only the stream ending can end this wait early.
		_, err := SendAndWaitForResponseWithTimeout(&l2sec.FromNodeAgent{Type: "GetControlPlaneNodes"}, 10*time.Second)
		resCh <- result{err, time.Since(start)}
	}()

	select {
	case <-stream.sent: // the request is now in flight
	case <-time.After(time.Second):
		t.Fatal("request was not sent")
	}
	close(stream.incoming) // stream EOF
	<-done

	select {
	case r := <-resCh:
		if r.err == nil {
			t.Fatal("request succeeded on a dead stream")
		}
		if strings.Contains(r.err.Error(), "deadline") {
			t.Fatalf("request ended by its timeout, want an immediate stream-ended error: %v", r.err)
		}
		if r.elapsed > time.Second {
			t.Fatalf("request took %v after the stream ended", r.elapsed)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("request still waiting 2 s after the stream ended")
	}
}

// A request sent on a replacement stream must not be failed by the end of the
// stream before it.
func TestStreamEndDoesNotFailRequestOnReplacementStream(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	old := &reconnectStream{incoming: make(chan *l2sec.ToNodeAgent), sent: make(chan *l2sec.FromNodeAgent, 10)}
	oldDone := startInstructionStreamHandler(ctx, old, handleInstructionWithSend)
	next := &reconnectStream{incoming: make(chan *l2sec.ToNodeAgent), sent: make(chan *l2sec.FromNodeAgent, 10)}
	nextDone := startInstructionStreamHandler(ctx, next, handleInstructionWithSend)
	defer SetGlobalStream(nil)

	resCh := make(chan error, 1)
	go func() {
		_, err := SendAndWaitForResponseWithTimeout(&l2sec.FromNodeAgent{Type: "GetControlPlaneNodes"}, 5*time.Second)
		resCh <- err
	}()
	var req *l2sec.FromNodeAgent
	select {
	case req = <-next.sent:
	case <-time.After(time.Second):
		t.Fatal("request was not sent on the replacement stream")
	}
	close(old.incoming) // the old stream ends late
	<-oldDone
	select {
	case err := <-resCh:
		t.Fatalf("request on the replacement stream ended early: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	next.incoming <- &l2sec.ToNodeAgent{Type: "GetControlPlaneNodes", Tag: req.Tag}
	select {
	case err := <-resCh:
		if err != nil {
			t.Fatalf("response not delivered: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("response not delivered")
	}
	cancel()
	close(next.incoming)
	<-nextDone
}
