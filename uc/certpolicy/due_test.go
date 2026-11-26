package certpolicy

import (
	"fmt"
	"testing"
	"time"
)

const day = 24 * time.Hour

var testLives = []time.Duration{15 * time.Minute, time.Hour, 30 * day, 90 * day, 180 * day}

// nodeIDs makes n generated node ids. They are not real ids.
func nodeIDs(n int) []string {
	ids := make([]string, n)
	for i := range ids {
		ids[i] = fmt.Sprintf("00000000-0000-4000-8000-%012x", i*7919+1)
	}
	return ids
}

func TestDuePointRange(t *testing.T) {
	notBefore := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
	for _, life := range testLives {
		notAfter := notBefore.Add(life)
		for _, id := range nodeIDs(1000) {
			tm := Evaluate(notBefore, notAfter, id, notBefore)
			lead := notAfter.Sub(tm.DueAt)
			if lead < life/4 || lead > life/3 {
				t.Fatalf("life %v node %s: due %v before NotAfter, want between %v and %v", life, id, lead, life/4, life/3)
			}
			if tm.Jitter < 0 || tm.Jitter > life/12 {
				t.Fatalf("life %v: jitter %v", life, tm.Jitter)
			}
			if tm.Life != life {
				t.Fatalf("life %v, got %v", life, tm.Life)
			}
		}
	}
}

func TestJitterPerNode(t *testing.T) {
	notBefore := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
	notAfter := notBefore.Add(90 * day)
	ids := nodeIDs(1000)
	seen := map[time.Duration]int{}
	for _, id := range ids {
		a := Evaluate(notBefore, notAfter, id, notBefore).Jitter
		b := Evaluate(notBefore, notAfter, id, notBefore.Add(40*day)).Jitter
		if a != b {
			t.Fatalf("node %s: jitter %v then %v", id, a, b)
		}
		seen[a]++
	}
	if Evaluate(notBefore, notAfter, ids[0], notBefore).Jitter == Evaluate(notBefore, notAfter, ids[1], notBefore).Jitter {
		t.Fatal("two node ids give one jitter")
	}
	// A spread function, not a constant: nearly every id differs.
	if len(seen) < 990 {
		t.Fatalf("only %d distinct jitters for 1000 ids", len(seen))
	}
	// An empty node id is valid and fixed.
	if Evaluate(notBefore, notAfter, "", notBefore).Jitter != Evaluate(notBefore, notAfter, "", notAfter).Jitter {
		t.Fatal("empty node id gives two jitters")
	}
}

func TestNinetyDayDueInsideDeployedWindow(t *testing.T) {
	notBefore := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
	notAfter := notBefore.Add(90 * day)
	for _, id := range nodeIDs(1000) {
		tm := Evaluate(notBefore, notAfter, id, notBefore)
		if notAfter.Sub(tm.DueAt) > 32*day {
			t.Fatalf("node %s: due %v before NotAfter, earlier than the 32-day window", id, notAfter.Sub(tm.DueAt))
		}
	}
}

func TestDueAndExpiredFlags(t *testing.T) {
	notBefore := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
	notAfter := notBefore.Add(90 * day)
	id := "00000000-0000-4000-8000-000000000001"
	due := Evaluate(notBefore, notAfter, id, notBefore).DueAt

	before := Evaluate(notBefore, notAfter, id, due.Add(-time.Second))
	if before.Due || before.Expired {
		t.Fatalf("one second before the due point: %+v", before)
	}
	at := Evaluate(notBefore, notAfter, id, due)
	if !at.Due || at.Expired {
		t.Fatalf("at the due point: %+v", at)
	}
	if at.Remaining != notAfter.Sub(due) {
		t.Fatalf("remaining %v", at.Remaining)
	}
	exp := Evaluate(notBefore, notAfter, id, notAfter)
	if !exp.Expired || !exp.Due || exp.Remaining != 0 {
		t.Fatalf("at NotAfter: %+v", exp)
	}
	late := Evaluate(notBefore, notAfter, id, notAfter.Add(time.Hour))
	if late.Remaining != 0 {
		t.Fatalf("remaining after expiry must be 0: %v", late.Remaining)
	}
}

func TestMalformedLifeIsDue(t *testing.T) {
	ts := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
	for _, notAfter := range []time.Time{ts, ts.Add(-time.Hour)} {
		tm := Evaluate(ts, notAfter, "x", ts.Add(-day))
		if !tm.Due || tm.Jitter != 0 {
			t.Fatalf("life %v: %+v", notAfter.Sub(ts), tm)
		}
	}
}

func TestShortCertificateDuePoint(t *testing.T) {
	notBefore := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
	notAfter := notBefore.Add(15 * time.Minute)
	for _, id := range nodeIDs(200) {
		lead := notAfter.Sub(Evaluate(notBefore, notAfter, id, notBefore).DueAt)
		if lead < 3*time.Minute+45*time.Second || lead > 5*time.Minute {
			t.Fatalf("15-minute life: due %v before NotAfter", lead)
		}
	}
}
