package agentstream

import (
	"context"
	"fmt"
	"github.com/google/uuid"
	"github.com/runos-official/nodeagent/l2sec"
	"github.com/runos-official/nodeagent/roslog"
	"sync"
	"time"
)

// Global variables for managing outbound messages
var (
	// Stream client reference
	globalStream l2sec.Nodeward_NodeAgentStreamClient

	// Mutex to protect access to the stream
	streamMutex sync.Mutex

	// pendingResponses tracks response channels for tagged messages
	pendingResponses sync.Map // Maps tag (UUID string) to response channel
)

// SetGlobalStream sets the stream client for sending outbound messages
func SetGlobalStream(client l2sec.Nodeward_NodeAgentStreamClient) {
	streamMutex.Lock()
	defer streamMutex.Unlock()
	globalStream = client
}

// SendToNodeward sends a message to Nodeward
func SendToNodeward(msg *l2sec.FromNodeAgent) error {
	streamMutex.Lock()
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
