package agentstream

import (
	"context"
	"errors"
	"fmt"
	"github.com/google/uuid"
	"github.com/runos-official/nodeagent/l2sec"
	"github.com/runos-official/nodeagent/roslog"
	"sync"
	"time"
)

// errStreamEnded is returned to a request that was waiting for a reply when the
// stream it was sent on ended.
var errStreamEnded = errors.New("stream to Nodeward ended before the reply arrived")

// Global variables for managing outbound messages
var (
	// Stream client reference
	globalStream l2sec.Nodeward_NodeAgentStreamClient

	// Mutex to protect access to the stream
	streamMutex sync.Mutex

	// pendingResponses tracks response channels for tagged messages
	pendingResponses sync.Map // Maps tag (UUID string) to response channel
)

// streamEpoch marks one stream. Its channel closes when that stream ends, or when
// another stream replaces it. A request that waits for a reply on the stream
// stops waiting at that point, because the reply can no longer arrive.
type streamEpoch struct {
	ended chan struct{}
	once  sync.Once
}

func newStreamEpoch() *streamEpoch { return &streamEpoch{ended: make(chan struct{})} }

func (e *streamEpoch) end() { e.once.Do(func() { close(e.ended) }) }

var (
	epochMu      sync.Mutex
	currentEpoch = func() *streamEpoch { e := newStreamEpoch(); e.end(); return e }()
)

// beginStreamEpoch ends the epoch of the stream before it and starts a new one.
func beginStreamEpoch(active bool) *streamEpoch {
	epochMu.Lock()
	defer epochMu.Unlock()
	currentEpoch.end()
	e := newStreamEpoch()
	if !active {
		e.end()
	}
	currentEpoch = e
	return e
}

func getStreamEpoch() *streamEpoch {
	epochMu.Lock()
	defer epochMu.Unlock()
	return currentEpoch
}

// SetGlobalStream sets the stream client for sending outbound messages
func SetGlobalStream(client l2sec.Nodeward_NodeAgentStreamClient) {
	setGlobalStream(client)
}

func setGlobalStream(client l2sec.Nodeward_NodeAgentStreamClient) *streamEpoch {
	streamMutex.Lock()
	defer streamMutex.Unlock()
	globalStream = client
	return beginStreamEpoch(client != nil)
}

// SendToNodeward sends a message to Nodeward.
//
// This is the CONTROL path: instruction responses, status, everything the agent sends today. Its
// behaviour is unchanged, including returning the send error synchronously, which every caller
// relies on. It is counted as control while it waits so that a bulk sender (goal 31 session data)
// gives way to it; see egress.go.
func SendToNodeward(msg *l2sec.FromNodeAgent) error {
	controlWaiting.Add(1)
	streamMutex.Lock()
	controlWaiting.Add(-1)
	defer streamMutex.Unlock()

	// Check if we have a valid stream client
	if globalStream == nil {
		return fmt.Errorf("stream not initialized")
	}

	// Ensure the message has a tag
	if msg.Tag == "" {
		// Generate a new UUID for messages without tags
		msg.Tag = uuid.New().String()
	}

	if err := checkSendSize(msg); err != nil {
		roslog.E("Refusing to send an oversized message to Nodeward", err, "type", msg.Type, "tag", msg.Tag)
		return err
	}

	// Send the message
	payloadBytes := len(msg.JsonB64)
	err := globalStream.Send(msg)
	if err != nil {
		roslog.E("Error sending message to Nodeward", err, "type", msg.Type, "tag", msg.Tag, "bytes", payloadBytes)
		return err
	}

	roslog.I("Sent message to Nodeward", "type", msg.Type, "tag", msg.Tag, "bytes", payloadBytes)
	return nil
}

// sendOnStream binds an instruction response to the connection that delivered the instruction.
func sendOnStream(ctx context.Context, stream l2sec.Nodeward_NodeAgentStreamClient, msg *l2sec.FromNodeAgent) error {
	controlWaiting.Add(1)
	streamMutex.Lock()
	controlWaiting.Add(-1)
	defer streamMutex.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	if msg.Tag == "" {
		msg.Tag = uuid.New().String()
	}
	if err := checkSendSize(msg); err != nil {
		return err
	}
	return stream.Send(msg)
}

