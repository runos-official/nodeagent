package agentstream

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/runos-official/nodeagent/l2sec"
)

// Goal 31, decisions 1 and 2. A session is not an instruction: it is dispatched on the receiver,
// never enters the worker pool, carries its own bound, and is REFUSED rather than queued when that
// bound is reached.
//
// These drive the registry directly. The send path is captured rather than mocked at the network,
// so what nodeward would actually receive is what is asserted.

// captureSends redirects the stream to a recorder for the duration of one test.
type capture struct {
	mu   sync.Mutex
	sent []*l2sec.FromNodeAgent
}

func (c *capture) Send(m *l2sec.FromNodeAgent) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sent = append(c.sent, m)
	return nil
}

func (c *capture) byType(t string) []*l2sec.FromNodeAgent {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []*l2sec.FromNodeAgent
	for _, m := range c.sent {
		if m.Type == t {
			out = append(out, m)
		}
	}
	return out
}

// fakeStream is the minimum of the generated client interface that the send path touches.
type fakeStream struct {
	l2sec.Nodeward_NodeAgentStreamClient
	c *capture
}

func (f *fakeStream) Send(m *l2sec.FromNodeAgent) error { return f.c.Send(m) }

// newTestBed installs a capturing stream and clears the registry, and returns the capture plus a
// restore function. Package state is global here because the agent has exactly one stream.
func newTestBed(t *testing.T) *capture {
	t.Helper()
	c := &capture{}

	streamMutex.Lock()
	prev := globalStream
	globalStream = &fakeStream{c: c}
	streamMutex.Unlock()

	sessionsMu.Lock()
	sessions = map[string]*session{}
	sessionsMu.Unlock()

	prevDialer := currentDialer()

	t.Cleanup(func() {
		streamMutex.Lock()
		globalStream = prev
		streamMutex.Unlock()
		SetDialer(prevDialer)
		sessionsMu.Lock()
		sessions = map[string]*session{}
		sessionsMu.Unlock()
	})
	return c
}

// fakeFarEnd records what the session wrote towards it.
type fakeFarEnd struct {
	mu        sync.Mutex
	written   []byte
	cols      uint32
	rows      uint32
	closes    int
	writeErr  error
	resizeErr error
}

func (f *fakeFarEnd) Write(p []byte) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.writeErr != nil {
		return 0, f.writeErr
	}
	f.written = append(f.written, p...)
	return len(p), nil
}

func (f *fakeFarEnd) Resize(cols, rows uint32) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.resizeErr != nil {
		return f.resizeErr
	}
	f.cols, f.rows = cols, rows
	return nil
}

func (f *fakeFarEnd) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closes++
	return nil
}

func openFrame(id string, open StreamOpen) *l2sec.ToNodeAgent {
	b, _ := json.Marshal(open)
	return &l2sec.ToNodeAgent{
		Type:    OpenStreamRequestType,
		Tag:     id,
		JsonB64: base64.StdEncoding.EncodeToString(b),
	}
}

func dataFrame(id string, data []byte) *l2sec.ToNodeAgent {
	return &l2sec.ToNodeAgent{
		Type:    StreamDataRequestType,
		Tag:     id,
		JsonB64: base64.StdEncoding.EncodeToString(data),
	}
}

func closedReason(t *testing.T, m *l2sec.FromNodeAgent) StreamClosed {
	t.Helper()
	raw, err := base64.StdEncoding.DecodeString(m.JsonB64)
	if err != nil {
		t.Fatalf("close payload was not base64: %v", err)
	}
	var c StreamClosed
	if err := json.Unmarshal(raw, &c); err != nil {
		t.Fatalf("close payload was not JSON: %v", err)
	}
	return c
}

func TestSessionFrameIsRecognisedAndNothingElseIs(t *testing.T) {
	// The receiver keys on this to decide what never reaches the worker pool. An instruction
	// wrongly classified as a session would silently stop being executed at all.
	for _, ty := range []string{OpenStreamRequestType, StreamDataRequestType, StreamResizeRequestType, StreamCloseRequestType} {
		if !IsSessionFrame(&l2sec.ToNodeAgent{Type: ty}) {
			t.Fatalf("%s should be a session frame", ty)
		}
	}
	for _, ty := range []string{GetNodeStatusRequestType, ApplyCRRequestType, RunRemoteScriptRequestType, RunWebRequestType, ""} {
		if IsSessionFrame(&l2sec.ToNodeAgent{Type: ty}) {
			t.Fatalf("%q must NOT be a session frame: it would stop being executed", ty)
		}
	}
	if IsSessionFrame(nil) {
		t.Fatal("a nil instruction must not be a session frame")
	}
}

