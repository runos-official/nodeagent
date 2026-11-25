package agent

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/spf13/viper"
	"google.golang.org/grpc"

	"github.com/runos-official/nodeagent/agentstream"
	"github.com/runos-official/nodeagent/backend"
	"github.com/runos-official/nodeagent/commons"
	"github.com/runos-official/nodeagent/l2sec"
	"github.com/runos-official/nodeagent/roslog"
	"github.com/runos-official/nodeagent/uc/certificate"
	"github.com/runos-official/nodeagent/uc/certpolicy"
	"github.com/runos-official/nodeagent/uc/certrenew"
	"github.com/runos-official/nodeagent/uc/certstore"
)

// testNID is a made-up node id.
const testNID = "node-under-test"

// reconnectLine is the line of a lost connection.
const reconnectLine = "Connection to Nodeward lost"

// safeLog is a log sink that several goroutines share.
type safeLog struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (s *safeLog) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.Write(p)
}

// lines decodes the JSON lines written so far.
func (s *safeLog) lines(t *testing.T) []map[string]any {
	t.Helper()
	s.mu.Lock()
	text := s.buf.String()
	s.mu.Unlock()
	var out []map[string]any
	for _, l := range strings.Split(strings.TrimSpace(text), "\n") {
		if l == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(l), &m); err != nil {
			t.Fatalf("log line is not JSON: %s", l)
		}
		out = append(out, m)
	}
	return out
}

// withMsg returns the lines whose message holds msg.
func (s *safeLog) withMsg(t *testing.T, msg string) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, l := range s.lines(t) {
		if m, _ := l["msg"].(string); strings.Contains(m, msg) {
			out = append(out, l)
		}
	}
	return out
}

// loud returns the WARN and ERROR lines other than the expired line.
func (s *safeLog) loud(t *testing.T) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, l := range s.lines(t) {
		if l["msg"] == expiredMessage {
			continue
		}
		if lv := l["level"]; lv == "WARN" || lv == "ERROR" {
			out = append(out, l)
		}
	}
	return out
}

func captureSafeLog(t *testing.T) *safeLog {
	t.Helper()
	s := &safeLog{}
	old := roslog.L
	roslog.L = slog.New(slog.NewJSONHandler(s, &slog.HandlerOptions{Level: slog.LevelDebug}))
	t.Cleanup(func() { roslog.L = old })
	return s
}

func useNID(t *testing.T, nid string) {
	t.Helper()
	old := viper.GetString("node.nid")
	viper.Set("node.nid", nid)
	t.Cleanup(func() { viper.Set("node.nid", old) })
}

// validHold is a hold whose held leaf never expires.
func validHold() *expiredHold {
	return &expiredHold{
		readLeaf: func() (certrenew.Leaf, error) {
			return certrenew.Leaf{Serial: "ab", NotAfter: time.Now().Add(1000 * time.Hour)}, nil
		},
		loadPair: func() (certrenew.Leaf, error) { return certrenew.Leaf{}, errors.New("not used") },
		now:      time.Now,
		reporter: newExpiredReporter(testNID, nil),
		every:    expiredCheckEvery,
	}
}

// fileHold is the production hold on the paths p with the clock now.
func fileHold(p certstore.Paths, now func() time.Time, r *expiredReporter, b *commons.Backoff) *expiredHold {
	return &expiredHold{
		readLeaf: func() (certrenew.Leaf, error) { return certrenew.ReadLeafFile(p.Cert) },
		loadPair: func() (certrenew.Leaf, error) { return loadPairLeaf(p.Cert, p.Key) },
		now:      now,
		reporter: r,
		backoff:  b,
		every:    expiredCheckEvery,
	}
}

