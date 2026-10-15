package agentstream

import (
	"context"
	"fmt"
	"runtime/debug"

	"github.com/runos-official/nodeagent/l2sec"
	"github.com/runos-official/nodeagent/roslog"
	"google.golang.org/protobuf/proto"
)

// Number of worker goroutines to spawn
const numWorkers = 5

// Reconnects share the limit while instructions from an expired stream finish.
var instructionSlots = make(chan struct{}, numWorkers)

// instructionTask represents a task to process an instruction
type instructionTask struct {
	Instruction *l2sec.ToNodeAgent
}

// StartInstructionStreamHandler initializes the stream handler that processes instructions from Nodeward
// The returned channel closes after reception stops. Active instructions can finish after it closes.
func StartInstructionStreamHandler(ctx context.Context, stream l2sec.Nodeward_NodeAgentStreamClient) <-chan struct{} {
	return startInstructionStreamHandler(ctx, stream, handleInstructionWithSend)
}

func startInstructionStreamHandler(ctx context.Context, stream l2sec.Nodeward_NodeAgentStreamClient, handle func(*l2sec.ToNodeAgent, func(*l2sec.FromNodeAgent) error) *l2sec.FromNodeAgent) <-chan struct{} {
	ctx, cancel := context.WithCancel(ctx)
	send := func(msg *l2sec.FromNodeAgent) error { return sendOnStream(ctx, stream, msg) }
	done := make(chan struct{})

	// Set the global stream for outbound messages
	epoch := setGlobalStream(stream)

	// Create channel for instructions
	instructionChan := make(chan instructionTask, 20) // Buffer for incoming instructions

	for i := 0; i < numWorkers; i++ {
		go processInstructions(ctx, i, instructionChan, func(in *l2sec.ToNodeAgent) *l2sec.FromNodeAgent {
			return handle(in, send)
		}, send)
	}

	// Start the receiving goroutine
	go func() {
		defer close(done)

		defer func() {
			// Stop queued work immediately. Active operations finish without blocking reconnect.
			// Do not close the queue: dispatch goroutines can still select a send during cancellation.
			cancel()
			// Fail requests that wait for a reply on this stream. The reply cannot arrive,
			// and the reconnect waits for them.
			epoch.end()
			CloseAllSessions("the node's connection to RunOS went down")
		}()

		// Create a channel to receive instructions asynchronously.
		// Buffer size 1 plus a ctx.Done guard on each send so the inner
		// receive goroutine can never block forever during shutdown (the
		// outer loop may have already returned and stopped draining).
		recvChan := make(chan *l2sec.ToNodeAgent, 1)
		recvErrChan := make(chan error, 1)

		// Start a goroutine to receive from the stream
		go func() {
			for {
				instruction, err := stream.Recv()
				if err != nil {
					select {
					case recvErrChan <- err:
					case <-ctx.Done():
					}
					return
				}
				select {
				case recvChan <- instruction:
				case <-ctx.Done():
					return
				}
			}
		}()

		for {
			select {
			case <-ctx.Done():
				roslog.I("Stream handler context cancelled, initiating shutdown")
				// Close the stream to unblock the Recv() call
				if closer, ok := stream.(interface{ CloseSend() error }); ok {
					if err := closer.CloseSend(); err != nil {
						roslog.E("Error closing stream", err)
					}
				}
				return
			case err := <-recvErrChan:
				roslog.E("Error receiving instruction", err)
				return
			case instruction := <-recvChan:
				roslog.I("Received instruction", "type", instruction.Type, "tag", instruction.Tag)

				// Check if this is a response to an outbound message
				if HandleInstructionAsResponse(instruction) {
					// If it was handled as a response, we're done
					continue
				}

				// A SESSION IS NOT AN INSTRUCTION (goal 31, decision 1). Handled here, on the
				// receiver, beside HandleInstructionAsResponse and BEFORE the pool, so an
				// interactive terminal never holds one of the five workers for its whole life.
				// Five terminals would otherwise leave this node unable to apply a manifest, run a
				// script or report its status, and no value of numWorkers fixes that.
				//
				// HandleSessionFrame returns immediately in every case; see its comment for the
				// one thing it does wait on and why that wait is the backpressure.
				if IsSessionFrame(instruction) {
					HandleSessionFrame(instruction)
					continue
				}

				// Process the instruction in a separate goroutine
				go func(instruction *l2sec.ToNodeAgent) {
					// Send the instruction to the worker pool
					select {
					case <-ctx.Done():
						return
					case instructionChan <- instructionTask{Instruction: instruction}:
						// Successfully queued the task
					}
				}(instruction)
			}
		}
	}()

	return done
}