func TestOpenIsRefusedWhenNoDialerIsInstalled(t *testing.T) {
	// The honest state of a build with no consumer. Accepting the open and staying silent would
	// leave an operator with a terminal that never prints, which is the harder failure to diagnose.
	c := newTestBed(t)
	SetDialer(nil)

	HandleSessionFrame(openFrame("s1", StreamOpen{Target: "SERVICE_WEBSOCKET"}))

	closes := c.byType(StreamClosedResponseType)
	if len(closes) != 1 {
		t.Fatalf("expected one STREAM_CLOSED, got %d", len(closes))
	}
	got := closedReason(t, closes[0])
	if !got.Error {
		t.Fatal("a refusal must be marked as an error")
	}
	if got.Reason == "" {
		t.Fatal("a refusal with no reason is the thing this design exists to avoid")
	}
	if SessionCount() != 0 {
		t.Fatalf("a refused open must leave no session, got %d", SessionCount())
	}
}

func TestOpenDataResizeAndClose(t *testing.T) {
	c := newTestBed(t)
	far := &fakeFarEnd{}
	SetDialer(func(id string, open StreamOpen, onData func([]byte), onClose func(string, bool)) (FarEnd, error) {
		return far, nil
	})

	HandleSessionFrame(openFrame("s1", StreamOpen{Target: "SERVICE_WEBSOCKET", Cols: 80, Rows: 24}))
	if SessionCount() != 1 {
		t.Fatalf("expected one open session, got %d", SessionCount())
	}
	if len(c.byType(StreamOpenedResponseType)) != 1 {
		t.Fatal("an opened session must be confirmed, or the far side waits for ever")
	}

	HandleSessionFrame(dataFrame("s1", []byte("ls -l\n")))
	far.mu.Lock()
	written := string(far.written)
	far.mu.Unlock()
	if written != "ls -l\n" {
		t.Fatalf("far end should have received the keystrokes, got %q", written)
	}

	size, _ := json.Marshal(StreamResize{Cols: 120, Rows: 40})
	HandleSessionFrame(&l2sec.ToNodeAgent{Type: StreamResizeRequestType, Tag: "s1", JsonB64: base64.StdEncoding.EncodeToString(size)})
	far.mu.Lock()
	cols, rows := far.cols, far.rows
	far.mu.Unlock()
	if cols != 120 || rows != 40 {
		t.Fatalf("resize did not reach the far end: %dx%d", cols, rows)
	}

	HandleSessionFrame(&l2sec.ToNodeAgent{Type: StreamCloseRequestType, Tag: "s1"})
	if SessionCount() != 0 {
		t.Fatalf("a closed session must be removed, got %d", SessionCount())
	}
	far.mu.Lock()
	closes := far.closes
	far.mu.Unlock()
	if closes != 1 {
		t.Fatalf("far end should be hung up exactly once, got %d", closes)
	}
}

func TestSessionsAreBoundedAndRefusedNotQueued(t *testing.T) {
	// Decision 2. The refusal must NAME the limit: a queued terminal looks like a hung terminal,
	// and the reason is the whole value of refusing instead.
	c := newTestBed(t)
	SetDialer(func(id string, open StreamOpen, onData func([]byte), onClose func(string, bool)) (FarEnd, error) {
		return &fakeFarEnd{}, nil
	})

	for i := 0; i < MaxConcurrentSessions; i++ {
		HandleSessionFrame(openFrame(fmt.Sprintf("s%d", i), StreamOpen{}))
	}
	if SessionCount() != MaxConcurrentSessions {
		t.Fatalf("expected %d sessions, got %d", MaxConcurrentSessions, SessionCount())
	}

	HandleSessionFrame(openFrame("one-too-many", StreamOpen{}))

	if SessionCount() != MaxConcurrentSessions {
		t.Fatalf("the bound was exceeded: %d open", SessionCount())
	}
	closes := c.byType(StreamClosedResponseType)
	if len(closes) != 1 {
		t.Fatalf("the refused open must be answered exactly once, got %d", len(closes))
	}
	if closes[0].Tag != "one-too-many" {
		t.Fatalf("the refusal must be tagged with the session that was refused, got %q", closes[0].Tag)
	}
	reason := closedReason(t, closes[0]).Reason
	if !contains(reason, fmt.Sprint(MaxConcurrentSessions)) {
		t.Fatalf("the refusal must name the limit so a person can act on it, got %q", reason)
	}
}

