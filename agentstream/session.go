package agentstream

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"sync"

	"github.com/runos-official/nodeagent/l2sec"
	"github.com/runos-official/nodeagent/roslog"
)

// Interactive sessions on the agent (goal 31, decisions 1 and 2 of `the-starvation-answer`).
//
// A SESSION IS NOT AN INSTRUCTION, and that is the whole design. An instruction is
// request-and-reply and returns; a session is open for as long as somebody is typing. Putting a
// session in the worker pool would hold one of five workers for the life of a terminal, so five
// open terminals would leave a node unable to apply a manifest, run a script or report its status.
// No value of numWorkers fixes that: any fixed pool is exhausted by enough sessions, and a pool
// large enough not to be has stopped bounding anything.
//
// So session frames are dispatched ON THE RECEIVER GOROUTINE, beside HandleInstructionAsResponse
// and before the pool, and they never touch instructionChan. The pool cannot see them, so a node
// with sixteen terminals open owes the control plane exactly what it owed with none.
//
// THE L2SEC CONTRACT IS UNCHANGED, deliberately. Session frames ride the ToNodeAgent message the
// agent already has: `type` names the frame, `tag` CARRIES THE SESSION ID, and `jsonB64` carries
// the payload. The l2sec proto is hand-synced between nodeward and this repo with no gate over it,
// unlike conductor's l3sec copy, so a change there is the coordination hazard this goal names as
// its worst. Reusing the shape costs one base64 expansion on terminal output and buys no drift.

const (
	// Frame types. They mirror the l3sec StreamFrame bodies one for one, so nodeward translates
	// rather than interprets.
	OpenStreamRequestType   = "OPEN_STREAM"
	StreamDataRequestType   = "STREAM_DATA"
	StreamResizeRequestType = "STREAM_RESIZE"
	StreamCloseRequestType  = "STREAM_CLOSE"

	// Sent back to nodeward. A close carries a reason a person can read.
	StreamOpenedResponseType = "STREAM_OPENED"
	StreamDataResponseType   = "STREAM_DATA"
	StreamClosedResponseType = "STREAM_CLOSED"
)

// MaxConcurrentSessions is the per-node ceiling, separate from numWorkers and much larger.
//
// REACHING IT IS REFUSED, NEVER QUEUED. A queued terminal is a terminal that appears to hang, with
// nothing to look at and nothing to do; a refusal that names the limit is something a person can
// act on. Sixteen is far above any real use of one node and far below anything that threatens it.
const MaxConcurrentSessions = 16

// StreamOpen is the OPEN_STREAM payload, the l3sec StreamOpen as the agent needs it. The fields
// nodeward resolves for itself (aid, cid, nid) are not repeated here: by this point the frame is
// already on the right node.
type StreamOpen struct {
	Target    string            `json:"target"`
	Namespace string            `json:"namespace"`
	Name      string            `json:"name"`
	Port      uint32            `json:"port"`
	Path      string            `json:"path"`
	Headers   map[string]string `json:"headers"`
	Cols      uint32            `json:"cols"`
	Rows      uint32            `json:"rows"`
}

// StreamResize is the STREAM_RESIZE payload.
type StreamResize struct {
	Cols uint32 `json:"cols"`
	Rows uint32 `json:"rows"`
}

// StreamClosed is the STREAM_CLOSED payload sent back to nodeward.
type StreamClosed struct {
	Reason string `json:"reason"`
	Error  bool   `json:"error"`
}

// FarEnd is one open connection to whatever the session is a terminal onto.
//
// An interface because the two consumers differ ONLY here: a plain websocket to a Service for the
// web terminal, the KubeVirt subresources for a VM console. Everything above this line, which is
// the part that can starve a node, is the same for both and is written once.
type FarEnd interface {
	// Write sends bytes the operator typed towards the far end.
	Write(p []byte) (int, error)
	// Resize tells the far end the terminal's new size. A far end with no notion of size returns
	// nil rather than an error: a resize that cannot apply is not a session failure.
	Resize(cols, rows uint32) error
	// Close hangs up. Called exactly once per session.
	Close() error
}

// Dialer opens the far end named by an OPEN_STREAM.
type Dialer func(sessionID string, open StreamOpen, onData func([]byte), onClose func(reason string, isError bool)) (FarEnd, error)

