package agentstream

import (
	"encoding/base64"
	"encoding/json"
	"strconv"
	"sync"

	"github.com/runos-official/nodeagent/l2sec"
)

// PROGRESS TAGS (FPL315 phase 8).
//
// A long instruction can stream progress or log messages before its reply.
// Today each one goes out with a fresh random tag, so nothing links it to the
// instruction. When the instruction payload has "progressTags": true, every
// progress message carries the tag "<instruction tag>/<n>" instead, n a
// decimal counter from 1. Nodehub routes such a tag to the replica that sent
// the instruction without knowing the message type.
//
// Nodeward sets the field only when it runs behind nodehub. A nodeward on the
// direct path never sets it, so it never sees a tag it would read as a reply.
// Without the field the agent behaves exactly as before.
//
// Any streaming instruction can use this: build one ProgressTagger per
// instruction and send each progress message through its Send.

// ProgressTagger numbers the progress messages of one instruction. A nil
// *ProgressTagger is valid and sends as before, with a fresh random tag.
type ProgressTagger struct {
	mu   sync.Mutex // held across the send, so wire order matches n
	base string
	n    uint64
	send func(*l2sec.FromNodeAgent) error
}

// NewProgressTagger returns a tagger for instruction, or nil when its payload
// does not set "progressTags": true or the instruction has no tag.
func NewProgressTagger(instruction *l2sec.ToNodeAgent) *ProgressTagger {
	if instruction == nil || instruction.Tag == "" || instruction.JsonB64 == "" {
		return nil
	}
	raw, err := base64.StdEncoding.DecodeString(instruction.JsonB64)
	if err != nil {
		return nil
	}
	var ask struct {
		ProgressTags *bool `json:"progressTags"`
	}
	// A payload of another shape, or a field that is not a bool, means "not asked".
	if err := json.Unmarshal(raw, &ask); err != nil || ask.ProgressTags == nil || !*ask.ProgressTags {
		return nil
	}
	return &ProgressTagger{base: instruction.Tag}
}

// Send sends one progress message. With a tagger it sets the next progress
// tag; without one it leaves the tag empty so SendToNodeward picks a random
// one, as today.
func (p *ProgressTagger) Send(msg *l2sec.FromNodeAgent) error {
	if p == nil {
		return SendToNodeward(msg)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.base != "" {
		p.n++
		msg.Tag = p.base + "/" + strconv.FormatUint(p.n, 10)
	}
	if p.send != nil {
		return p.send(msg)
	}
	return SendToNodeward(msg)
}
