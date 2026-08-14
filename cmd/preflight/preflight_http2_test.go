package preflight

import (
	"net/http"
	"testing"
)

// Goal 23 F28. Preflight BLOCKED every node install on machines where the
// network was demonstrably fine:
//
//	✗ BLOCKED [egress-endpoints]: Cannot reach required HTTPS endpoint(s) on 443:
//	    - github.com (the node binary release): Get "https://github.com/": EOF
//
// while `curl https://github.com/` on the same box returned HTTP 200 five times
// out of five, and the remediation text told the operator to open a firewall
// that was already open.
//
// The cause was this client, not the network. Setting DialContext on an
// http.Transport switches OFF Go's automatic HTTP/2 upgrade, so preflight spoke
// HTTP/1.1 while every other tool on the box spoke HTTP/2. GitHub drops HTTP/1.1
// from some hosts and returns an empty reply, which Go reports as `EOF`.
// Measured against github.com, registry.k8s.io and quay.io: with DialContext set
// and ForceAttemptHTTP2 off, the probe negotiates HTTP/1.1; with it on, HTTP/2.0.
//
// This test is hermetic on purpose. Asserting the negotiated protocol would need
// the network, and the bug is a transport CONFIGURATION mistake, so the
// configuration is the right thing to pin.
func TestNetHTTPClientForcesHTTP2(t *testing.T) {
	for _, follow := range []bool{true, false} {
		c := netHTTPClient(follow)
		tr, ok := c.Transport.(*http.Transport)
		if !ok {
			t.Fatalf("follow=%v: transport is %T, want *http.Transport", follow, c.Transport)
		}
		if !tr.ForceAttemptHTTP2 {
			t.Errorf("follow=%v: ForceAttemptHTTP2 is false. This client sets DialContext, "+
				"which disables Go's automatic HTTP/2 upgrade, so it will speak HTTP/1.1 and "+
				"report EOF against hosts that refuse HTTP/1.1. See goal 23 F28.", follow)
		}
		if tr.DialContext == nil {
			t.Errorf("follow=%v: DialContext is nil; if the custom dialer was removed, "+
				"ForceAttemptHTTP2 is no longer load-bearing and this test should be revisited", follow)
		}
	}
}

// Goal 23 F28. A single dropped connection used to block an install outright.
// These hosts drop connections intermittently, so a verdict as severe as
// "nothing has been installed" must not rest on one attempt.
func TestNetProbeRetriesMoreThanOnce(t *testing.T) {
	if netProbeAttempts < 2 {
		t.Errorf("netProbeAttempts = %d, want >= 2 so one dropped connection cannot block an install",
			netProbeAttempts)
	}
}