// leafOf reads the leaf of material m.
func leafOf(t *testing.T, m certstore.Material) certrenew.Leaf {
	t.Helper()
	path := filepath.Join(t.TempDir(), "leaf.crt")
	if err := os.WriteFile(path, m.CertPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	l, err := certrenew.ReadLeafFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return l
}

// renewStub counts renewal requests.
type renewStub struct{ calls atomic.Int64 }

func (s *renewStub) deps(t *testing.T) certrenew.Deps {
	return certrenew.Deps{
		Renew: func(context.Context, certrenew.Request) (certificate.Result, error) {
			s.calls.Add(1)
			return certificate.Result{}, errors.New("test: no renewal")
		},
		StatePath: filepath.Join(t.TempDir(), "run", "certificate-renewal.json"),
		Rand:      func() float64 { return 0.5 },
		PID:       4242,
	}
}

// checkExpiredLine checks the members of one expired line.
func checkExpiredLine(t *testing.T, l map[string]any, leaf certrenew.Leaf) {
	t.Helper()
	if l["level"] != "ERROR" || l["expired_at"] != leaf.NotAfter.UTC().Format(time.RFC3339) ||
		l["serial"] != leaf.Serial || l["nid"] != testNID || l["next_step"] != certpolicy.ExpiredRemedy(leaf.NotAfter) {
		t.Fatalf("expired line = %v, want expiry %s, serial %s, node %s and the remedy", l,
			leaf.NotAfter.UTC().Format(time.RFC3339), leaf.Serial, testNID)
	}
}

// runAgentInBubble runs runAgentWith in a goroutine. stop cancels it and
// waits for its end.
func runAgentInBubble(t *testing.T, d agentDeps) (done <-chan struct{}, stop func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	ch := make(chan struct{})
	go func() {
		defer close(ch)
		runAgentWith(ctx, d)
	}()
	return ch, func() {
		cancel()
		<-ch
	}
}

func isClosed(ch <-chan struct{}) bool {
	select {
	case <-ch:
		return true
	default:
		return false
	}
}

// TestExpiredReporterOneLinePerHour (plan 3.5): two callers report one serial
// from several goroutines for 3 hours: one line at entry and at most 3 more.
// A new serial starts a new count.
func TestExpiredReporterOneLinePerHour(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		logs := captureSafeLog(t)
		r := newExpiredReporter(testNID, nil)
		a := certrenew.Leaf{Serial: "aa", NotAfter: time.Now().Add(-time.Minute)}
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Hour+time.Minute)
		defer cancel()
		var wg sync.WaitGroup
		for _, every := range []time.Duration{5 * time.Second, 30 * time.Second, 30 * time.Second} {
			wg.Add(1)
			go func() {
				defer wg.Done()
				tk := time.NewTicker(every)
				defer tk.Stop()
				r.Report(a)
				for {
					select {
					case <-ctx.Done():
						return
					case <-tk.C:
						r.Report(a)
					}
				}
			}()
		}
		time.Sleep(time.Second)
		synctest.Wait()
		if n := len(logs.withMsg(t, expiredMessage)); n != 1 {
			t.Fatalf("lines at entry = %d, want 1", n)
		}
		wg.Wait()
		lines := logs.withMsg(t, expiredMessage)
		if len(lines) < 2 || len(lines) > 4 {
			t.Fatalf("lines over 3 hours = %d, want 1 at entry and at most 3 more", len(lines))
		}
		for _, l := range lines {
			checkExpiredLine(t, l, a)
		}
		b := certrenew.Leaf{Serial: "bb", NotAfter: time.Now().Add(-time.Second)}
		r.Report(b)
		r.Report(b)
		if got := logs.withMsg(t, expiredMessage); len(got) != len(lines)+1 || got[len(got)-1]["serial"] != "bb" {
			t.Fatalf("a new serial gave %d new lines, want 1", len(got)-len(lines))
		}
	})
}

