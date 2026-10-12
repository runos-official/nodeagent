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
	var waits []time.Duration
	i := 0
	connect := func(context.Context) (bool, time.Duration) {
		c := conns[i]
		i++
		return c.shutdown, c.upFor
	}
	wait := func(_ context.Context, d time.Duration) { waits = append(waits, d) }
	superviseConnections(context.Background(), commons.NewReconnectBackoff(), connect, wait)
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
