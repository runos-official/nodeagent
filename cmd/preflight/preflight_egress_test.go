package preflight

import (
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

// Goal 23 review, F4-a / F28-a / F28-c. The egress check probed first and measured DNS only
// after a failure, so a resolver that was slow once and cached by the time it was measured read
// as "resolved in 1ms then timed out connecting (firewall)". It also probed nine hosts one after
// another, three attempts each, so a fully blocked host took four to six minutes to report. And
// after three transport-error retries the summary still sent the operator to a firewall team
// with no way to tell a preflight defect apart from a real block.

// fakeEgress stands in for the network: it records the order of calls per host and answers from
// a table.
type fakeEgress struct {
	mu       sync.Mutex
	order    map[string][]string
	probeErr map[string]error
	slow     map[string]time.Duration
	delay    time.Duration
}

func (f *fakeEgress) note(host, what string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.order[host] = append(f.order[host], what)
}

func (f *fakeEgress) install(t *testing.T) {
	t.Helper()
	origProbe, origResolve := netProbeHTTPSFn, netMeasureResolveFn
	netProbeHTTPSFn = func(host, path string) (int, error) {
		f.note(host, "probe")
		time.Sleep(f.delay)
		if err, ok := f.probeErr[host]; ok {
			return 0, err
		}
		return 200, nil
	}
	netMeasureResolveFn = func(host string) (bool, string, time.Duration) {
		f.note(host, "resolve")
		time.Sleep(f.delay)
		if d, ok := f.slow[host]; ok {
			return true, "", d
		}
		return true, "", time.Millisecond
	}
	t.Cleanup(func() { netProbeHTTPSFn, netMeasureResolveFn = origProbe, origResolve })
}

func TestEgressCheckMeasuresDNSBeforeProbingAndRunsConcurrently(t *testing.T) {
	timeout := errors.New(`Get "https://x/": context deadline exceeded (Client.Timeout exceeded)`)
	f := &fakeEgress{
		order:    map[string][]string{},
		probeErr: map[string]error{},
		slow:     map[string]time.Duration{},
		delay:    150 * time.Millisecond,
	}
	targets := netEgressTargets()
	for _, tg := range targets {
		f.probeErr[tg.host] = timeout
		f.slow[tg.host] = 5 * time.Second // the dead-resolver penalty, measured before the probe
	}
	f.install(t)

	started := time.Now()
	err := checkEgressEndpointSetComplete()
	took := time.Since(started)
	if err == nil {
		t.Fatal("every probe timed out; want a finding")
	}

	// F28-c: nine resolves then nine probes, each 150ms; sequential would be ~2.7s.
	if took > 4*f.delay+time.Second {
		t.Errorf("check took %v; targets are probed sequentially, want concurrent (about %v)", took, 2*f.delay)
	}
	// F4-a: resolution is measured BEFORE the probe for every host.
	for _, tg := range targets {
		got := f.order[tg.host]
		if len(got) != 2 || got[0] != "resolve" || got[1] != "probe" {
			t.Errorf("%s: call order %v, want [resolve probe]", tg.host, got)
		}
	}
	// And the classification uses that pre-measured value: a 5s lookup is a DNS verdict, never
	// "resolved in 1ms then timed out connecting".
	if strings.Contains(err.Error(), "likely firewall/proxy block") {
		t.Errorf("a 5s lookup measured before the probe must read as DNS, got:\n%s", err)
	}
	if !strings.Contains(err.Error(), "NAME RESOLUTION") {
		t.Errorf("want the all-DNS summary, got:\n%s", err)
	}
	// Results stay in target order.
	first := strings.Index(err.Error(), targets[0].host)
	last := strings.Index(err.Error(), targets[len(targets)-1].host)
	if first < 0 || last < 0 || first > last {
		t.Errorf("findings are not in target order:\n%s", err)
	}
}

func TestEgressCheckNamesTheSkipFlagWhenItMightBeWrong(t *testing.T) {
	timeout := errors.New(`Get "https://x/": context deadline exceeded (Client.Timeout exceeded)`)
	f := &fakeEgress{
		order:    map[string][]string{},
		probeErr: map[string]error{"github.com": timeout},
		slow:     map[string]time.Duration{},
	}
	f.install(t)

	err := checkEgressEndpointSetComplete()
	if err == nil {
		t.Fatal("github.com timed out; want a finding")
	}
	msg := err.Error()
	// F28-a: the operator can tell a preflight defect from a real block, and knows how to move on.
	if !strings.Contains(msg, "curl -sS -o /dev/null -w '%{http_code}' https://<host>/") {
		t.Errorf("want the curl cross-check in the summary, got:\n%s", msg)
	}
	if !strings.Contains(msg, "--skip-check "+egressCheckName) {
		t.Errorf("want the skip flag named with this check's name, got:\n%s", msg)
	}
	if !strings.Contains(msg, "preflight defect") {
		t.Errorf("want the summary to say this may be a preflight defect, got:\n%s", msg)
	}
	// Only the failing host is listed as a finding (the remedy names the whole allowlist).
	if findings := strings.SplitN(msg, "\n\n", 2)[0]; strings.Contains(findings, "quay.io") {
		t.Errorf("a host that answered must not be listed as a finding:\n%s", findings)
	}
}
