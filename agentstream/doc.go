// Package agentstream runs the bidirectional gRPC stream to the Nodeward control
// plane: it receives instructions, dispatches them to a bounded worker pool of
// per-type handlers (the in_*.go files), and sends UUID-correlated responses.
package agentstream
