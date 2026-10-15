package agent

import (
	"errors"
	"os"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

// SIGHUP (sent by logrotate through systemctl reload) must reopen the log file
// and must not end the agent. SIGTERM must still end it.
func TestHandleSignals_HupReopensLogAndKeepsRunning(t *testing.T) {
	sigs := make(chan os.Signal, 4)
	var reopens, cancels atomic.Int32
	done := make(chan struct{})
	go func() {
		handleSignals(sigs,
			func() error { reopens.Add(1); return nil },
			func() { cancels.Add(1) })
		close(done)
	}()

	sigs <- syscall.SIGHUP
	sigs <- syscall.SIGHUP
	waitFor(t, func() bool { return reopens.Load() == 2 })
	if cancels.Load() != 0 {
		t.Fatal("SIGHUP ended the agent, want it to keep running")
	}
	select {
	case <-done:
		t.Fatal("signal loop returned after SIGHUP")
	default:
	}

	sigs <- syscall.SIGTERM
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("SIGTERM did not end the signal loop")
	}
	if cancels.Load() != 1 {
		t.Fatalf("cancel calls = %d, want 1", cancels.Load())
	}
}

func TestHandleSignals_ReopenErrorDoesNotEndAgent(t *testing.T) {
	sigs := make(chan os.Signal, 2)
	var reopens, cancels atomic.Int32
	go handleSignals(sigs,
		func() error { reopens.Add(1); return errors.New("boom") },
		func() { cancels.Add(1) })
	sigs <- syscall.SIGHUP
	waitFor(t, func() bool { return reopens.Load() == 1 })
	if cancels.Load() != 0 {
		t.Fatal("a failed reopen ended the agent")
	}
	sigs <- syscall.SIGINT
	waitFor(t, func() bool { return cancels.Load() == 1 })
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("condition not met in time")
}
