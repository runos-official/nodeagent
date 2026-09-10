package agentstream

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/runos-official/nodeagent/l2sec"
)

// THE MEASUREMENT THE GOAL DEMANDS, run against the real session machinery.
//
// Goal 31's definition of done: "A terminal producing output as fast as it can does not delay
// cluster operations on the same node, measured." The thing at risk is entirely inside this
// package: every message a node sends takes ONE mutex, so terminal output competes with every
// status reply that node owes the control plane.
//
// This drives that contention directly. Real sessions, opened through the real registry, with a
// real websocket far end producing output as fast as it can, and real control senders measured
// while they do. Nothing here is a mock of the thing under test.
//
// WHAT IT DOES NOT PROVE. This is not the end-to-end acceptance test: it does not include the
// edge, conductor, nodeward or the round trip, and the numbers are not comparable with the
// `nodes/software-info` baseline. It measures the ONE mechanism those hops cannot change, which is
// whether bulk traffic delays control traffic through the shared send path. The end-to-end run
// still needs the nodeward and conductor hops.
//
// Run it with:
//
//	go test ./agentstream/ -run TestMeasureControlLatencyUnderSessionLoad -v -count=1
//
// It is skipped in a normal test run: it takes tens of seconds and it is a measurement, not a gate.

// ratedStream models a link of finite capacity, which is what makes the shared mutex matter.
//
// A fake whose Send returns instantly would hold the lock for nanoseconds and show no contention
// whatever the code did, so it would report a pass for any implementation. The cost is charged per
// byte, so a terminal shovelling megabytes genuinely occupies the link, exactly as it would on the
// wire.
type ratedStream struct {
	l2sec.Nodeward_NodeAgentStreamClient
	bytesPerSecond int
	control        atomic.Int64
	bulk           atomic.Int64

	// Per-session bulk bytes, so the measurement can answer what each terminal actually gets when
	// they are all busy, not only whether the control plane survives them.
	perSession sync.Map // session id -> *atomic.Int64

	closesMu sync.Mutex
	closes   []string
}

func (r *ratedStream) countSession(tag string, n int) {
	v, _ := r.perSession.LoadOrStore(tag, &atomic.Int64{})
	v.(*atomic.Int64).Add(int64(n))
}

func (r *ratedStream) sessionBytes() map[string]int64 {
	out := map[string]int64{}
	r.perSession.Range(func(k, v any) bool {
		out[k.(string)] = v.(*atomic.Int64).Load()
		return true
	})
	return out
}

func (r *ratedStream) Send(m *l2sec.FromNodeAgent) error {
	n := len(m.JsonB64) + len(m.Type) + len(m.Tag)
	if m.Type == StreamDataResponseType {
		r.bulk.Add(1)
		r.countSession(m.Tag, len(m.JsonB64))
	} else {
		r.control.Add(1)
		if m.Type == StreamClosedResponseType {
			if raw, err := base64.StdEncoding.DecodeString(m.JsonB64); err == nil {
				var c StreamClosed
				if json.Unmarshal(raw, &c) == nil {
					r.closesMu.Lock()
					r.closes = append(r.closes, m.Tag+": "+c.Reason)
					r.closesMu.Unlock()
				}
			}
		}
	}
	// The link's own cost, paid while the caller holds streamMutex, as on the wire.
	time.Sleep(time.Duration(float64(n) / float64(r.bytesPerSecond) * float64(time.Second)))
	return nil
}

// spewServer is `yes` as a websocket: it writes as fast as the other side will take it.
func spewServer(t *testing.T, chunk int) (string, func()) {
	t.Helper()
	up := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	stop := make(chan struct{})

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := up.Upgrade(w, r, http.Header{"Sec-WebSocket-Protocol": []string{"runos.tty.v1"}})
		if err != nil {
			return
		}
		defer conn.Close()
		go func() {
			for {
				if _, _, err := conn.ReadMessage(); err != nil {
					return
				}
			}
		}()
		payload := []byte(strings.Repeat("y\n", chunk/2))
		for {
			select {
			case <-stop:
				return
			default:
			}
			if err := conn.WriteMessage(websocket.TextMessage, payload); err != nil {
				return
			}
		}
	}))

	return "ws" + strings.TrimPrefix(srv.URL, "http"), func() { close(stop); srv.Close() }
}