func TestADialFailureReturnsTheSlot(t *testing.T) {
	// Without this a node loses a slot on every failed dial and eventually refuses every terminal
	// while holding none, which reads as a bound that works and is a leak.
	c := newTestBed(t)
	SetDialer(func(id string, open StreamOpen, onData func([]byte), onClose func(string, bool)) (FarEnd, error) {
		return nil, fmt.Errorf("connection refused")
	})

	for i := 0; i < MaxConcurrentSessions+4; i++ {
		HandleSessionFrame(openFrame(fmt.Sprintf("s%d", i), StreamOpen{}))
	}
	if SessionCount() != 0 {
		t.Fatalf("no session should survive a failed dial, got %d", SessionCount())
	}
	if got := len(c.byType(StreamClosedResponseType)); got != MaxConcurrentSessions+4 {
		t.Fatalf("every failed open must be answered, got %d", got)
	}
}

func TestADuplicateSessionIdIsRefusedNotReplaced(t *testing.T) {
	// Replacing would orphan the first far end: nothing would ever close it, and its output would
	// arrive tagged as the second session's.
	c := newTestBed(t)
	first := &fakeFarEnd{}
	second := &fakeFarEnd{}
	n := 0
	SetDialer(func(id string, open StreamOpen, onData func([]byte), onClose func(string, bool)) (FarEnd, error) {
		n++
		if n == 1 {
			return first, nil
		}
		return second, nil
	})

	HandleSessionFrame(openFrame("dup", StreamOpen{}))
	HandleSessionFrame(openFrame("dup", StreamOpen{}))

	if n != 1 {
		t.Fatalf("the second open must not dial at all, dialled %d times", n)
	}
	if SessionCount() != 1 {
		t.Fatalf("expected one session, got %d", SessionCount())
	}
	if got := len(c.byType(StreamClosedResponseType)); got != 1 {
		t.Fatalf("expected exactly one refusal, got %d", got)
	}
}

func TestClosingTwiceHangsUpOnceAndReportsOnce(t *testing.T) {
	// Both sides can end a session at the same moment: the operator closes the tab as the shell
	// exits. Two STREAM_CLOSED frames for one session would be a protocol error at the far side.
	c := newTestBed(t)
	far := &fakeFarEnd{}
	var closeFromFarEnd func(string, bool)
	SetDialer(func(id string, open StreamOpen, onData func([]byte), onClose func(string, bool)) (FarEnd, error) {
		closeFromFarEnd = onClose
		return far, nil
	})

	HandleSessionFrame(openFrame("s1", StreamOpen{}))

	// THE INTERLEAVING IS FORCED, not raced for. Sequentially the second close finds nothing in
	// the registry and returns, so a sequential test passes with the guard deleted; and simply
	// starting two goroutines together did not reliably hit the window either. Both were measured
	// 2026-08-20 by deleting the guard and watching the test stay green.
	//
	// The window the guard protects is between one closer reading the session out of the registry
	// and it removing it. Holding the session's own lock puts BOTH closers inside that window and
	// keeps them there until this test lets go.
	held := lookupSession("s1")
	if held == nil {
		t.Fatal("the session should be open at this point")
	}
	held.mu.Lock()

	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); closeFromFarEnd("the shell exited", false) }()
	go func() {
		defer wg.Done()
		HandleSessionFrame(&l2sec.ToNodeAgent{Type: StreamCloseRequestType, Tag: "s1"})
	}()

	// Both are now past the registry read and blocked on the lock this test holds.
	time.Sleep(20 * time.Millisecond)
	held.mu.Unlock()
	wg.Wait()

	far.mu.Lock()
	closes := far.closes
	far.mu.Unlock()
	if closes != 1 {
		t.Fatalf("far end hung up %d times, want 1", closes)
	}
	if got := len(c.byType(StreamClosedResponseType)); got != 1 {
		t.Fatalf("reported closed %d times, want 1: two STREAM_CLOSED for one session is a protocol error at the far side", got)
	}
}