// SendBulkToNodeward sends session data (goal 31), yielding to control traffic first.
//
// Same stream, same mutex, same error contract. The only difference is that it waits while the
// node has control traffic to send, up to a ceiling. That ceiling matters: without it a node under
// sustained control load would stall a terminal for ever, which is the opposite failure and just
// as bad as starving the control plane.
//
// The caller is the session goroutine, so making it wait IS the backpressure: it stops reading
// from the far end rather than buffering, and no frame is ever dropped.
func SendBulkToNodeward(msg *l2sec.FromNodeAgent) error {
	bulkGate.Lock()
	defer bulkGate.Unlock()
	if waitForControlIdle() {
		roslog.I("Bulk frame gave way to control traffic", "type", msg.Type)
	}
	streamMutex.Lock()
	defer streamMutex.Unlock()

	if globalStream == nil {
		return fmt.Errorf("stream not initialized")
	}
	if msg.Tag == "" {
		msg.Tag = uuid.New().String()
	}
	if err := globalStream.Send(msg); err != nil {
		roslog.E("Error sending bulk frame to Nodeward", err, "type", msg.Type, "tag", msg.Tag)
		return err
	}
	return nil
}

// SendAndWaitForResponse sends a message and waits for a response with the same tag
func SendAndWaitForResponse(ctx context.Context, msg *l2sec.FromNodeAgent) (*l2sec.ToNodeAgent, error) {
	// Ensure the message has a UUID tag
	tagID := uuid.New()
	msg.Tag = tagID.String()

	// Create a buffered response channel
	responseChan := make(chan *l2sec.ToNodeAgent, 1)

	// Store the response channel with the tag as key
	pendingResponses.Store(tagID.String(), responseChan)

	// Ensure cleanup when we're done
	defer pendingResponses.Delete(tagID.String())

	// The stream this request goes out on. If it ends, no reply can arrive.
	epoch := getStreamEpoch()

	// Send the message
	if err := SendToNodeward(msg); err != nil {
		return nil, fmt.Errorf("failed to send message: %w", err)
	}

	// Wait for either a response, context cancellation, or timeout
	select {
	case response, ok := <-responseChan:
		if !ok {
			// Channel was closed
			return nil, fmt.Errorf("response channel was closed unexpectedly")
		}
		return response, nil
	case <-epoch.ended:
		// A reply can arrive only on the stream that carried the request.
		// Prefer a reply that arrived at the same moment.
		select {
		case response, ok := <-responseChan:
			if ok {
				return response, nil
			}
		default:
		}
		return nil, errStreamEnded
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// SendAndWaitForResponseWithTimeout is a convenience function that adds a timeout to SendAndWaitForResponse
func SendAndWaitForResponseWithTimeout(msg *l2sec.FromNodeAgent, timeout time.Duration) (*l2sec.ToNodeAgent, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	return SendAndWaitForResponse(ctx, msg)
}

// HandleInstructionAsResponse checks if an instruction is a response to a pending request
// Returns true if the instruction was handled as a response
func HandleInstructionAsResponse(instruction *l2sec.ToNodeAgent) bool {
	tag := instruction.Tag

	// Check if this is a response to a request we're waiting for
	if respChanValue, exists := pendingResponses.Load(tag); exists {
		// This is a response to our request
		responseChan := respChanValue.(chan *l2sec.ToNodeAgent)

		// Try to send the response to the waiting goroutine
		select {
		case responseChan <- instruction:
			// Successfully delivered the response
			roslog.I("Delivered response", "tag", tag)
			return true
		default:
			// Channel is full or closed
			roslog.W("Could not deliver response", nil, "tag", tag)
		}

		// Remove from the pending responses as we've handled it
		pendingResponses.Delete(tag)
		return true
	}

	return false
}
