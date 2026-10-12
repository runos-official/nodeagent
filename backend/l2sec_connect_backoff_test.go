package backend

import (
	"net"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// Most callers of NodewardL2Sec dial once and never retry a failed dial
// (install, UpdateStatus, AddNodelog, sync, certificate renew, status). So
// grpc-go's reconnect schedule inside one dial must keep making attempts
// through the whole l2secDialTimeout window, or a nodeward restart of about
// 10 s fails a dial that would have ridden it out. Fleet spreading belongs to
// the supervised reconnect loop (commons.Backoff), not here (FPL315 review).

// connectGap returns the shortest and longest wait grpc-go can choose after
// failed attempt n: retry 0 waits the base delay, later retries wait
// min(base*mult^n, max) * (1 + jitter*(2r-1)) with r in [0, 1).
func connectGap(n int) (lo, hi time.Duration) {
	b := l2secConnectBackoff
	if n == 0 {
		return b.BaseDelay, b.BaseDelay
	}
	d, max := float64(b.BaseDelay), float64(b.MaxDelay)
	for d < max && n > 0 {
		d *= b.Multiplier
		n--
	}
	if d > max {
		d = max
	}
	return time.Duration(d * (1 - b.Jitter)), time.Duration(d * (1 + b.Jitter))
}

// latestAttemptAfter bounds when the first connect attempt at or after outage
// starts, whatever the jitter draws: some attempt n starts before the outage
// ends, and the one after it starts at most its longest gap later. Attempt n
// cannot start before the sum of the shortest gaps before it. With full jitter
// the shortest gap is 0, so the walk stops at a fixed number of attempts.
func latestAttemptAfter(outage time.Duration) time.Duration {
	var earliest, worst time.Duration
	for n := 0; earliest < outage && n < 100; n++ {
		lo, hi := connectGap(n)
		if outage+hi > worst {
			worst = outage + hi
		}
		earliest += lo
	}
	return worst
}

func TestL2SecConnectBackoffFitsTheDialWindow(t *testing.T) {
	const outage = 10 * time.Second
	// Leave room for the TCP, TLS and HTTP/2 handshake after the attempt starts.
	const handshake = time.Second
	if got := latestAttemptAfter(outage); got+handshake > l2secDialTimeout {
		t.Fatalf("after a %s outage the next connect attempt can start at %s, "+
			"too late for the %s dial window", outage, got.Round(10*time.Millisecond), l2secDialTimeout)
	}
}

// A dial against a listener that only comes up after 10 s must connect within
// l2secDialTimeout.
func TestL2SecDialRidesThroughATenSecondOutage(t *testing.T) {
	if testing.Short() {
		t.Skip("waits 10 s for the listener")
	}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	l.Close() // refuse connections until the server comes up

	srv := grpc.NewServer()
	t.Cleanup(srv.Stop)
	up := make(chan error, 1)
	time.AfterFunc(10*time.Second, func() {
		l, err := net.Listen("tcp", addr)
		up <- err
		if err == nil {
			go srv.Serve(l)
		}
	})

	start := time.Now()
	conn, err := dialL2Sec(addr, insecure.NewCredentials())
	if err != nil {
		if lerr := <-up; lerr != nil {
			t.Skipf("could not listen on %s again: %v", addr, lerr)
		}
		t.Fatalf("dial failed after %s: %v", time.Since(start).Round(time.Second), err)
	}
	conn.Close()
	if took := time.Since(start); took < 10*time.Second {
		t.Fatalf("dial returned after %s, before the listener came up", took)
	}
}