func TestFarEndOutputGoesOutOnTheBulkPath(t *testing.T) {
	// Decision 3. Terminal output must not compete with what the node owes the control plane.
	c := newTestBed(t)
	var emit func([]byte)
	SetDialer(func(id string, open StreamOpen, onData func([]byte), onClose func(string, bool)) (FarEnd, error) {
		emit = onData
		return &fakeFarEnd{}, nil
	})
	HandleSessionFrame(openFrame("s1", StreamOpen{}))

	emit([]byte("hello"))

	data := c.byType(StreamDataResponseType)
	if len(data) != 1 {
		t.Fatalf("expected one data frame, got %d", len(data))
	}
	if data[0].Tag != "s1" {
		t.Fatalf("output must carry its session id, got %q", data[0].Tag)
	}
	raw, err := base64.StdEncoding.DecodeString(data[0].JsonB64)
	if err != nil || string(raw) != "hello" {
		t.Fatalf("output was not carried intact: %q %v", string(raw), err)
	}

	// AND IT REALLY IS THE BULK PATH. Checking the frame's contents says nothing about which
	// function carried it: measured 2026-08-20 by swapping SendBulkToNodeward for SendToNodeward
	// and watching every assertion above stay green. What separates them is the behaviour that
	// matters, so that is what is asserted: bulk gives way while control is waiting, control does
	// not. controlWaiting is held above zero for the whole send, so a bulk sender yields to its
	// ceiling and a control sender returns at once.
	controlWaiting.Add(1)
	defer controlWaiting.Add(-1)

	began := time.Now()
	emit([]byte("world"))
	elapsed := time.Since(began)

	if elapsed < bulkMaxYield/2 {
		t.Fatalf("session output returned in %v while control traffic was waiting, so it did not "+
			"take the bulk path. A terminal on the control path competes with everything this "+
			"node owes RunOS.", elapsed)
	}
}

func TestAFrameForAnUnknownSessionIsDroppedNotInvented(t *testing.T) {
	// Inventing a session to receive a late frame would let the far side reopen one this node
	// believes it hung up.
	c := newTestBed(t)
	SetDialer(func(id string, open StreamOpen, onData func([]byte), onClose func(string, bool)) (FarEnd, error) {
		return &fakeFarEnd{}, nil
	})

	HandleSessionFrame(dataFrame("never-opened", []byte("x")))
	HandleSessionFrame(&l2sec.ToNodeAgent{Type: StreamCloseRequestType, Tag: "never-opened"})

	if SessionCount() != 0 {
		t.Fatalf("no session should exist, got %d", SessionCount())
	}
	if got := len(c.sent); got != 0 {
		t.Fatalf("an unknown session must produce no traffic at all, got %d frames", got)
	}
}

func TestAFailedWriteEndsTheSessionAndAFailedResizeDoesNot(t *testing.T) {
	// A far end that will not take input is finished. A far end with no notion of size is not:
	// killing a working terminal because it could not be told its width would be absurd.
	c := newTestBed(t)
	far := &fakeFarEnd{resizeErr: fmt.Errorf("this far end has no tty")}
	SetDialer(func(id string, open StreamOpen, onData func([]byte), onClose func(string, bool)) (FarEnd, error) {
		return far, nil
	})

	HandleSessionFrame(openFrame("s1", StreamOpen{}))
	size, _ := json.Marshal(StreamResize{Cols: 10, Rows: 10})
	HandleSessionFrame(&l2sec.ToNodeAgent{Type: StreamResizeRequestType, Tag: "s1", JsonB64: base64.StdEncoding.EncodeToString(size)})
	if SessionCount() != 1 {
		t.Fatal("a refused resize must not end the session")
	}

	far.mu.Lock()
	far.writeErr = fmt.Errorf("broken pipe")
	far.mu.Unlock()
	HandleSessionFrame(dataFrame("s1", []byte("x")))

	if SessionCount() != 0 {
		t.Fatalf("a far end that stopped accepting input must end the session, %d open", SessionCount())
	}
	closes := c.byType(StreamClosedResponseType)
	if len(closes) != 1 || !closedReason(t, closes[0]).Error {
		t.Fatal("the close must be reported once and marked as an error")
	}
}