// TestAgentStaysUpWithExpiredCertificate (criterion 3) and
// TestStateFileSaysExpired (criterion 7): start with an expired pair gives no
// exit, zero dials and one ERROR line; after 3 hours at most 3 more such
// lines and zero reconnect lines. The state file says expired and no
// request left the agent.
func TestAgentStaysUpWithExpiredCertificate(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		p := useCredDir(t, t.TempDir())
		useNID(t, testNID)
		now := time.Now()
		m := timedMaterial(t, now.Add(-48*time.Hour), now.Add(-time.Hour))
		writeMaterial(t, p, m)
		leaf := leafOf(t, m)
		logs := captureSafeLog(t)

		var dials atomic.Int64
		stub := &renewStub{}
		deps := stub.deps(t)
		done, stop := runAgentInBubble(t, agentDeps{
			connect: func(context.Context, *certrenew.Loop) (bool, time.Duration) {
				dials.Add(1)
				return false, 0
			},
			renewal: deps,
		})
		defer stop()

		time.Sleep(time.Minute)
		synctest.Wait()
		lines := logs.withMsg(t, expiredMessage)
		if isClosed(done) || dials.Load() != 0 || len(lines) != 1 {
			t.Fatalf("after start: exited=%v dials=%d expired lines=%d, want running, 0 and 1",
				isClosed(done), dials.Load(), len(lines))
		}
		checkExpiredLine(t, lines[0], leaf)

		time.Sleep(3 * time.Hour)
		synctest.Wait()
		lines = logs.withMsg(t, expiredMessage)
		if isClosed(done) || dials.Load() != 0 || len(lines) < 2 || len(lines) > 4 {
			t.Fatalf("after 3 hours: exited=%v dials=%d expired lines=%d, want running, 0 and 2 to 4",
				isClosed(done), dials.Load(), len(lines))
		}
		if n := len(logs.withMsg(t, reconnectLine)); n != 0 {
			t.Fatalf("%d reconnect lines, want 0", n)
		}
		if loud := logs.loud(t); len(loud) != 0 {
			t.Fatalf("other WARN or ERROR lines: %v", loud)
		}

		// TestStateFileSaysExpired.
		b, err := os.ReadFile(deps.StatePath)
		if err != nil {
			t.Fatal(err)
		}
		var st certrenew.State
		if err := json.Unmarshal(b, &st); err != nil {
			t.Fatal(err)
		}
		if st.LastResult != certrenew.ResultExpired || st.Attempts != 0 || stub.calls.Load() != 0 {
			t.Fatalf("state = %+v, renewal requests = %d; want lastResult expired and no request", st, stub.calls.Load())
		}
	})
}

// TestStreamEndAfterExpiryEntersState (criterion 4): a connection that ends
// after the leaf expired gives the state, zero further dials and no
// reconnect line.
func TestStreamEndAfterExpiryEntersState(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		p := useCredDir(t, t.TempDir())
		now := time.Now()
		writeMaterial(t, p, timedMaterial(t, now.Add(-48*time.Hour), now.Add(time.Hour)))
		logs := captureSafeLog(t)

		var dials atomic.Int64
		connect := func(ctx context.Context) (bool, time.Duration) {
			dials.Add(1)
			// The stream stays up past the expiry, then ends.
			select {
			case <-ctx.Done():
				return true, 0
			case <-time.After(2 * time.Hour):
				return false, 2 * time.Hour
			}
		}
		b := commons.NewReconnectBackoff()
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() {
			defer close(done)
			superviseConnections(ctx, b, connect, waitOrDone, fileHold(p, time.Now, newExpiredReporter(testNID, nil), b))
		}()
		time.Sleep(5 * time.Hour)
		synctest.Wait()
		if dials.Load() != 1 || isClosed(done) {
			t.Fatalf("dials = %d, returned = %v; want 1 and running", dials.Load(), isClosed(done))
		}
		if n := len(logs.withMsg(t, reconnectLine)); n != 0 {
			t.Fatalf("%d reconnect lines, want 0", n)
		}
		if n := len(logs.withMsg(t, expiredMessage)); n < 1 {
			t.Fatal("no expired line: the state was not entered")
		}
		cancel()
		<-done
	})
}

