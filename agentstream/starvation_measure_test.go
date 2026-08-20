package agentstream

import (
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
}

func (r *ratedStream) Send(m *l2sec.FromNodeAgent) error {
	n := len(m.JsonB64) + len(m.Type) + len(m.Tag)
	if m.Type == StreamDataResponseType {
		r.bulk.Add(1)
	} else {
		r.control.Add(1)
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
			HandleSessionFrame(openFrame(fmt.Sprintf("m%d", i), StreamOpen{
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
		sessions  int
		p50, p99  time.Duration
		bulkSent  int64
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