func percentile(v []time.Duration, p float64) time.Duration {
	if len(v) == 0 {
		return 0
	}
	i := int(float64(len(v)-1) * p / 100)
	return v[i]
}

// measureControl times SendToNodeward, which is the exact call every instruction response makes.
func measureControl(samples int, gap time.Duration) []time.Duration {
	out := make([]time.Duration, 0, samples)
	msg := &l2sec.FromNodeAgent{Type: GetNodeStatusResponseType, JsonB64: strings.Repeat("s", 512)}
	for i := 0; i < samples; i++ {
		began := time.Now()
		_ = SendToNodeward(&l2sec.FromNodeAgent{Type: msg.Type, Tag: fmt.Sprintf("c%d", i), JsonB64: msg.JsonB64})
		out = append(out, time.Since(began))
		time.Sleep(gap)
	}
	sort.Slice(out, func(a, b int) bool { return out[a] < out[b] })
	return out
}

func TestMeasureControlLatencyUnderSessionLoad(t *testing.T) {
	if os.Getenv("RUNOS_MEASURE") == "" {
		t.Skip("measurement, not a gate: set RUNOS_MEASURE=1 to run it")
	}

	// ~8 MB/s, a deliberately modest link so contention is visible rather than hidden by headroom.
	const linkBytesPerSecond = 8 * 1024 * 1024
	const controlSamples = 60
	const controlGap = 15 * time.Millisecond

	chunk := 16 * 1024
	if v := os.Getenv("RUNOS_MEASURE_CHUNK"); v != "" {
		fmt.Sscanf(v, "%d", &chunk)
	}
	t.Logf("far-end output chunk = %d bytes, link = %d bytes/sec, so one bulk send costs about %v",
		chunk, linkBytesPerSecond, time.Duration(float64(chunk)/float64(linkBytesPerSecond)*float64(time.Second)))
	url, stopSpew := spewServer(t, chunk)
	defer stopSpew()

	run := func(sessions int) (p50, p99 time.Duration, bulk int64) {
		rs := &ratedStream{bytesPerSecond: linkBytesPerSecond}

		streamMutex.Lock()
		prev := globalStream
		globalStream = rs
		streamMutex.Unlock()

		sessionsMu.Lock()
		sessionsSnapshot := sessions2map()
		sessions2clear()
		sessionsMu.Unlock()

		prevDialer := currentDialer()
		SetDialer(func(id string, open StreamOpen, onData func([]byte), onClose func(string, bool)) (FarEnd, error) {
			return dialWebSocketAt(url, []string{"runos.tty.v1", "runos.psk.X"}, http.Header{}, onData, onClose)
		})

		defer func() {
			CloseAllSessions("measurement over")
			SetDialer(prevDialer)
			streamMutex.Lock()
			globalStream = prev
			streamMutex.Unlock()
			sessionsMu.Lock()
			sessions2restore(sessionsSnapshot)
			sessionsMu.Unlock()
		}()

		for i := 0; i < sessions; i++ {
			HandleSessionFrame(openFrame(fmt.Sprintf("lat%d-m%d", sessions, i), StreamOpen{
				Target: "SERVICE_WEBSOCKET", Namespace: "runos", Name: "spew", Port: 7681,
			}))
		}
		if got := SessionCount(); got != sessions {
			t.Fatalf("wanted %d sessions, opened %d", sessions, got)
		}
		// Let the far ends get to full speed before measuring.
		time.Sleep(1500 * time.Millisecond)

		before := rs.bulk.Load()
		lat := measureControl(controlSamples, controlGap)
		return percentile(lat, 50), percentile(lat, 99), rs.bulk.Load() - before
	}

	type row struct {
		sessions int
		p50, p99 time.Duration
		bulkSent int64
	}
	var rows []row
	for _, n := range []int{0, 1, 16} {
		p50, p99, bulk := run(n)
		rows = append(rows, row{n, p50, p99, bulk})
		t.Logf("sessions=%-3d control p50=%-8v p99=%-8v   bulk frames sent during the window=%d", n, p50, p99, bulk)
	}

	// THE BOUND IS DERIVED, not chosen. Control cannot recall a bulk send already in flight, so it
	// waits at most one output frame's transmit time. That frame is capped at MaxBulkFrameBytes, so
	// the bound follows from the cap and the modelled link and nothing else. A generous round
	// number here would have passed the 70 ms starvation that G31-F1 actually was.
	oneFrame := time.Duration(float64(MaxBulkFrameBytes) / float64(linkBytesPerSecond) * float64(time.Second))
	t.Logf("one capped output frame costs %v on this link, so control should wait about that and no more", oneFrame)

	idle := rows[0]
	for _, r := range rows[1:] {
		if r.bulkSent == 0 {
			t.Fatalf("with %d sessions no terminal output was sent at all, so this measured nothing", r.sessions)
		}
		limit := idle.p50 + 3*oneFrame
		if r.p50 > limit {
			t.Errorf("STARVATION: control p50 went from %v idle to %v with %d sessions running flat out. "+
				"The bound is one capped frame (%v); above that, session data is delaying the control plane.",
				idle.p50, r.p50, r.sessions, oneFrame)
		}
	}
}