// TestValidPairEndsStateWithoutRestart (criterion 5): a valid pair written
// through the store, a pair copied by hand (certificate first, key 3 s
// later) and a clock set back each give one dial within 10 seconds. The
// supervise function never returned before that dial.
func TestValidPairEndsStateWithoutRestart(t *testing.T) {
	type env struct {
		p      certstore.Paths
		offset *atomic.Int64
		logs   *safeLog
		dialAt chan time.Time
		done   chan struct{}
		cancel context.CancelFunc
	}
	setup := func(t *testing.T) *env {
		e := &env{offset: &atomic.Int64{}, dialAt: make(chan time.Time, 10), done: make(chan struct{})}
		e.p = useCredDir(t, t.TempDir())
		now := time.Now()
		writeMaterial(t, e.p, timedMaterial(t, now.Add(-48*time.Hour), now.Add(-time.Hour)))
		e.logs = captureSafeLog(t)
		clock := func() time.Time { return time.Now().Add(time.Duration(e.offset.Load())) }
		b := commons.NewReconnectBackoffWith(topOfRange)
		// A backoff at its cap shows that the end of the state resets it.
		for range 6 {
			b.AfterConnection(0)
		}
		connect := func(ctx context.Context) (bool, time.Duration) {
			e.dialAt <- time.Now()
			<-ctx.Done()
			return true, 0
		}
		var ctx context.Context
		ctx, e.cancel = context.WithCancel(context.Background())
		go func() {
			defer close(e.done)
			superviseConnections(ctx, b, connect, waitOrDone, fileHold(e.p, clock, newExpiredReporter(testNID, nil), b))
		}()
		time.Sleep(time.Minute)
		synctest.Wait()
		if len(e.dialAt) != 0 || isClosed(e.done) {
			t.Fatal("the agent dialed or returned with an expired pair")
		}
		return e
	}
	expectDialWithin := func(t *testing.T, e *env, from time.Time) {
		t.Helper()
		time.Sleep(10 * time.Second)
		synctest.Wait()
		if len(e.dialAt) != 1 {
			t.Fatalf("dials = %d within 10 s, want 1", len(e.dialAt))
		}
		if at := <-e.dialAt; at.Sub(from) > 10*time.Second {
			t.Fatalf("dial %s after the pair, want within 10 s", at.Sub(from))
		}
		if isClosed(e.done) {
			t.Fatal("the supervise function returned")
		}
		if n := len(e.logs.withMsg(t, validAgainMessage)); n != 1 {
			t.Fatalf("%d valid-again lines, want 1", n)
		}
		if n := len(e.logs.withMsg(t, reconnectLine)); n != 0 {
			t.Fatalf("%d reconnect lines, want 0", n)
		}
		e.cancel()
		<-e.done
	}

	t.Run("pair adopted through the store", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			e := setup(t)
			now := time.Now()
			s := certstore.Open(e.p)
			if err := s.Prepare(); err != nil {
				t.Fatal(err)
			}
			if err := s.Adopt(timedMaterial(t, now.Add(-time.Minute), now.Add(24*time.Hour))); err != nil {
				t.Fatal(err)
			}
			expectDialWithin(t, e, time.Now())
		})
	})

	t.Run("pair copied by hand, key 3 s after the certificate", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			e := setup(t)
			now := time.Now()
			m := timedMaterial(t, now.Add(-time.Minute), now.Add(24*time.Hour))
			if err := os.WriteFile(e.p.Cert, m.CertPEM, 0o644); err != nil {
				t.Fatal(err)
			}
			time.Sleep(3 * time.Second)
			synctest.Wait()
			if len(e.dialAt) != 0 {
				t.Fatal("a half-copied pair ended the state")
			}
			if err := os.WriteFile(e.p.CA, m.CAPEM, 0o644); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(e.p.Key, m.KeyPEM, 0o600); err != nil {
				t.Fatal(err)
			}
			expectDialWithin(t, e, time.Now())
		})
	})

	t.Run("clock set back", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			e := setup(t)
			e.offset.Store(int64(-3 * time.Hour))
			expectDialWithin(t, e, time.Now())
		})
	})
}