// processInstructions runs queued instructions and sends replies through the originating stream.
func processInstructions(ctx context.Context, workerID int, instructionChan <-chan instructionTask, handle func(*l2sec.ToNodeAgent) *l2sec.FromNodeAgent, send func(*l2sec.FromNodeAgent) error) {
	roslog.I("Worker started", "worker_id", workerID)

	for {
		select {
		case <-ctx.Done():
			roslog.I("Worker shutting down due to context cancellation", "worker_id", workerID)
			return
		case task, ok := <-instructionChan:
			if !ok {
				// Channel closed
				roslog.I("Worker shutting down due to closed channel", "worker_id", workerID)
				return
			}

			select {
			case instructionSlots <- struct{}{}:
			case <-ctx.Done():
				return
			}
			if ctx.Err() != nil {
				<-instructionSlots
				return
			}

			// Process the instruction
			roslog.I("Worker processing instruction", "worker_id", workerID, "tag", task.Instruction.Tag, "type", task.Instruction.Type)

			// Handle the instruction and get response. safeHandleInstruction
			// wraps handleInstruction in a recover boundary so a panic in any
			// handler turns into an ERROR response for that instruction instead
			// of unwinding the worker goroutine (and crashing the process).
			response := safeHandleInstructionWith(workerID, task.Instruction, handle)
			<-instructionSlots

			// A nil response is an explicit "do not ack" signal (used by
			// VIP_ASSIGN so Nodeward hands off to the next candidate on its
			// 5s timeout).
			if response == nil {
				roslog.I("Suppressing ack", "worker_id", workerID, "tag", task.Instruction.Tag, "type", task.Instruction.Type)
				continue
			}

			// Send the response if context is still valid
			select {
			case <-ctx.Done():
				roslog.I("Context cancelled, abandoning response", "worker_id", workerID, "tag", task.Instruction.Tag)
			default:
				// The sender checks cancellation again under the stream mutex.
				if err := send(response); err != nil {
					roslog.E("Error sending response", err, "worker_id", workerID, "tag", task.Instruction.Tag)
				} else {
					roslog.I("Sent response", "worker_id", workerID, "tag", task.Instruction.Tag)
				}
			}
		}
	}
}

// safeHandleInstruction calls handleInstruction inside a recover boundary so a
// panic in any handler can never crash the worker goroutine (and with it the
// process). On panic it logs the value plus a stack trace and returns an ERROR
// response tagged for correlation, so the worker continues serving instructions.
func safeHandleInstruction(workerID int, instruction *l2sec.ToNodeAgent) (response *l2sec.FromNodeAgent) {
	return safeHandleInstructionWith(workerID, instruction, handleInstruction)
}

func safeHandleInstructionWith(
	workerID int,
	instruction *l2sec.ToNodeAgent,
	handle func(*l2sec.ToNodeAgent) *l2sec.FromNodeAgent,
) (response *l2sec.FromNodeAgent) {
	defer func() {
		if r := recover(); r != nil {
			// Read tag/type defensively: the panic may itself have come from a
			// nil instruction, and the recovery path must never panic again.
			var tag, typ string
			if instruction != nil {
				tag = instruction.Tag
				typ = instruction.Type
			}
			roslog.E("Recovered from panic while handling instruction",
				fmt.Errorf("panic: %v", r),
				"worker_id", workerID,
				"tag", tag,
				"type", typ,
				"stack", string(debug.Stack()),
			)
			response = &l2sec.FromNodeAgent{
				JsonB64: fmt.Sprintf("internal error handling instruction: %v", r),
				Type:    "ERROR",
				Tag:     tag,
			}
		}
	}()

	return handle(instruction)
}