// The registry is package state and this measurement borrows it. These keep the borrow explicit.
func sessions2map() map[string]*session {
	out := make(map[string]*session, len(sessions))
	for k, v := range sessions {
		out[k] = v
	}
	return out
}

func sessions2clear() { sessions = map[string]*session{} }

func sessions2restore(m map[string]*session) { sessions = m }

var _ = sync.Mutex{}

// What does EACH terminal get when they are all busy, and is the share fair?
//
// The operator's question, and it is not the same one the starvation test answers. That one asks
// whether the control plane survives busy terminals. This asks whether the terminals themselves
// are usable, and whether one of them can take the link off the others.
//
// EVERY SESSION ON A NODE SHARES THAT NODE'S ONE STREAM. Bulk senders serialise against each other
// on `bulkGate`, so the node's uplink is divided between whatever is running. That is a property
// of the design, not a defect; what would be a defect is an UNFAIR division, where one terminal
// gets most of it and another gets almost none.
func TestMeasurePerSessionShare(t *testing.T) {
	if os.Getenv("RUNOS_MEASURE") == "" {
		t.Skip("measurement, not a gate: set RUNOS_MEASURE=1 to run it")
	}

	const linkBytesPerSecond = 8 * 1024 * 1024
	const window = 4 * time.Second

	url, stopSpew := spewServer(t, 16*1024)
	defer stopSpew()

	run := func(sessions int) {
		rs := &ratedStream{bytesPerSecond: linkBytesPerSecond}

		streamMutex.Lock()
		prev := globalStream
		globalStream = rs
		streamMutex.Unlock()

		sessionsMu.Lock()
		snap := sessions2map()
		sessions2clear()
		sessionsMu.Unlock()

		// WHICH SIDE IS STARVING, if anything is. A session that sends nothing because its far end
		// fed it nothing is a limit of this harness; one that sends nothing while being fed is the
		// send path starving it. Counting both is the only way to tell them apart.
		var recvMu sync.Mutex
		received := map[string]int64{}

		prevDialer := currentDialer()
		SetDialer(func(id string, open StreamOpen, onData func([]byte), onClose func(string, bool)) (FarEnd, error) {
			counted := func(b []byte) {
				recvMu.Lock()
				received[id] += int64(len(b))
				recvMu.Unlock()
				onData(b)
			}
			return dialWebSocketAt(url, []string{"runos.tty.v1", "runos.psk.X"}, http.Header{}, counted, onClose)
		})
		defer func() {
			CloseAllSessions("measurement over")
			SetDialer(prevDialer)
			streamMutex.Lock()
			globalStream = prev
			streamMutex.Unlock()
			sessionsMu.Lock()
			sessions2restore(snap)
			sessionsMu.Unlock()
		}()

		for i := 0; i < sessions; i++ {
			HandleSessionFrame(openFrame(fmt.Sprintf("share%d-s%02d", sessions, i), StreamOpen{
				Target: "SERVICE_WEBSOCKET", Namespace: "runos", Name: "spew", Port: 7681,
			}))
		}
		time.Sleep(500 * time.Millisecond)

		// A session that never opened would look exactly like a starved one, so rule that out
		// before reading anything into a zero.
		if got := SessionCount(); got != sessions {
			// Say WHY. A failed dial and a starved session look identical from the byte counts, and
			// the reason is the whole difference between a harness limit and a real defect.
			var reasons []string
			for _, m := range rs.closes {
				reasons = append(reasons, m)
			}
			t.Fatalf("wanted %d sessions open, got %d. Refusals: %s", sessions, got, strings.Join(reasons, " | "))
		}

		start := rs.sessionBytes()
		time.Sleep(window)
		end := rs.sessionBytes()

		// COMPUTED OVER THE KNOWN SESSION IDS, never over whatever keys a map happens to hold.
		// The first draft iterated the byte map and padded the result to length, and it reported
		// "4 sessions got nothing" for a run in which the raw rows showed all sixteen sending
		// within 1.2% of each other. A summary that disagrees with its own raw data is worse than
		// no summary, because it is the line somebody quotes.
		recvMu.Lock()
		type share struct {
			id       string
			fed      int64
			sentKiBs float64
		}
		rows := make([]share, 0, sessions)
		for i := 0; i < sessions; i++ {
			id := fmt.Sprintf("share%d-s%02d", sessions, i)
			sent := float64(end[id]-start[id]) * 3 / 4 / window.Seconds() / 1024
			rows = append(rows, share{id, received[id], sent})
		}
		recvMu.Unlock()

		var total float64
		var fedAndSending []float64
		fedButSilent, neverFed := 0, 0
		for _, r := range rows {
			total += r.sentKiBs
			switch {
			case r.fed == 0:
				neverFed++
			case r.sentKiBs == 0:
				fedButSilent++
			default:
				fedAndSending = append(fedAndSending, r.sentKiBs)
			}
			if os.Getenv("RUNOS_MEASURE_RAW") != "" {
				t.Logf("   RAW %-16s fed=%-10d sent=%.0f KiB/s", r.id, r.fed, r.sentKiBs)
			}
		}
		sort.Float64s(fedAndSending)

		lo, hi, med := 0.0, 0.0, 0.0
		if n := len(fedAndSending); n > 0 {
			lo, hi, med = fedAndSending[0], fedAndSending[n-1], fedAndSending[n/2]
		}
		t.Logf("sessions=%-3d total=%7.0f KiB/s   per session: slowest=%6.0f median=%6.0f fastest=%6.0f KiB/s   (fed+sending=%d, fed+silent=%d, never fed=%d)",
			sessions, total, lo, med, hi, len(fedAndSending), fedButSilent, neverFed)

		// ONLY A SESSION THE FAR END ACTUALLY FED can be starved by the send path. One that was
		// never fed is this harness failing to drive N websockets evenly on one machine, and
		// failing the run for that would blame the code for the test's own limit.
		if fedButSilent > 0 {
			t.Errorf("STARVED BY THE SEND PATH: %d session(s) were fed by the far end and still sent nothing in %v",
				fedButSilent, window)
		}
		// FAIRNESS among the sessions that had something to send. A node's uplink divided N ways is
		// the design; one terminal taking the link while another crawls is not.
		if len(fedAndSending) > 1 && hi > lo*4 {
			t.Errorf("UNFAIR: fastest %.0f KiB/s, slowest %.0f KiB/s, a %.1fx spread across %d fed sessions",
				hi, lo, hi/lo, len(fedAndSending))
		}
	}

	for _, n := range []int{1, 4, 16} {
		run(n)
	}
}
