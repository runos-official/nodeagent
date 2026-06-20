package agentstream

import (
	pb "github.com/runos-official/nodeagent/l2sec"
)

// NoContentResponse is the standard acknowledgement returned by handlers that
// perform an action but have no payload to return. Its JsonB64 is base64("{}").
var NoContentResponse = &pb.FromNodeAgent{
	JsonB64: "e30=",
	Type:    "NO_CONTENT",
}
