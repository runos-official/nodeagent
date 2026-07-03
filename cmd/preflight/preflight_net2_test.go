package preflight

import (
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
)

// A mirror root that 301s to an unreachable host must still count as
// reachable: the response itself proves the mirror is up, and following the
// redirect would probe a host apt never contacts (the security.ubuntu.com ->
// www.ubuntu.com case that false-blocked a real install).
func TestNwProbeMirrorDoesNotFollowRedirect(t *testing.T) {
	// A listener we immediately close: guaranteed connection-refused target.
	dead, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	deadAddr := dead.Addr().String()
	dead.Close()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, fmt.Sprintf("http://%s/", deadAddr), http.StatusMovedPermanently)
	}))
	defer srv.Close()

	reason, blocked := nwProbeMirrorUnreachable(srv.URL)
	if blocked {
		t.Fatalf("301 response must prove reachability without following the redirect; got blocked with reason %q", reason)
	}
}

// Mirror roots commonly reject a bare request; any HTTP status is proof of
// reachability.
func TestNwProbeMirrorErrorStatusIsReachable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv.Close()

	if reason, blocked := nwProbeMirrorUnreachable(srv.URL); blocked {
		t.Fatalf("403 must count as reachable; got blocked with reason %q", reason)
	}
}

// A genuinely dead port must still be flagged.
func TestNwProbeMirrorConnectionRefused(t *testing.T) {
	dead, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	deadURL := "http://" + dead.Addr().String()
	dead.Close()

	reason, blocked := nwProbeMirrorUnreachable(deadURL)
	if !blocked {
		t.Fatal("closed port must be flagged unreachable")
	}
	if reason != "connection refused" {
		t.Fatalf("expected reason %q, got %q", "connection refused", reason)
	}
}
