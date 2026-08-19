package agentstream

import (
	"sync"
	"sync/atomic"
	"time"
)

// Egress priority for interactive sessions (goal 31, the-starvation-answer).
//
// EVERY message a node sends takes one mutex, so a terminal producing output as fast as it can
// competes with every status reply that node owes the control plane. RUN_WEB_REQUEST caps a body
// at 32 MiB; a console has no equivalent bound, because it is not a body.
//
// TWO CLASSES, AND DELIBERATELY NOT MORE. Control is everything the agent sends today: instruction
// responses, status, heartbeats. Bulk is session data. Control is never made to wait behind bulk.
// Control traffic is tiny and infrequent, so strict priority cannot starve bulk in practice, and
// it is far easier to reason about than a weighted scheme.
//
// WHY THIS SHAPE RATHER THAN A QUEUE. A queue in front of the send would make SendToNodeward
// fire-and-forget, and every existing caller relies on getting the send error back synchronously.
// So control keeps exactly today's behaviour, byte for byte, and only BULK yields. A bulk sender
// that waits is correct backpressure: it is the session goroutine, and making it wait is precisely
// what should happen when the node has real work to report.
var controlWaiting atomic.Int64

// bulkGate serialises bulk senders against each other so one session cannot monopolise the yield
// check and starve another.
var bulkGate sync.Mutex

// How long a bulk sender yields for before re-checking. Short enough that a terminal stays
// responsive when the node is quiet, long enough that a burst of control traffic drains.
const bulkYield = 2 * time.Millisecond

// How long a bulk sender will yield in total before sending anyway. Without a ceiling, a node
// under sustained control load would stall a terminal indefinitely, which is the opposite failure
// and just as bad: a console that never prints is a console that is broken.
const bulkMaxYield = 250 * time.Millisecond

// waitForControlIdle yields while any control sender is waiting for or holding the stream, up to
// bulkMaxYield. Returns true when it gave way, which the caller may log.
func waitForControlIdle() bool {
	if controlWaiting.Load() == 0 {
		return false
	}
	deadline := time.Now().Add(bulkMaxYield)
	yielded := false
	for controlWaiting.Load() > 0 && time.Now().Before(deadline) {
		yielded = true
		time.Sleep(bulkYield)
	}
	return yielded
}