// TestExpiredStateResetsTheBackoff: the end of the state resets the reconnect
// backoff, so a connection lost right after it waits at most 1 s.
func TestExpiredStateResetsTheBackoff(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		p := useCredDir(t, t.TempDir())
		now := time.Now()
		writeMaterial(t, p, timedMaterial(t, now.Add(-48*time.Hour), now.Add(-time.Hour)))
		captureSafeLog(t)
		b := commons.NewReconnectBackoffWith(topOfRange)
		for range 6 {
			b.AfterConnection(0)
		}
		h := fileHold(p, time.Now, newExpiredReporter(testNID, nil), b)
		writeMaterial(t, p, timedMaterial(t, now.Add(-time.Minute), now.Add(24*time.Hour)))
		if h.hold(context.Background(), leafOf(t, timedMaterial(t, now.Add(-2*time.Hour), now.Add(-time.Hour)))) {
			t.Fatal("hold reported a shutdown")
		}
		if d := b.AfterConnection(0); d > time.Second {
			t.Fatalf("wait after the state = %s, want at most 1 s", d)
		}
	})
}

// TestExpiredStateLeavesProxyAlone (criterion 6): entry into the state and 3
// hours in it write no HAProxy configuration and reload nothing.
func TestExpiredStateLeavesProxyAlone(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		p := useCredDir(t, t.TempDir())
		now := time.Now()
		writeMaterial(t, p, timedMaterial(t, now.Add(-48*time.Hour), now.Add(-time.Hour)))
		captureSafeLog(t)
		before := agentstream.HAProxyChanges()
		stub := &renewStub{}
		done, stop := runAgentInBubble(t, agentDeps{
			connect: func(context.Context, *certrenew.Loop) (bool, time.Duration) {
				t.Error("the agent connected with an expired pair")
				return false, 0
			},
			renewal: stub.deps(t),
		})
		defer stop()
		time.Sleep(3 * time.Hour)
		synctest.Wait()
		if isClosed(done) {
			t.Fatal("the agent ended")
		}
		if got := agentstream.HAProxyChanges(); got != before {
			t.Fatalf("HAProxy writes and reloads = %d, want %d", got, before)
		}
	})
}

// TestStartWithExpiredPairReachesHoldState (criterion 10): through the start
// function, an expired pair that matches passes the registration check and
// reaches the hold state. The order is agent lock, store recovery,
// registration check.
func TestStartWithExpiredPairReachesHoldState(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		p := useCredDir(t, t.TempDir())
		useNID(t, testNID)
		now := time.Now()
		m := timedMaterial(t, now.Add(-48*time.Hour), now.Add(-time.Hour))
		writeMaterial(t, p, m)
		logs := captureSafeLog(t)

		var mu sync.Mutex
		var order []string
		record := func(s string) {
			mu.Lock()
			order = append(order, s)
			mu.Unlock()
		}
		saved := startSteps
		t.Cleanup(func() { startSteps = saved })
		startSteps.lock = func(path string) (*os.File, error) { record("lock"); return saved.lock(path) }
		startSteps.recover = func() { record("recover"); saved.recover() }
		startSteps.check = func() error { record("check"); return saved.check() }

		ctx, cancel := context.WithCancel(context.Background())
		var dials atomic.Int64
		stub := &renewStub{}
		var startErr error
		done := make(chan struct{})
		go func() {
			defer close(done)
			startErr = start(filepath.Join(t.TempDir(), "agent.lock"), func() {
				record("run")
				runAgentWith(ctx, agentDeps{
					connect: func(context.Context, *certrenew.Loop) (bool, time.Duration) {
						dials.Add(1)
						return false, 0
					},
					renewal: stub.deps(t),
				})
			})
		}()
		time.Sleep(time.Minute)
		synctest.Wait()
		mu.Lock()
		got := strings.Join(order, ",")
		mu.Unlock()
		if got != "lock,recover,check,run" {
			t.Fatalf("start order = %s, want lock,recover,check,run", got)
		}
		lines := logs.withMsg(t, expiredMessage)
		if isClosed(done) || dials.Load() != 0 || len(lines) != 1 {
			t.Fatalf("exited=%v dials=%d expired lines=%d; want the hold state", isClosed(done), dials.Load(), len(lines))
		}
		checkExpiredLine(t, lines[0], leafOf(t, m))
		cancel()
		<-done
		if startErr != nil {
			t.Fatalf("start: %v", startErr)
		}
	})
}