func TestAMalformedOpenIsRefusedRatherThanCrashing(t *testing.T) {
	c := newTestBed(t)
	SetDialer(func(id string, open StreamOpen, onData func([]byte), onClose func(string, bool)) (FarEnd, error) {
		t.Fatal("a malformed open must never reach the dialer")
		return nil, nil
	})

	HandleSessionFrame(&l2sec.ToNodeAgent{Type: OpenStreamRequestType, Tag: "s1", JsonB64: "!!!not base64!!!"})
	HandleSessionFrame(&l2sec.ToNodeAgent{Type: OpenStreamRequestType, Tag: "s2", JsonB64: base64.StdEncoding.EncodeToString([]byte("not json"))})

	if SessionCount() != 0 {
		t.Fatalf("no session should exist, got %d", SessionCount())
	}
	if got := len(c.byType(StreamClosedResponseType)); got != 2 {
		t.Fatalf("both malformed opens must be answered, got %d", got)
	}
}

func TestAFrameWithNoSessionIdIsIgnored(t *testing.T) {
	c := newTestBed(t)
	SetDialer(func(id string, open StreamOpen, onData func([]byte), onClose func(string, bool)) (FarEnd, error) {
		t.Fatal("a frame with no session id must never dial")
		return nil, nil
	})
	HandleSessionFrame(&l2sec.ToNodeAgent{Type: OpenStreamRequestType, Tag: ""})
	if len(c.sent) != 0 {
		t.Fatalf("expected no traffic, got %d frames", len(c.sent))
	}
}

func TestCloseAllSessionsHangsUpEveryOne(t *testing.T) {
	// A session outlives nothing. Left open across a reconnect it strands a shell on the node.
	c := newTestBed(t)
	var ends []*fakeFarEnd
	var mu sync.Mutex
	SetDialer(func(id string, open StreamOpen, onData func([]byte), onClose func(string, bool)) (FarEnd, error) {
		f := &fakeFarEnd{}
		mu.Lock()
		ends = append(ends, f)
		mu.Unlock()
		return f, nil
	})
	for i := 0; i < 5; i++ {
		HandleSessionFrame(openFrame(fmt.Sprintf("s%d", i), StreamOpen{}))
	}

	CloseAllSessions("the node's connection to RunOS went down")

	if SessionCount() != 0 {
		t.Fatalf("expected every session gone, got %d", SessionCount())
	}
	mu.Lock()
	defer mu.Unlock()
	for i, f := range ends {
		f.mu.Lock()
		n := f.closes
		f.mu.Unlock()
		if n != 1 {
			t.Fatalf("far end %d hung up %d times, want 1", i, n)
		}
	}
	if got := len(c.byType(StreamClosedResponseType)); got != 5 {
		t.Fatalf("every session must be reported closed, got %d", got)
	}
}

func TestConcurrentOpensCannotExceedTheBound(t *testing.T) {
	// The bound is taken BEFORE the dial precisely so two opens arriving together cannot both pass
	// the check. Without the reservation this races and the node ends up over its ceiling.
	newTestBed(t)
	SetDialer(func(id string, open StreamOpen, onData func([]byte), onClose func(string, bool)) (FarEnd, error) {
		time.Sleep(time.Millisecond) // widen the window the reservation closes
		return &fakeFarEnd{}, nil
	})

	var wg sync.WaitGroup
	for i := 0; i < MaxConcurrentSessions*3; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			HandleSessionFrame(openFrame(fmt.Sprintf("s%d", i), StreamOpen{}))
		}(i)
	}
	wg.Wait()

	if got := SessionCount(); got > MaxConcurrentSessions {
		t.Fatalf("the bound was exceeded under concurrency: %d open, limit %d", got, MaxConcurrentSessions)
	}
}

func contains(haystack, needle string) bool {
	return len(needle) > 0 && len(haystack) >= len(needle) && (func() bool {
		for i := 0; i+len(needle) <= len(haystack); i++ {
			if haystack[i:i+len(needle)] == needle {
				return true
			}
		}
		return false
	})()
}