// dialer is the installed dialer. Nil means this build has none, which is the honest state until
// a consumer lands: OPEN_STREAM is then REFUSED with that reason rather than accepted and left
// silent, because a terminal that opens and never prints is the harder failure to diagnose.
var (
	dialerMu sync.RWMutex
	dialer   Dialer
)

// SetDialer installs the far-end dialer. Called once at startup by whatever consumer is built in.
func SetDialer(d Dialer) {
	dialerMu.Lock()
	defer dialerMu.Unlock()
	dialer = d
}

func currentDialer() Dialer {
	dialerMu.RLock()
	defer dialerMu.RUnlock()
	return dialer
}

type session struct {
	id     string
	far    FarEnd
	closed bool
	// Guards closed and serialises Close against a concurrent frame, so a far end is hung up once
	// however the two sides race.
	mu sync.Mutex
}

var (
	sessionsMu sync.Mutex
	sessions   = map[string]*session{}
)

// SessionCount is the number of open sessions. For tests and for the node status read.
func SessionCount() int {
	sessionsMu.Lock()
	defer sessionsMu.Unlock()
	return len(sessions)
}

// IsSessionFrame says whether the receiver should handle this itself rather than queue it.
//
// Checked on the receiver goroutine, so it must stay a string comparison and nothing more.
func IsSessionFrame(instruction *l2sec.ToNodeAgent) bool {
	if instruction == nil {
		return false
	}
	switch instruction.Type {
	case OpenStreamRequestType, StreamDataRequestType, StreamResizeRequestType, StreamCloseRequestType:
		return true
	}
	return false
}

// HandleSessionFrame processes one session frame. It RETURNS IMMEDIATELY in every case: the only
// work it does inline is registry bookkeeping and a write to an already-open far end.
//
// It is called on the receiver goroutine, so nothing here may block on the network or on a lock
// another network call holds. A far-end WRITE is the one exception and it is deliberate: that is
// what the operator typed, it is bounded by the far end's own buffer, and making the receiver wait
// on it is the backpressure that stops an unbounded queue forming (decision 4). Keystrokes are
// bytes per second, not megabytes.
func HandleSessionFrame(instruction *l2sec.ToNodeAgent) {
	sessionID := instruction.Tag
	if sessionID == "" {
		roslog.E("Session frame with no session id", fmt.Errorf("tag is empty"), "type", instruction.Type)
		return
	}

	switch instruction.Type {
	case OpenStreamRequestType:
		handleOpenStream(sessionID, instruction.JsonB64)
	case StreamDataRequestType:
		handleStreamData(sessionID, instruction.JsonB64)
	case StreamResizeRequestType:
		handleStreamResize(sessionID, instruction.JsonB64)
	case StreamCloseRequestType:
		closeSession(sessionID, "the far side hung up", false)
	}
}

func handleOpenStream(sessionID string, payloadB64 string) {
	raw, err := base64.StdEncoding.DecodeString(payloadB64)
	if err != nil {
		refuseOpen(sessionID, fmt.Sprintf("the open frame was not valid base64: %v", err))
		return
	}
	var open StreamOpen
	if err := json.Unmarshal(raw, &open); err != nil {
		refuseOpen(sessionID, fmt.Sprintf("the open frame was not valid JSON: %v", err))
		return
	}

	d := currentDialer()
	if d == nil {
		refuseOpen(sessionID, "this node agent has no dialer for interactive sessions installed, so it cannot open one. The node is running a build older than the one that serves terminals.")
		return
	}

	// THE BOUND IS TAKEN BEFORE THE DIAL, and a duplicate id is refused rather than replacing what
	// is there. Reserving first means two opens arriving together cannot both pass the check and
	// leave the node over its ceiling with a far end nobody will ever close.
	sessionsMu.Lock()
	if _, exists := sessions[sessionID]; exists {
		sessionsMu.Unlock()
		refuseOpen(sessionID, "a session with this id is already open on this node")
		return
	}
	if len(sessions) >= MaxConcurrentSessions {
		open := len(sessions)
		sessionsMu.Unlock()
		refuseOpen(sessionID, fmt.Sprintf("this node already has %d interactive sessions open, which is its limit of %d. Close one and try again; RunOS refuses rather than queueing, because a queued terminal looks like a terminal that has hung.", open, MaxConcurrentSessions))
		return
	}
	s := &session{id: sessionID}
	sessions[sessionID] = s
	sessionsMu.Unlock()

	// THE CALLBACKS BIND TO THIS SESSION, NOT TO ITS ID. A far end outlives the moment it is
	// hung up: its reader goroutine is still in flight and will call back once, afterwards. If the
	// id has been reused by then, an id-bound callback closes somebody else's session and reports
	// their terminal as ended. Conductor issues unique ids, so this is a guard rather than a
	// live defect, but it costs one pointer comparison and removes the question.
	far, err := d(sessionID, open,
		func(b []byte) { sendSessionDataFrom(s, b) },
		func(reason string, isError bool) { closeSessionInstance(s, reason, isError) },
	)
	if err != nil {
		// The reservation must come back, or a node loses a slot every time a dial fails and
		// eventually refuses every terminal while holding none.
		dropSession(sessionID)
		refuseOpen(sessionID, fmt.Sprintf("could not open the far end: %v", err))
		return
	}

	s.mu.Lock()
	alreadyClosed := s.closed
	s.far = far
	s.mu.Unlock()

	// A close that arrived while the dial was in flight has nothing to hang up yet, so it is
	// honoured here instead. Without this the far end leaks for the life of the process.
	if alreadyClosed {
		_ = far.Close()
		return
	}

	roslog.I("Interactive session opened", "session", sessionID, "target", open.Target, "open_sessions", SessionCount())
	if err := SendToNodeward(&l2sec.FromNodeAgent{Type: StreamOpenedResponseType, Tag: sessionID}); err != nil {
		roslog.E("Could not confirm the session opened", err, "session", sessionID)
	}
}