// openStream is a fake L2Sec instruction stream that stays open. It answers
// the control-plane request with an empty list.
type openStream struct {
	grpc.ClientStream
	recv   chan *l2sec.ToNodeAgent
	closed chan struct{}
	once   sync.Once

	cpRequests atomic.Int64
	closeSends atomic.Int64
}

func newOpenStream() *openStream {
	return &openStream{recv: make(chan *l2sec.ToNodeAgent, 16), closed: make(chan struct{})}
}

func (s *openStream) Send(m *l2sec.FromNodeAgent) error {
	if m.Type == "GetControlPlaneNodes" {
		s.cpRequests.Add(1)
		select {
		case s.recv <- &l2sec.ToNodeAgent{Tag: m.Tag, JsonB64: base64.StdEncoding.EncodeToString([]byte("[]"))}:
		case <-s.closed:
		}
	}
	return nil
}

func (s *openStream) Recv() (*l2sec.ToNodeAgent, error) {
	select {
	case m := <-s.recv:
		return m, nil
	case <-s.closed:
		return nil, io.EOF
	}
}

func (s *openStream) CloseSend() error {
	s.closeSends.Add(1)
	return nil
}

func (s *openStream) Context() context.Context { return context.Background() }

func (s *openStream) end() { s.once.Do(func() { close(s.closed) }) }

// streamClient is a fake Nodeward client whose stream is s.
type streamClient struct {
	l2sec.NodewardClient
	s *openStream
}

func (c streamClient) NodeAgentStream(context.Context, ...grpc.CallOption) (grpc.BidiStreamingClient[l2sec.FromNodeAgent, l2sec.ToNodeAgent], error) {
	return c.s, nil
}

