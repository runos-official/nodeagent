// Package backend is the node agent's client to the Nodeward control plane. It
// establishes the L1Sec (registration) and L2Sec (mTLS) gRPC connections with
// retry, and provides helpers for status updates, node logs, and discovery.
package backend