func handleStreamData(sessionID string, payloadB64 string) {
	s := lookupSession(sessionID)
	if s == nil {
		// A frame for an id this node does not hold is DROPPED rather than guessed at. It is the
		// tail of a session that has already closed, and inventing one to receive it would let the
		// far side reopen a session the node believes it hung up.
		return
	}
	raw, err := base64.StdEncoding.DecodeString(payloadB64)
	if err != nil {
		closeSession(sessionID, fmt.Sprintf("a data frame was not valid base64: %v", err), true)
		return
	}

	s.mu.Lock()
	far, closed := s.far, s.closed
	s.mu.Unlock()
	if closed || far == nil {
		return
	}
	if _, err := far.Write(raw); err != nil {
		closeSession(sessionID, fmt.Sprintf("the far end stopped accepting input: %v", err), true)
	}
}

func handleStreamResize(sessionID string, payloadB64 string) {
	s := lookupSession(sessionID)
	if s == nil {
		return
	}
	raw, err := base64.StdEncoding.DecodeString(payloadB64)
	if err != nil {
		return
	}
	var size StreamResize
	if err := json.Unmarshal(raw, &size); err != nil {
		return
	}
	s.mu.Lock()
	far, closed := s.far, s.closed
	s.mu.Unlock()
	if closed || far == nil {
		return
	}
	// A resize that fails is NOT a session failure. Not every far end has a notion of size, and
	// killing a working terminal because it could not be told its width would be absurd.
	if err := far.Resize(size.Cols, size.Rows); err != nil {
		roslog.I("Far end would not resize", "session", sessionID, "error", err.Error())
	}
}

// MaxBulkFrameBytes caps one frame of session output, and it is a LATENCY bound, not a memory one.
//
// G31-F1, MEASURED 2026-08-20 against the real session machinery over a modelled 8 MB/s link:
//
//	output frame   control p50, idle   control p50, 1 session   control p50, 16 sessions
//	16 KiB         0.16 ms             0.19 ms                  1.80 ms
//	512 KiB        0.16 ms             70.3 ms                  70.4 ms
//
// The egress priority in egress.go stops control traffic QUEUEING behind session data: a bulk
// sender yields while control is waiting. What it cannot do is recall a bulk send already in
// flight, because the mutex is held for the whole of it. So control latency is bounded by ONE
// output frame's transmit time, and until this cap existed that bound was whatever the far end
// happened to hand over. ONE terminal was enough to hold the control plane for 70 ms.
//
// Capping the FRAME rather than making the send path clever keeps decision 3's shape: the mutex
// stays where it is, control still never queues behind bulk, and the residual wait is now one
// small frame. 16 KiB is a typical PTY read, so ordinary terminal output is unaffected and only a
// coalesced burst is split.
const MaxBulkFrameBytes = 16 * 1024

