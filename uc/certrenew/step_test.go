package certrenew

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/runos-official/nodeagent/uc/certpolicy"
)

func jsonUnmarshal(b []byte, v any) error { return json.Unmarshal(b, v) }

func TestDecide(t *testing.T) {
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	life := 90 * 24 * time.Hour
	nb, na := t0, t0.Add(life)
	timing := func(now time.Time) certpolicy.Timing { return certpolicy.Evaluate(nb, na, testNodeID, now) }
	due := timing(t0).DueAt

	cases := []struct {
		name string
		in   input
		want decision
	}{
		{"not due sleeps to the due point", input{Now: t0, Timing: timing(t0)},
			decision{Action: actSleep, At: due, NextAttemptAt: due}},
		{"expired waits 6 hours", input{Now: na, Timing: timing(na)},
			decision{Action: actExpired, At: na.Add(expiredWake), NextAttemptAt: na.Add(expiredWake)}},
		{"retry time in the future", input{Now: due, Timing: timing(due), RetryAt: due.Add(time.Hour)},
			decision{Action: actSleep, At: due.Add(time.Hour), NextAttemptAt: due.Add(time.Hour)}},
		{"retry time past NotAfter is capped", input{Now: na.Add(-time.Minute), Timing: timing(na.Add(-time.Minute)), RetryAt: na.Add(time.Hour)},
			decision{Action: actSleep, At: na, NextAttemptAt: na.Add(time.Hour)}},
		{"due, no stream, waits 2 minutes", input{Now: due, Timing: timing(due)},
			decision{Action: actWaitStream, At: due.Add(noStreamLimit), ReadyAt: due, NextAttemptAt: due}},
		{"due, stream just up, waits the settle", input{Now: due, Timing: timing(due), Connected: true, ConnectedAt: due.Add(-time.Second)},
			decision{Action: actWaitStream, At: due.Add(settle - time.Second), ReadyAt: due, NextAttemptAt: due}},
		{"due, stream settled", input{Now: due, Timing: timing(due), Connected: true, ConnectedAt: due.Add(-settle)},
			decision{Action: actAttempt, NextAttemptAt: due}},
		{"due, no stream for 2 minutes", input{Now: due.Add(noStreamLimit), Timing: timing(due.Add(noStreamLimit)), ReadyAt: due},
			decision{Action: actAttempt, NextAttemptAt: due}},
		{"retry reached, stream settled", input{Now: due.Add(time.Hour), Timing: timing(due.Add(time.Hour)), RetryAt: due.Add(time.Hour), Connected: true},
			decision{Action: actAttempt, NextAttemptAt: due.Add(time.Hour)}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := decide(tc.in); got != tc.want {
				t.Fatalf("decide = %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestWriteStateAtomicAndCut(t *testing.T) {
	path := filepath.Join(t.TempDir(), "run", "runos", "certificate-renewal.json")
	long := strings.Repeat("é", 400)
	if err := writeState(path, State{Version: 1, LastResult: ResultRefused, LastErrorMessage: long}); err != nil {
		t.Fatal(err)
	}
	st, err := ReadState(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(st.LastErrorMessage) > maxErrorMessage || !strings.HasPrefix(long, st.LastErrorMessage) {
		t.Fatalf("message not cut on a rune boundary: %d bytes", len(st.LastErrorMessage))
	}
	ents, _ := os.ReadDir(filepath.Dir(path))
	if len(ents) != 1 {
		t.Fatalf("temporary files stay: %v", ents)
	}
	info, _ := os.Stat(path)
	if info.Mode().Perm() != 0o644 {
		t.Fatalf("mode %v", info.Mode().Perm())
	}
}

func TestNilLoopDoesNothing(t *testing.T) {
	var l *Loop
	l.Start(t.Context())
	l.NotifyConnected()
	l.NotifyDisconnected()
	if !l.Wait(time.Second) {
		t.Fatal("Wait on a nil loop")
	}
}
