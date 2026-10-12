package agent

import (
	"context"
	"testing"
	"time"

	"github.com/runos-official/nodeagent/commons"
)

// FPL315 phase 8: every reconnect of the L2SEC stream waits a jittered,
// capped, exponential delay, and a connection that stayed up 60 s resets it.

type fakeConnection struct {
	upFor    time.Duration
	shutdown bool
}

func runSupervisor(t *testing.T, conns []fakeConnection) []time.Duration {
	t.Helper()
	return runSupervisorWith(t, commons.NewReconnectBackoff(), conns)
}

func runSupervisorWith(t *testing.T, b *commons.Backoff, conns []fakeConnection) []time.Duration {
	t.Helper()
	var waits []time.Duration
	i := 0
	connect := func(context.Context) (bool, time.Duration) {
		c := conns[i]
		i++
		return c.shutdown, c.upFor
	}
	wait := func(_ context.Context, d time.Duration) { waits = append(waits, d) }
	superviseConnections(context.Background(), b, connect, wait)
	if i != len(conns) {
		t.Fatalf("supervisor made %d connections, want %d", i, len(conns))
	}
	return waits
}

func TestSuperviseConnectionsWaitsWithinTheJitteredCaps(t *testing.T) {
	conns := make([]fakeConnection, 8)
	conns = append(conns, fakeConnection{shutdown: true})
	caps := []time.Duration{1, 2, 4, 8, 16, 30, 30, 30}
	for run := 0; run < 50; run++ {
		waits := runSupervisor(t, conns)
		if len(waits) != len(caps) {
			t.Fatalf("got %d waits, want %d", len(waits), len(caps))
		}
		for i, w := range waits {
			if w < 0 || w > caps[i]*time.Second {
				t.Fatalf("wait %d = %s, outside [0, %s]", i+1, w, caps[i]*time.Second)
			}
		}
	}
}

func TestSuperviseConnectionsResetsAfterAStableConnection(t *testing.T) {
	// Five quick failures push the cap to 30 s. One connection that stays up
	// 60 s must bring the next wait back under 1 s.
	conns := []fakeConnection{{}, {}, {}, {}, {}, {upFor: 60 * time.Second}, {shutdown: true}}
	for run := 0; run < 50; run++ {
		waits := runSupervisor(t, conns)
		if last := waits[len(waits)-1]; last > time.Second {
			t.Fatalf("wait after a stable connection = %s, want at most 1 s", last)
		}
	}
}

func TestSuperviseConnectionsStopsOnShutdown(t *testing.T) {
	if waits := runSupervisor(t, []fakeConnection{{shutdown: true}}); len(waits) != 0 {
		t.Fatalf("shutdown must not wait, got %v", waits)
	}
}

func topOfRange(n int64) int64 { return n - 1 }

func secondsOf(ds []time.Duration) []time.Duration {
	out := make([]time.Duration, len(ds))
	for i, d := range ds {
		out[i] = d / time.Second
	}
	return out
}

// With every draw at the top of its range the waits show the cap itself, so a
// supervisor that rebuilt or reset its backoff each loop would show 1 s
// forever instead of the doubling cap.
func TestSuperviseConnectionsDoublesTheCapUpToThirtySeconds(t *testing.T) {
	conns := append(make([]fakeConnection, 7), fakeConnection{shutdown: true})
	waits := runSupervisorWith(t, commons.NewReconnectBackoffWith(topOfRange), conns)
	want := []time.Duration{1, 2, 4, 8, 16, 30, 30}
	got := secondsOf(waits)
	if len(got) != len(want) {
		t.Fatalf("waits = %v s, want %v s", got, want)
	}
	for i := range want {
		if got[i] != want[i] || waits[i]%time.Second != 0 {
			t.Fatalf("waits = %v, want %v s", waits, want)
		}
	}
}

func TestSuperviseConnectionsKeepsTheCapAfterAShortConnection(t *testing.T) {
	// Five quick failures push the cap to 30 s. A connection up 59 s is not
	// stable, so the cap stays; one up 60 s resets it to 1 s, and the next
	// failure doubles it again.
	conns := []fakeConnection{{}, {}, {}, {}, {}, {upFor: 59 * time.Second}, {upFor: 60 * time.Second}, {}, {shutdown: true}}
	waits := runSupervisorWith(t, commons.NewReconnectBackoffWith(topOfRange), conns)
	want := []time.Duration{1, 2, 4, 8, 16, 30, 1, 2}
	got := secondsOf(waits)
	if len(got) != len(want) {
		t.Fatalf("waits = %v s, want %v s", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("waits = %v s, want %v s", got, want)
		}
	}
}