// sendSessionData carries far-end output back to nodeward on the BULK path, so a terminal printing
// as fast as it can gives way to anything the node owes the control plane (decision 3).
//
// The dialer calls this from its own reader goroutine, and SendBulkToNodeward is synchronous, so a
// far end producing faster than the stream drains is made to wait. That IS the backpressure: the
// reader stops reading and the far end's own buffer fills, which is what a slow terminal should
// do. Nothing is ever dropped, because a console that silently loses bytes is worse than one that
// stutters.
//
// SPLIT AT MaxBulkFrameBytes, in order, so a burst cannot hold the send path for long. Splitting is
// safe on a byte stream: the far side reassembles by concatenation and a terminal has no frame
// boundaries to preserve.
func sendSessionData(sessionID string, data []byte) {
	for len(data) > 0 {
		n := len(data)
		if n > MaxBulkFrameBytes {
			n = MaxBulkFrameBytes
		}
		err := SendBulkToNodeward(&l2sec.FromNodeAgent{
			Type:    StreamDataResponseType,
			Tag:     sessionID,
			JsonB64: base64.StdEncoding.EncodeToString(data[:n]),
		})
		if err != nil {
			closeSession(sessionID, fmt.Sprintf("the session's output could not be sent: %v", err), true)
			return
		}
		data = data[n:]
	}
}

// stillRegistered says whether this exact session is the one the node currently holds under its
// id. A far end that was hung up moments ago fails this, and so does one whose id was reissued.
func stillRegistered(s *session) bool {
	sessionsMu.Lock()
	defer sessionsMu.Unlock()
	return sessions[s.id] == s
}

func sendSessionDataFrom(s *session, data []byte) {
	if !stillRegistered(s) {
		return
	}
	sendSessionData(s.id, data)
}

func closeSessionInstance(s *session, reason string, isError bool) {
	if !stillRegistered(s) {
		return
	}
	closeSession(s.id, reason, isError)
}

func lookupSession(sessionID string) *session {
	sessionsMu.Lock()
	defer sessionsMu.Unlock()
	return sessions[sessionID]
}

func dropSession(sessionID string) {
	sessionsMu.Lock()
	defer sessionsMu.Unlock()
	delete(sessions, sessionID)
}

// closeSession hangs up once, however many ways the session ends at the same moment.
//
// Both sides can end a session simultaneously: the operator closes the tab while the far end
// exits. Closing twice would double-close a socket and, worse, send two STREAM_CLOSED frames for
// one session, so the seen-it-already check and the removal happen under the same lock.
func closeSession(sessionID string, reason string, isError bool) {
	s := lookupSession(sessionID)
	if s == nil {
		return
	}

	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	far := s.far
	s.mu.Unlock()

	dropSession(sessionID)

	if far != nil {
		if err := far.Close(); err != nil {
			roslog.I("Far end reported an error on close", "session", sessionID, "error", err.Error())
		}
	}

	roslog.I("Interactive session closed", "session", sessionID, "reason", reason, "error", isError, "open_sessions", SessionCount())
	notifyClosed(sessionID, reason, isError)
}

// refuseOpen answers an open that never became a session. It is a CLOSE rather than a silence: an
// operator whose terminal simply never appears has nothing to read, and the reason is the whole
// value of refusing rather than queueing.
func refuseOpen(sessionID string, reason string) {
	roslog.I("Refused an interactive session", "session", sessionID, "reason", reason)
	notifyClosed(sessionID, reason, true)
}

func notifyClosed(sessionID string, reason string, isError bool) {
	payload, err := json.Marshal(StreamClosed{Reason: reason, Error: isError})
	if err != nil {
		// Cannot happen for this struct, and a session that cannot say why it ended must still say
		// that it ended, or the far side waits for ever.
		payload = []byte(`{"reason":"the session ended and its reason could not be encoded","error":true}`)
	}
	msg := &l2sec.FromNodeAgent{
		Type:    StreamClosedResponseType,
		Tag:     sessionID,
		JsonB64: base64.StdEncoding.EncodeToString(payload),
	}
	if err := SendToNodeward(msg); err != nil {
		roslog.E("Could not report a closed session", err, "session", sessionID)
	}
}

// CloseAllSessions hangs up every session, for stream teardown. A session is only meaningful while
// the stream that carries it is up, so leaving far ends open across a reconnect would strand a
// shell on the node with nobody reading it.
func CloseAllSessions(reason string) {
	sessionsMu.Lock()
	ids := make([]string, 0, len(sessions))
	for id := range sessions {
		ids = append(ids, id)
	}
	sessionsMu.Unlock()

	for _, id := range ids {
		closeSession(id, reason, true)
	}
}
