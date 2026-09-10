package agentstream

import (
	pb "github.com/runos-official/nodeagent/l2sec"
)

// NoContentResponse is the immutable acknowledgement template for handlers that
// perform an action but have no payload to return. Its JsonB64 is base64("{}").
// The dispatcher clones the template before it adds request-specific fields.
var NoContentResponse = &pb.FromNodeAgent{
	JsonB64: "e30=",
	Type:    "NO_CONTENT",
}
