package preflight

import (
	"crypto/tls"
	"crypto/x509"
	"net/http"
	"net/http/httptest"
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
//
// This test is hermetic and behavioural (goal 23 review, F28-b): a local TLS
// server that offers h2 must see the probe arrive over HTTP/2. A struct-field
// assertion could pass while a later change to the transport broke negotiation.
func TestNetProbeNegotiatesHTTP2(t *testing.T) {
	var sawProto string
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawProto = r.Proto
		w.WriteHeader(http.StatusOK)
	}))
	srv.EnableHTTP2 = true
	srv.StartTLS()
	defer srv.Close()

	pool := x509.NewCertPool()
	pool.AddCert(srv.Certificate())
	restore := netTLSClientConfigForTest(&tls.Config{RootCAs: pool})
	defer restore()

	for _, follow := range []bool{true, false} {
		req, err := http.NewRequest(http.MethodGet, srv.URL+"/", nil)
		if err != nil {
			t.Fatal(err)
		}
		resp, err := netHTTPClient(follow).Do(req)
		if err != nil {
			t.Fatalf("follow=%v: probe failed: %v", follow, err)
		}
		resp.Body.Close()
		if resp.ProtoMajor != 2 {
			t.Errorf("follow=%v: negotiated %s, want HTTP/2. This client sets DialContext, "+
				"which disables Go's automatic HTTP/2 upgrade unless ForceAttemptHTTP2 is on; "+
				"hosts that refuse HTTP/1.1 then report EOF. See goal 23 F28.", follow, resp.Proto)
		}
	}

	// The real probe path, end to end.
	code, err := netProbeHTTPSOnce(srv.URL + "/")
	if err != nil {
		t.Fatalf("netProbeHTTPSOnce: %v", err)
	}
	if code != http.StatusOK {
		t.Errorf("netProbeHTTPSOnce code = %d, want 200", code)
	}
	if sawProto != "HTTP/2.0" {
		t.Errorf("server saw the probe as %q, want HTTP/2.0", sawProto)
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
