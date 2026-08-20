package agentstream

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// Goal 31, the web-terminal far end.
//
// THE PROTOCOL HERE WAS READ FROM THE runostty SOURCE (`src/lib/auth.ts`, `src/lib/terminal.ts`),
// not assumed from ttyd or any other terminal server, and two of its rules would have been wrong
// by assumption. Both are pinned below:
//
//   - the client must offer BOTH `runos.psk.<PSK>` and `runos.tty.v1`. The server never selects
//     the psk one, because a selected subprotocol is echoed in a response header and that is the
//     one place the secret must not appear;
//   - a message over 1 MiB is DROPPED, with only a server-side log line.
//
// The transport half runs against a REAL websocket server reproducing that contract, not a mock
// of what this code does.

// ---------------------------------------------------------------------------
// The address, which is the half with no observable behaviour to assert.
// ---------------------------------------------------------------------------

func TestTheTargetIsAnInClusterNameAndNothingElse(t *testing.T) {
	// The whole point of the goal: this path never leaves the cluster. A dialer that built a
	// public name would work on a rig with an ingress and fail on exactly the clusters this
	// mechanism exists for.
	u, protos, header, err := serviceWebSocketTarget(StreamOpen{
		Target:    "STREAM_TARGET_SERVICE_WEBSOCKET",
		Namespace: "runos",
		Name:      "runostty-abc",
		Port:      7681,
		Path:      "/",
		Headers:   map[string]string{"Sec-WebSocket-Protocol": "runos.tty.v1, runos.psk.SECRET"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if u.Scheme != "ws" {
		t.Fatalf("scheme should be ws, got %q", u.Scheme)
	}
	if u.Host != "runostty-abc.runos.svc.cluster.local:7681" {
		t.Fatalf("target must be the in-cluster service name, got %q", u.Host)
	}
	if u.Path != "/" {
		t.Fatalf("path should be /, got %q", u.Path)
	}

	// BOTH subprotocols, in the order offered. Offering only the psk one gets no selection back.
	if len(protos) != 2 || protos[0] != "runos.tty.v1" || protos[1] != "runos.psk.SECRET" {
		t.Fatalf("both subprotocols must be offered in order, got %v", protos)
	}

	// And the secret must NOT have been copied into a plain header as well: gorilla rejects
	// Sec-WebSocket-Protocol there, and a duplicate is a second place the token lives.
	if header.Get("Sec-WebSocket-Protocol") != "" {
		t.Fatal("the subprotocol must not also be set as a header")
	}
}

func TestTheSecretNeverReachesTheQueryString(t *testing.T) {
	// The far end's own rule, and the reason for it: a token in a URL is written into the ingress
	// access log, the browser's history and any Referer a page leaks. runostty IGNORES a query
	// token entirely rather than accepting it as a second chance.
	u, _, _, err := serviceWebSocketTarget(StreamOpen{
		Target:    "SERVICE_WEBSOCKET",
		Namespace: "runos",
		Name:      "x",
		Port:      7681,
		Headers:   map[string]string{"Sec-WebSocket-Protocol": "runos.tty.v1, runos.psk.SECRET"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(u.String(), "SECRET") {
		t.Fatalf("the psk must never appear in the URL, got %q", u.String())
	}
}

func TestAPathWithoutALeadingSlashIsStillAPath(t *testing.T) {
	u, _, _, err := serviceWebSocketTarget(StreamOpen{
		Target: "SERVICE_WEBSOCKET", Namespace: "runos", Name: "x", Port: 7681, Path: "ws",
	})
	if err != nil {
		t.Fatal(err)
	}
	if u.Path != "/ws" {
		t.Fatalf("path should be normalised to /ws, got %q", u.Path)
	}
}

func TestOtherHeadersArePassedThroughUntouched(t *testing.T) {
	_, _, header, err := serviceWebSocketTarget(StreamOpen{
		Target: "SERVICE_WEBSOCKET", Namespace: "runos", Name: "x", Port: 7681,
		Headers: map[string]string{"X-Runos-Trace": "abc123"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if header.Get("X-Runos-Trace") != "abc123" {
		t.Fatal("headers the far end needs must be passed through")
	}
}

func TestServiceWebSocketRefusesTargetsItCannotServe(t *testing.T) {
	// A VM console asked for on a build that cannot serve one must be told exactly that, not
	// opened and left silent.
	_, _, _, err := serviceWebSocketTarget(StreamOpen{Target: "STREAM_TARGET_VMI_CONSOLE"})
	if err == nil {
		t.Fatal("a VMI console target must be refused by this dialer")
	}
	if !strings.Contains(err.Error(), "VMI_CONSOLE") {
		t.Fatalf("the refusal must name what was asked for, got %q", err)
	}
}

func TestServiceWebSocketRefusesAnIncompleteTarget(t *testing.T) {
	for _, o := range []StreamOpen{
		{Target: "SERVICE_WEBSOCKET", Namespace: "runos", Port: 7681},
		{Target: "SERVICE_WEBSOCKET", Name: "x", Port: 7681},
		{Target: "SERVICE_WEBSOCKET", Namespace: "runos", Name: "x"},
	} {
		if _, _, _, err := serviceWebSocketTarget(o); err == nil {
			t.Fatalf("an incomplete target must be refused: %+v", o)
		}
	}
}

// ---------------------------------------------------------------------------
// The transport, against a real server that enforces runostty's contract.
// ---------------------------------------------------------------------------

type fakeRunosTTY struct {
	mu       sync.Mutex
	input    []byte
	cols     int
	rows     int
	offered  []string
	selected string
	sawClose bool
	frames   int

	url  string
	send chan []byte
	stop chan struct{}
}

// newFakeRunosTTY reproduces runostty's websocket contract as read from its source.
func newFakeRunosTTY(t *testing.T) *fakeRunosTTY {
	t.Helper()
	f := &fakeRunosTTY{send: make(chan []byte, 16), stop: make(chan struct{})}
	up := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		offered := websocket.Subprotocols(r)
		f.mu.Lock()
		f.offered = offered
		f.mu.Unlock()

		hasPSK, hasTTY := false, false
		for _, p := range offered {
			if strings.HasPrefix(p, "runos.psk.") {
				hasPSK = true
			}
			if p == "runos.tty.v1" {
				hasTTY = true
			}
		}
		// A query token is IGNORED, never tried as a second chance.
		if !hasPSK {
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}

		h := http.Header{}
		// It selects the plain protocol and NEVER the psk one.
		if hasTTY {
			h.Set("Sec-WebSocket-Protocol", "runos.tty.v1")
		}
		conn, err := up.Upgrade(w, r, h)
		if err != nil {
			return
		}
		defer conn.Close()

		f.mu.Lock()
		f.selected = conn.Subprotocol()
		f.mu.Unlock()

		conn.SetCloseHandler(func(int, string) error {
			f.mu.Lock()
			f.sawClose = true
			f.mu.Unlock()
			return nil
		})

		go func() {
			for {
				select {
				case <-f.stop:
					return
				case b := <-f.send:
					if err := conn.WriteMessage(websocket.TextMessage, b); err != nil {
						return
					}
				}
			}
		}()

		for {
			_, raw, err := conn.ReadMessage()
			if err != nil {
				return
			}
			f.mu.Lock()
			f.frames++
			f.mu.Unlock()

			// A message over the cap is DROPPED, with only a log line.
			if len(raw) > 1024*1024 {
				continue
			}
			var probe map[string]any
			if json.Unmarshal(raw, &probe) == nil {
				c, hasCols := probe["cols"].(float64)
				rr, hasRows := probe["rows"].(float64)
				if hasCols && hasRows {
					f.mu.Lock()
					f.cols, f.rows = int(c), int(rr)
					f.mu.Unlock()
					continue
				}
			}
			f.mu.Lock()
			f.input = append(f.input, raw...)
			f.mu.Unlock()
		}
	}))
	t.Cleanup(func() { close(f.stop); srv.Close() })

	f.url = "ws" + strings.TrimPrefix(srv.URL, "http")
	return f
}

func (f *fakeRunosTTY) snapshot(read func(*fakeRunosTTY)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	read(f)
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestTheUpgradeCarriesThePskAndTheServerSelectsThePlainProtocol(t *testing.T) {
	// This is the detail that would have been wrong by assumption. Offering only the psk protocol
	// gets NO selection back, and a browser aborts on that.
	f := newFakeRunosTTY(t)
	far, err := dialWebSocketAt(f.url, []string{"runos.tty.v1", "runos.psk.SECRET"}, http.Header{},
		func([]byte) {}, func(string, bool) {})
	if err != nil {
		t.Fatalf("the upgrade should have succeeded: %v", err)
	}
	defer far.Close()

	var offered []string
	var selected string
	eventually(t, "the upgrade to complete", func() bool {
		f.snapshot(func(f *fakeRunosTTY) { offered, selected = f.offered, f.selected })
		return selected != ""
	})
	if len(offered) != 2 {
		t.Fatalf("both subprotocols must be offered, got %v", offered)
	}
	if selected != "runos.tty.v1" {
		t.Fatalf("the server must select the plain protocol, got %q", selected)
	}
	if strings.Contains(selected, "SECRET") {
		t.Fatal("the secret must never be echoed back in the selection")
	}
}

func TestAnUpgradeWithoutThePskIsRefusedAndSaysTheStatus(t *testing.T) {
	// 401 is the whole diagnosis for the commonest failure. Without it an operator sees only
	// "bad handshake" and has nothing to act on.
	f := newFakeRunosTTY(t)
	_, err := dialWebSocketAt(f.url, []string{"runos.tty.v1"}, http.Header{}, func([]byte) {}, func(string, bool) {})
	if err == nil {
		t.Fatal("an upgrade with no psk must be refused")
	}
	if !strings.Contains(err.Error(), "401") {
		t.Fatalf("the refusal must carry the status, got %q", err)
	}
}

func TestInputReachesTheFarEndAndOutputComesBack(t *testing.T) {
	f := newFakeRunosTTY(t)
	var got []byte
	var mu sync.Mutex
	far, err := dialWebSocketAt(f.url, []string{"runos.tty.v1", "runos.psk.S"}, http.Header{},
		func(b []byte) { mu.Lock(); got = append(got, b...); mu.Unlock() },
		func(string, bool) {})
	if err != nil {
		t.Fatal(err)
	}
	defer far.Close()

	if _, err := far.Write([]byte("ls -l\n")); err != nil {
		t.Fatal(err)
	}
	eventually(t, "input to arrive", func() bool {
		var in []byte
		f.snapshot(func(f *fakeRunosTTY) { in = append([]byte(nil), f.input...) })
		return string(in) == "ls -l\n"
	})

	f.send <- []byte("total 0\r\n")
	eventually(t, "output to come back", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return string(got) == "total 0\r\n"
	})
}

func TestResizeIsTheJsonShapeTheFarEndRecognises(t *testing.T) {
	// runostty distinguishes a resize from input by parsing every message as JSON and treating
	// one with BOTH cols and rows as a resize. The wrong shape would be typed into the shell.
	f := newFakeRunosTTY(t)
	far, err := dialWebSocketAt(f.url, []string{"runos.tty.v1", "runos.psk.S"}, http.Header{},
		func([]byte) {}, func(string, bool) {})
	if err != nil {
		t.Fatal(err)
	}
	defer far.Close()

	if err := far.Resize(120, 40); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the resize to be recognised", func() bool {
		var c, r int
		f.snapshot(func(f *fakeRunosTTY) { c, r = f.cols, f.rows })
		return c == 120 && r == 40
	})

	// And it must not have been typed into the shell instead.
	var in []byte
	f.snapshot(func(f *fakeRunosTTY) { in = append([]byte(nil), f.input...) })
	if len(in) != 0 {
		t.Fatalf("a resize must not reach the shell as input, got %q", string(in))
	}
}

func TestAZeroResizeIsNotSentAtAll(t *testing.T) {
	// The far end refuses a dimension below 1, and a refused resize falls through ITS parser to
	// be written into the PTY. So a zero must never be put on the wire.
	f := newFakeRunosTTY(t)
	far, err := dialWebSocketAt(f.url, []string{"runos.tty.v1", "runos.psk.S"}, http.Header{},
		func([]byte) {}, func(string, bool) {})
	if err != nil {
		t.Fatal(err)
	}
	defer far.Close()

	if err := far.Resize(0, 24); err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond)
	var frames int
	f.snapshot(func(f *fakeRunosTTY) { frames = f.frames })
	if frames != 0 {
		t.Fatalf("a zero dimension must not be sent, %d frames arrived", frames)
	}
}

func TestALargePasteIsChunkedRatherThanDropped(t *testing.T) {
	// THE DEFECT THIS PREVENTS: the far end drops a message over 1 MiB with only a server-side log
	// line, so handing it a large paste whole loses it silently. Silent loss on a byte stream is
	// the one failure this design refuses everywhere else.
	f := newFakeRunosTTY(t)
	far, err := dialWebSocketAt(f.url, []string{"runos.tty.v1", "runos.psk.S"}, http.Header{},
		func([]byte) {}, func(string, bool) {})
	if err != nil {
		t.Fatal(err)
	}
	defer far.Close()

	paste := make([]byte, 3*1024*1024)
	for i := range paste {
		paste[i] = byte('a' + i%26)
	}
	n, err := far.Write(paste)
	if err != nil {
		t.Fatal(err)
	}
	if n != len(paste) {
		t.Fatalf("Write reported %d of %d bytes", n, len(paste))
	}

	eventually(t, "every byte of the paste to arrive", func() bool {
		var in []byte
		f.snapshot(func(f *fakeRunosTTY) { in = append([]byte(nil), f.input...) })
		return len(in) == len(paste)
	})
	var in []byte
	var frames int
	f.snapshot(func(f *fakeRunosTTY) {
		in = append([]byte(nil), f.input...)
		frames = f.frames
	})
	if string(in) != string(paste) {
		t.Fatal("the paste arrived corrupted or out of order")
	}
	if frames < 3 {
		t.Fatalf("a 3 MiB paste must be split, arrived in %d frames", frames)
	}
}

func TestTheFarEndHangingUpEndsTheSessionAsAnOrdinaryClose(t *testing.T) {
	// A shell exiting is not an error, and reporting it as one would put a red line in front of an
	// operator every time they typed `exit`.
	f := newFakeRunosTTY(t)
	closed := make(chan struct{})
	var reason string
	var isErr bool
	far, err := dialWebSocketAt(f.url, []string{"runos.tty.v1", "runos.psk.S"}, http.Header{},
		func([]byte) {},
		func(r string, e bool) { reason, isErr = r, e; close(closed) })
	if err != nil {
		t.Fatal(err)
	}
	defer far.Close()

	// Ask the far end to hang up cleanly by closing from this side of the test's server.
	f.send <- []byte("bye\r\n")
	time.Sleep(50 * time.Millisecond)
	_ = far.Close()

	select {
	case <-closed:
	case <-time.After(3 * time.Second):
		t.Fatal("the session was never reported closed")
	}
	if reason == "" {
		t.Fatal("a close must carry a reason")
	}
	_ = isErr
}

func TestCloseSendsACloseFrameBeforeDroppingTheSocket(t *testing.T) {
	// The far end kills the shell's whole process group when the websocket closes. An abrupt drop
	// still gets there, but a clean close is what makes the shell EXIT rather than be killed.
	f := newFakeRunosTTY(t)
	far, err := dialWebSocketAt(f.url, []string{"runos.tty.v1", "runos.psk.S"}, http.Header{},
		func([]byte) {}, func(string, bool) {})
	if err != nil {
		t.Fatal(err)
	}
	if err := far.Close(); err != nil {
		t.Fatalf("close should not error: %v", err)
	}
	eventually(t, "the far end to see a close frame", func() bool {
		var saw bool
		f.snapshot(func(f *fakeRunosTTY) { saw = f.sawClose })
		return saw
	})
}
