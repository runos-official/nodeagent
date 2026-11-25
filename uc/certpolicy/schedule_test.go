package certpolicy

import (
	"testing"
	"time"
)

func fixed(v float64) func() float64 { return func() float64 { return v } }

func TestTransientBackoffBounds(t *testing.T) {
	now := time.Date(2030, 6, 1, 0, 0, 0, 0, time.UTC)
	remainings := []time.Duration{time.Second, 10 * time.Second, time.Minute, 7 * time.Minute, time.Hour, 5 * time.Hour, day, 30 * day, 90 * day}
	rands := []float64{0, 0.5, 0.999999999}
	o := Outcome{Kind: Transient}
	for _, rem := range remainings {
		notAfter := now.Add(rem)
		tm := Evaluate(notAfter.Add(-90*day), notAfter, "n", now)
		for f := 1; f <= 40; f++ {
			for _, r := range rands {
				p := Next(o, tm, f, now, fixed(r))
				d := p.At.Sub(now)
				if d > time.Hour {
					t.Fatalf("rem %v f %d r %v: delay %v over 1 hour", rem, f, r, d)
				}
				if d < 0 {
					t.Fatalf("rem %v f %d r %v: delay %v in the past", rem, f, r, d)
				}
				if !p.At.Before(notAfter) {
					t.Fatalf("rem %v f %d r %v: attempt at %v not before NotAfter %v", rem, f, r, p.At, notAfter)
				}
				if p.OnFileChange {
					t.Fatalf("Transient must not wait on a file change")
				}
			}
		}
	}
}

func TestTransientBackoffSteps(t *testing.T) {
	now := time.Date(2030, 6, 1, 0, 0, 0, 0, time.UTC)
	notAfter := now.Add(60 * day)
	tm := Evaluate(notAfter.Add(-90*day), notAfter, "n", now)
	o := Outcome{Kind: Transient}
	want := []time.Duration{time.Minute, 2 * time.Minute, 4 * time.Minute, 8 * time.Minute, 16 * time.Minute, 32 * time.Minute, time.Hour, time.Hour}
	for i, step := range want {
		f := i + 1
		lo := Next(o, tm, f, now, fixed(0)).At.Sub(now)
		if lo != step/2 {
			t.Errorf("failure %d: lower delay %v, want %v", f, lo, step/2)
		}
	}
	// Failures below 1 count as 1.
	if Next(o, tm, 0, now, fixed(0)).At.Sub(now) != 30*time.Second || Next(o, tm, -3, now, fixed(0)).At.Sub(now) != 30*time.Second {
		t.Error("failures below 1 must count as 1")
	}
	// Huge failure counts do not overflow.
	if d := Next(o, tm, 1<<30, now, fixed(0.999)).At.Sub(now); d > time.Hour || d < 30*time.Minute {
		t.Errorf("huge failure count: delay %v", d)
	}
	// A short remaining time caps the step at remaining/10.
	short := Evaluate(now.Add(-10*time.Minute), now.Add(5*time.Minute), "n", now)
	if d := Next(o, short, 1, now, fixed(0.999999)).At.Sub(now); d > 30*time.Second {
		t.Errorf("5 minutes left: delay %v over remaining/10", d)
	}
	// A nil random source is valid.
	if d := Next(o, tm, 1, now, nil).At.Sub(now); d < 30*time.Second || d > time.Minute {
		t.Errorf("nil rnd: delay %v", d)
	}
}

func TestWaitUntilSchedule(t *testing.T) {
	now := time.Date(2030, 6, 1, 0, 0, 0, 0, time.UTC)
	notAfter := now.Add(31 * day)
	tm := Evaluate(notAfter.Add(-90*day), notAfter, "n", now)
	retryAt := now.Add(day)
	o := Outcome{Kind: WaitUntil, RetryAt: retryAt}

	if p := Next(o, tm, 3, now, fixed(0)); !p.At.Equal(retryAt) || p.OnFileChange {
		t.Errorf("rnd 0: %+v", p)
	}
	if d := Next(o, tm, 3, now, fixed(0.999999)).At.Sub(retryAt); d > 5*time.Minute || d < 4*time.Minute {
		t.Errorf("spread %v, want up to 5 minutes", d)
	}
	// A short life caps the spread at life/12.
	shortNA := now.Add(10 * time.Minute)
	short := Evaluate(shortNA.Add(-12*time.Minute), shortNA, "n", now)
	so := Outcome{Kind: WaitUntil, RetryAt: now.Add(2 * time.Minute)}
	if d := Next(so, short, 1, now, fixed(0.999999)).At.Sub(so.RetryAt); d > time.Minute {
		t.Errorf("12-minute life: spread %v over life/12", d)
	}
	// A spread that would pass NotAfter is dropped.
	edge := Outcome{Kind: WaitUntil, RetryAt: notAfter.Add(-time.Second)}
	if p := Next(edge, tm, 1, now, fixed(0.9)); !p.At.Equal(edge.RetryAt) {
		t.Errorf("clamp: %v, want %v", p.At, edge.RetryAt)
	}
	// A time in the past starts from now.
	past := Outcome{Kind: WaitUntil, RetryAt: now.Add(-time.Hour)}
	if p := Next(past, tm, 1, now, fixed(0)); !p.At.Equal(now) {
		t.Errorf("past time: %v, want now", p.At)
	}
}

func TestNonRetryWaitsSixHours(t *testing.T) {
	now := time.Date(2030, 6, 1, 0, 0, 0, 0, time.UTC)
	notAfter := now.Add(20 * day)
	tm := Evaluate(notAfter.Add(-90*day), notAfter, "n", now)
	for _, k := range AllKinds() {
		if k.Retries() {
			continue
		}
		p := Next(Outcome{Kind: k}, tm, 1, now, fixed(0.5))
		if !p.At.Equal(now.Add(6*time.Hour)) || !p.OnFileChange {
			t.Errorf("%s: %+v, want now+6h and OnFileChange", k, p)
		}
	}
	// An expired certificate waits six hours for every kind.
	exp := Evaluate(now.Add(-90*day), now.Add(-time.Second), "n", now)
	for _, k := range AllKinds() {
		o := Outcome{Kind: k, RetryAt: now.Add(time.Minute)}
		p := Next(o, exp, 1, now, fixed(0.5))
		if !p.At.Equal(now.Add(6*time.Hour)) || !p.OnFileChange {
			t.Errorf("expired, %s: %+v, want now+6h and OnFileChange", k, p)
		}
	}
	// The timing may be older than now: NotAfter passed since Evaluate ran.
	stale := Evaluate(now.Add(-90*day), now.Add(time.Minute), "n", now)
	p := Next(Outcome{Kind: Transient}, stale, 1, now.Add(2*time.Minute), fixed(0.5))
	if !p.OnFileChange || !p.At.Equal(now.Add(2*time.Minute+6*time.Hour)) {
		t.Errorf("NotAfter passed since Evaluate: %+v", p)
	}
}