// handleInstruction processes a single instruction and returns a response
func handleInstruction(instruction *l2sec.ToNodeAgent) *l2sec.FromNodeAgent {
	return handleInstructionWithSend(instruction, SendToNodeward)
}

func handleInstructionWithSend(instruction *l2sec.ToNodeAgent, send func(*l2sec.FromNodeAgent) error) *l2sec.FromNodeAgent {
	var response *l2sec.FromNodeAgent
	var err error

	roslog.I("Handling instruction", "type", instruction.Type, "tag", instruction.Tag)

	// Create a response with the same tag to pair it with the request
	response = &l2sec.FromNodeAgent{
		JsonB64: "",
		Type:    "UNKNOWN_TYPE",
		Tag:     instruction.Tag,
	}

	// Process the instruction based on its type
	switch instruction.Type {
	case SetVpnPeersRequestType:
		response, err = HandleSetVpnPeers(instruction)

	case GetNodeStatusRequestType:
		response, err = HandleGetNodeStatus(instruction)

	case GetClusterJoinCommandRequestType:
		response, err = HandleGetClusterJoinCommand(instruction)

	case DeleteCRRequestType:
		response, err = HandleDeleteCR(instruction)

	case ApplyOperatorRequestType:
		response, err = HandleApplyOperator(instruction)

	case ApplyCRRequestType:
		response, err = HandleApplyCR(instruction)

	case RunKubectlCommandRequestType:
		response, err = HandleRunKubectlCommand(instruction)

	case RunRemoteScriptRequestType:
		response, err = HandleRunRemoteScript(instruction)

	case InstallHelmChartType:
		response, err = HandleInstallHelmChart(instruction)

	case RunWebRequestType:
		response, err = HandleRunWebRequest(instruction)

	case UninstallNodeRequestType:
		response, err = HandleUninstallNode()

	case RemoveEtcdMemberRequestType:
		response, err = HandleRemoveEtcdMember(instruction)

	case UpdateDnsmasqRequestType:
		response, err = HandleUpdateDnsmasq(instruction)

	case UninstallHelmChartType:
		response, err = HandleUninstallHelmChart(instruction)

	case UpgradeNodeK8sRequestType:
		progress := NewProgressTagger(instruction)
		if progress == nil {
			progress = &ProgressTagger{}
		}
		progress.send = send
		response, err = handleUpgradeNodeK8s(instruction, progress)

	case RestartServiceRequestType:
		response, err = HandleRestartService(instruction)

	default:
		err = fmt.Errorf("unknown instruction type: %s", instruction.Type)
	}

	return finalizeInstructionResponse(instruction, response, err)
}

// finalizeInstructionResponse applies the common reply contract after a handler returns.
func finalizeInstructionResponse(instruction *l2sec.ToNodeAgent, response *l2sec.FromNodeAgent, err error) *l2sec.FromNodeAgent {
	// Handle any errors from instruction processing
	if err != nil {
		roslog.E("Error processing instruction", err, "type", instruction.Type, "tag", instruction.Tag)
		return &l2sec.FromNodeAgent{
			JsonB64: err.Error(),
			Type:    "ERROR",
			Tag:     instruction.Tag, // Preserve the tag for correlation
		}
	}

	// Handlers may return (nil, nil) to signal "suppress ack" (see VIP_ASSIGN).
	if response == nil {
		return nil
	}

	// A handler can return a shared immutable template. Clone the response before
	// request-specific mutation so concurrent workers cannot change another reply.
	response = proto.Clone(response).(*l2sec.FromNodeAgent)
	response.Tag = instruction.Tag

	roslog.I("Response from agent stream", "type", response.Type, "tag", response.Tag)

	return response
}
