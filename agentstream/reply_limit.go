package agentstream

import (
	"errors"
	"fmt"

	"github.com/runos-official/nodeagent/l2sec"
	"github.com/runos-official/nodeagent/roslog"
	"google.golang.org/protobuf/proto"
)

// NodewardMaxMessageBytes is the largest message Nodeward, and the hub in front of
// it, accept on the L2 agent stream. A larger message is refused with
// ResourceExhausted and ends the whole stream, so every in-flight call on it fails.
// This mirrors the server's limit; change it only together with that limit.
const NodewardMaxMessageBytes = 16 * 1024 * 1024

// replyHeadroomBytes is the room kept below the Nodeward limit for anything added
// on the way to the server.
const replyHeadroomBytes = 64 * 1024

// MaxReplyBytes is the largest message (the whole protobuf message, tag and type
// included) this agent puts on the stream. A reply above it is replaced by a
// REPLY_TOO_LARGE error, so one big reply fails one request and not the stream.
const MaxReplyBytes = NodewardMaxMessageBytes - replyHeadroomBytes

// ReplyTooLargeCode starts the text of the ERROR reply sent in place of an
// oversized reply. The text is
//
//	REPLY_TOO_LARGE size=<bytes> limit=<bytes> type=<reply type>: <advice>
//
// It travels as an ordinary ERROR reply, so Nodeward and conductor versions that
// predate this limit already carry it to the caller.
const ReplyTooLargeCode = "REPLY_TOO_LARGE"

// errMessageTooLarge is returned by a send that was refused for its size, before
// anything was written to the stream.
var errMessageTooLarge = errors.New("message is larger than the stream allows")

// boundReply returns response when it fits on the stream. Otherwise it returns a
// small ERROR reply with the same tag, which names the size and the limit.
func boundReply(response *l2sec.FromNodeAgent) *l2sec.FromNodeAgent {
	if response == nil {
		return nil
	}
	size := proto.Size(response)
	if size <= MaxReplyBytes {
		return response
	}
	roslog.E("Reply is larger than the stream allows; sending an error instead", errMessageTooLarge,
		"type", response.Type, "tag", response.Tag, "bytes", size, "limit", MaxReplyBytes)
	return &l2sec.FromNodeAgent{
		Type: "ERROR",
		Tag:  response.Tag,
		JsonB64: fmt.Sprintf("%s size=%d limit=%d type=%s: the reply is too large for the node connection; ask for less output (a namespace, a label, a limit or a tail)",
			ReplyTooLargeCode, size, MaxReplyBytes, response.Type),
	}
}

// checkSendSize refuses a message the stream would reject. Sending it would end
// the stream, so the caller gets an error and the stream stays up.
func checkSendSize(msg *l2sec.FromNodeAgent) error {
	if size := proto.Size(msg); size > MaxReplyBytes {
		return fmt.Errorf("%w: %d bytes, limit %d (type %s)", errMessageTooLarge, size, MaxReplyBytes, msg.Type)
	}
	return nil
}