// TestQuietExpiryWithOpenStream (criterion 8): a leaf expires while the
// stream stays open. The real connection code runs over a fake stream: the
// renewal loop, the reporter, the control-plane poll of the HAProxy service,
// the VPN resync ticker and the lifecycle node log. The VPN sync and the node
// log run their real L2Sec dial once the leaf expired (before it they report
// success, because this test has no Nodeward and no wg0). Over 3 hours: one
// ERROR line at expiry and at most 3 more, no other WARN or ERROR line, no
// dial and no close of the stream by the agent.
func TestQuietExpiryWithOpenStream(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		p := useCredDir(t, t.TempDir())
		useNID(t, testNID)
		now := time.Now()
		m := timedMaterial(t, now.Add(-2*time.Hour), now.Add(30*time.Second))
		writeMaterial(t, p, m)
		leaf := leafOf(t, m)
		logs := captureSafeLog(t)

		var expired atomic.Bool
		var syncCalls, logCalls atomic.Int64
		stream := newOpenStream()
		var connects atomic.Int64
		savedDial, savedSync, savedLog, savedSvc := dialNodeward, vpnSync, addNodelog, connServices
		t.Cleanup(func() {
			dialNodeward, vpnSync, addNodelog, connServices = savedDial, savedSync, savedLog, savedSvc
		})
		dialNodeward = func() (l2sec.NodewardClient, func(), error) {
			connects.Add(1)
			return streamClient{s: stream}, func() {}, nil
		}
		vpnSync = func() (int, error) {
			if !expired.Load() {
				return 0, nil
			}
			syncCalls.Add(1)
			_, _, _, conn, err := backend.NodewardL2Sec()
			if conn != nil {
				conn.Close()
			}
			return 0, err
		}
		addNodelog = func(sev int, typ, msg string) error {
			if !expired.Load() {
				return nil
			}
			logCalls.Add(1)
			return backend.AddNodelog(sev, typ, msg)
		}
		idle := func(ctx context.Context) chan struct{} {
			ch := make(chan struct{})
			go func() { <-ctx.Done(); close(ch) }()
			return ch
		}
		connServices.heartbeat = idle
		connServices.patchFacts = idle
		// connServices.haproxy stays the real service: its control-plane
		// poll asks over the stream every 5 s.

		stub := &renewStub{}
		deps := stub.deps(t)
		deps.Renew = func(context.Context, certrenew.Request) (certificate.Result, error) {
			stub.calls.Add(1)
			return certificate.Result{Status: certificate.StatusAlreadyRenewed, Serial: leaf.Serial, NotAfter: leaf.NotAfter}, nil
		}
		dialsBefore := backend.DialAttempts()
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() {
			defer close(done)
			runAgentWith(ctx, agentDeps{renewal: deps})
		}()
		defer func() {
			cancel()
			stream.end()
			<-done
		}()

		time.Sleep(31 * time.Second)
		synctest.Wait()
		expired.Store(true)
		// Before the expiry the renewal loop writes its late warning; count
		// only what comes after the expiry.
		loudAtExpiry := len(logs.loud(t))
		renewsAtExpiry := stub.calls.Load()
		cpAtExpiry := stream.cpRequests.Load()
		if n := len(logs.withMsg(t, expiredMessage)); n != 1 {
			t.Fatalf("expired lines at expiry = %d, want 1", n)
		}

		time.Sleep(3 * time.Hour)
		synctest.Wait()
		lines := logs.withMsg(t, expiredMessage)
		if len(lines) < 1 || len(lines) > 4 {
			t.Fatalf("expired lines = %d, want 1 at expiry and at most 3 more", len(lines))
		}
		checkExpiredLine(t, lines[0], leaf)
		if loud := logs.loud(t); len(loud) != loudAtExpiry {
			t.Fatalf("WARN or ERROR lines after the expiry other than the expired line: %v", loud[loudAtExpiry:])
		}
		for _, msg := range []string{reconnectLine, "nodeward L2", "client cert", "Error connecting"} {
			if n := len(logs.withMsg(t, msg)); n != 0 {
				t.Fatalf("%d lines with %q", n, msg)
			}
		}
		if got := backend.DialAttempts(); got != dialsBefore {
			t.Fatalf("L2Sec dials = %d, want 0", got-dialsBefore)
		}
		if syncCalls.Load() < 100 {
			t.Fatalf("the VPN resync ran %d times after expiry, want one per minute", syncCalls.Load())
		}
		if stream.cpRequests.Load()-cpAtExpiry < 1000 {
			t.Fatalf("the control-plane poll ran %d times after expiry", stream.cpRequests.Load()-cpAtExpiry)
		}
		if stub.calls.Load() != renewsAtExpiry {
			t.Fatal("the renewal loop sent a request for an expired leaf")
		}
		if connects.Load() != 1 || isClosed(done) || stream.closeSends.Load() != 0 {
			t.Fatalf("connects=%d returned=%v closeSends=%d; want the stream open", connects.Load(), isClosed(done), stream.closeSends.Load())
		}

		cancel()
		stream.end()
		<-done
		if logCalls.Load() != 1 {
			t.Fatalf("lifecycle node log calls after expiry = %d, want 1 (Agent stopped)", logCalls.Load())
		}
		if loud := logs.loud(t); len(loud) != loudAtExpiry {
			t.Fatalf("the shutdown wrote WARN or ERROR lines: %v", loud[loudAtExpiry:])
		}
	})
}
