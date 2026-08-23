package backend

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"
)

// A node that lost every per-link DNS scope reports the L2Sec dial failure as
// "did not connect to nodeward L2: context deadline exceeded" and repeats it
// forever. That sentence never mentions DNS, so the operator has nothing to act
// on. These tests pin the explanation this package must add: the name that did
// not resolve, what the error shape actually proves, and the commands that check
// and repair it.
//
// The error shapes below are the ones a real net.Resolver produces. They were
// measured against a fake DNS server and a black-holed address with
// CGO_ENABLED=0, which is the resolver the shipped Linux binary uses.

// dnsErrServer is the nameserver Go records in DNSError.Server. On a RunOS node
// running systemd-resolved this is the stub the node is pointed at.
const dnsErrServer = "127.0.0.53:53"

// servfail is a resolver that was reached and cannot answer. systemd-resolved
// with no scope to forward to answers SERVFAIL, which Go reports as
// "server misbehaving".
func servfail(context.Context, string) ([]string, error) {
	return nil, &net.DNSError{
		Err: "server misbehaving", Name: "nodeward.runos.com",
		Server: dnsErrServer, IsTemporary: true,
	}
}

func TestExplainNodewardDialFailureBrokenResolver(t *testing.T) {
	dialErr := context.DeadlineExceeded

	t.Run("SERVFAIL names DNS, the host, the resolver and the remedy", func(t *testing.T) {
		err := explainNodewardDialFailure("nodeward.runos.com", dialErr, servfail)
		if err == nil {
			t.Fatal("want an error, got nil")
		}
		msg := err.Error()
		for _, want := range []string{
			"nodeward.runos.com",
			"no working DNS resolver",
			// Named as a sentence, not only echoed inside the raw resolver error.
			"The resolver at " + dnsErrServer,
			"resolvectl status",
			"resolvectl dns",
			"systemctl restart runos.service",
		} {
			if !strings.Contains(msg, want) {
				t.Errorf("message does not mention %q\ngot: %s", want, msg)
			}
		}
		if !errors.Is(err, dialErr) {
			t.Error("the original dial error must stay wrapped, so callers can still match it")
		}
	})

	t.Run("nothing listening on the resolver address is the same diagnosis", func(t *testing.T) {
		refused := func(context.Context, string) ([]string, error) {
			return nil, &net.DNSError{
				Err:  "read udp 127.0.0.1:1->127.0.0.53:53: read: connection refused",
				Name: "nodeward.runos.com", Server: dnsErrServer, IsTemporary: true,
			}
		}
		msg := explainNodewardDialFailure("nodeward.runos.com", dialErr, refused).Error()
		if !strings.Contains(msg, "no working DNS resolver") {
			t.Errorf("a refused resolver resolves nothing\ngot: %s", msg)
		}
	})

	// The message must state what RunOS does, not guess which cause applies. The
	// one node this fault was ever seen on had been reset, so its wg0 tunnel and
	// its dnsmasq were both already gone: commons/uninstall.go stops, disables and
	// purges dnsmasq and wireguard. A message that blames those two sends the
	// operator to inspect services that do not exist on the machine in front of
	// them.
	t.Run("the message does not blame a cause it has not proven", func(t *testing.T) {
		msg := explainNodewardDialFailure("nodeward.runos.com", dialErr, servfail).Error()
		for _, banned := range []string{
			"tunnel or dnsmasq is down",
			"wg0 tunnel or dnsmasq",
		} {
			if strings.Contains(msg, banned) {
				t.Errorf("message asserts an unproven cause %q\ngot: %s", banned, msg)
			}
		}
	})
}

// A lookup that ran out of time proves the resolver did not answer. It does not
// prove the resolver is absent, and it does not prove DNS is the fault at all: a
// node whose link is down or whose default route is gone fails this lookup and
// the dial for one shared reason. Telling that operator to set a per-link
// resolver wastes their time and hides the real fault.
func TestExplainNodewardDialFailureTimeout(t *testing.T) {
	dialErr := context.DeadlineExceeded

	// Measured shape of a black-holed resolver: IsTimeout is set, and the error
	// does NOT satisfy errors.Is(err, context.DeadlineExceeded). A branch that
	// tests only the context deadline misses this case entirely.
	blackHoled := func(context.Context, string) ([]string, error) {
		return nil, &net.DNSError{
			Err:  "read udp 10.0.0.5:1->203.0.113.1:53: i/o timeout",
			Name: "nodeward.runos.com", Server: "203.0.113.1:53",
			IsTimeout: true, IsTemporary: true,
		}
	}

	t.Run("a timed-out lookup never claims the node has no resolver", func(t *testing.T) {
		msg := explainNodewardDialFailure("nodeward.runos.com", dialErr, blackHoled).Error()
		if strings.Contains(msg, "no working DNS resolver") {
			t.Errorf("a timeout does not prove a resolver is absent\ngot: %s", msg)
		}
		if strings.Contains(msg, "symptom and not the cause") {
			t.Errorf("a timeout does not prove DNS is the cause\ngot: %s", msg)
		}
	})

	t.Run("a timed-out lookup sends the operator to the link first", func(t *testing.T) {
		msg := explainNodewardDialFailure("nodeward.runos.com", dialErr, blackHoled).Error()
		for _, want := range []string{
			"did not answer in time",
			"ip -brief address",
			"ip route",
			"The resolver at 203.0.113.1:53",
		} {
			if !strings.Contains(msg, want) {
				t.Errorf("message does not mention %q\ngot: %s", want, msg)
			}
		}
	})

	t.Run("a bare context deadline takes the timeout branch too", func(t *testing.T) {
		expired := func(context.Context, string) ([]string, error) {
			return nil, context.DeadlineExceeded
		}
		msg := explainNodewardDialFailure("nodeward.runos.com", dialErr, expired).Error()
		if strings.Contains(msg, "no working DNS resolver") {
			t.Errorf("an expired diagnosis context proves nothing about the resolver\ngot: %s", msg)
		}
	})

	t.Run("the timeout branch still wraps the dial error", func(t *testing.T) {
		err := explainNodewardDialFailure("nodeward.runos.com", dialErr, blackHoled)
		if !errors.Is(err, dialErr) {
			t.Error("the original dial error must stay wrapped")
		}
	})
}

// Go reports NXDOMAIN and NOERROR-with-no-answers as the same error: an
// IsNotFound DNSError reading "no such host". A resolver with nothing to forward
// to can therefore produce this for a name that does exist, so the message must
// not blame the configured host on its own.
func TestExplainNodewardDialFailureNotFound(t *testing.T) {
	dialErr := context.DeadlineExceeded

	notFound := func(context.Context, string) ([]string, error) {
		return nil, &net.DNSError{
			Err: "no such host", Name: "typo.runos.com",
			Server: dnsErrServer, IsNotFound: true,
		}
	}

	t.Run("a not-found answer does not call the resolver broken", func(t *testing.T) {
		msg := explainNodewardDialFailure("typo.runos.com", dialErr, notFound).Error()
		if strings.Contains(msg, "no working DNS resolver") {
			t.Errorf("the resolver answered; do not claim it is broken\ngot: %s", msg)
		}
	})

	// A lookup that returns an empty list with no error is also a resolver that
	// responded. Go's own resolver does not produce that shape, so this arm is
	// defensive, but calling a responding resolver dead would overclaim.
	t.Run("an empty answer with no error is a resolver that responded", func(t *testing.T) {
		empty := func(context.Context, string) ([]string, error) { return nil, nil }
		msg := explainNodewardDialFailure("nodeward.runos.com", dialErr, empty).Error()
		if strings.Contains(msg, "no working DNS resolver") {
			t.Errorf("the resolver returned an answer; do not claim it is absent\ngot: %s", msg)
		}
		if !strings.Contains(msg, "answered with no address") {
			t.Errorf("want the empty-answer wording\ngot: %s", msg)
		}
	})

	t.Run("a not-found answer offers both faults, scopes first", func(t *testing.T) {
		msg := explainNodewardDialFailure("typo.runos.com", dialErr, notFound).Error()
		for _, want := range []string{
			"client.server.nodeward",
			"resolvectl status",
			"Current Scopes: none",
		} {
			if !strings.Contains(msg, want) {
				t.Errorf("message does not mention %q\ngot: %s", want, msg)
			}
		}
		scopes := strings.Index(msg, "resolvectl status")
		name := strings.Index(msg, "client.server.nodeward")
		if scopes > name {
			t.Errorf("check the scopes before the configured name; a scope-less resolver "+
				"answers \"no such host\" for a name that exists\ngot: %s", msg)
		}
	})

	t.Run("the not-found branch still wraps the dial error", func(t *testing.T) {
		err := explainNodewardDialFailure("typo.runos.com", dialErr, notFound)
		if !errors.Is(err, dialErr) {
			t.Error("the original dial error must stay wrapped")
		}
	})
}

// Everything here must leave the dial error exactly as it was. A node whose DNS
// works has a different problem, and this code must not relabel it.
func TestExplainNodewardDialFailureLeavesWorkingPathsAlone(t *testing.T) {
	dialErr := context.DeadlineExceeded

	t.Run("a resolvable host returns the dial error untouched", func(t *testing.T) {
		ok := func(context.Context, string) ([]string, error) { return []string{"10.0.0.1"}, nil }
		err := explainNodewardDialFailure("nodeward.runos.com", dialErr, ok)
		if err != dialErr {
			t.Errorf("want the dial error unchanged, got: %v", err)
		}
	})

	t.Run("an IP literal host is never resolved", func(t *testing.T) {
		called := false
		spy := func(context.Context, string) ([]string, error) {
			called = true
			return nil, errors.New("must not be called")
		}
		for _, host := range []string{"192.168.0.10", "2001:db8::1"} {
			if err := explainNodewardDialFailure(host, dialErr, spy); err != dialErr {
				t.Errorf("host %q: want the dial error unchanged, got: %v", host, err)
			}
		}
		if called {
			t.Error("an IP literal has no name to look up; the resolver must not be called")
		}
	})

	t.Run("an empty host is left alone", func(t *testing.T) {
		spy := func(context.Context, string) ([]string, error) {
			t.Fatal("the resolver must not be called for an empty host")
			return nil, nil
		}
		if err := explainNodewardDialFailure("", dialErr, spy); err != dialErr {
			t.Errorf("want the dial error unchanged, got: %v", err)
		}
	})

	t.Run("a nil dial error stays nil", func(t *testing.T) {
		if err := explainNodewardDialFailure("nodeward.runos.com", nil, servfail); err != nil {
			t.Errorf("want nil, got: %v", err)
		}
	})
}

// `runos status` needs to know whether the error it is about to print already
// names its own repair. Without that it appends "run `runos register`", which
// resolves the same host and fails the same way. ErrNodewardDNS is how the dial
// path says so, and it must never be attached to a path this file left alone.
func TestExplainNodewardDialFailureMarksADiagnosedError(t *testing.T) {
	dialErr := context.DeadlineExceeded

	blackHoled := func(context.Context, string) ([]string, error) {
		return nil, &net.DNSError{
			Err: "i/o timeout", Name: "nodeward.runos.com",
			Server: "203.0.113.1:53", IsTimeout: true,
		}
	}
	notFound := func(context.Context, string) ([]string, error) {
		return nil, &net.DNSError{
			Err: "no such host", Name: "nodeward.runos.com",
			Server: dnsErrServer, IsNotFound: true,
		}
	}

	diagnosed := map[string]hostResolver{
		"broken resolver": servfail,
		"timed out":       blackHoled,
		"not found":       notFound,
	}
	for name, resolve := range diagnosed {
		t.Run(name+" is marked", func(t *testing.T) {
			err := explainNodewardDialFailure("nodeward.runos.com", dialErr, resolve)
			if !errors.Is(err, ErrNodewardDNS) {
				t.Errorf("a message that names its own repair must carry ErrNodewardDNS\ngot: %v", err)
			}
			if !errors.Is(err, dialErr) {
				t.Error("marking the error must not drop the wrapped dial error")
			}
		})
	}

	t.Run("a working lookup is never marked", func(t *testing.T) {
		ok := func(context.Context, string) ([]string, error) { return []string{"10.0.0.1"}, nil }
		err := explainNodewardDialFailure("nodeward.runos.com", dialErr, ok)
		if errors.Is(err, ErrNodewardDNS) {
			t.Error("DNS worked; the error must not claim a resolver diagnosis")
		}
	})

	t.Run("an IP literal is never marked", func(t *testing.T) {
		spy := func(context.Context, string) ([]string, error) { return nil, errors.New("unused") }
		err := explainNodewardDialFailure("192.168.0.10", dialErr, spy)
		if errors.Is(err, ErrNodewardDNS) {
			t.Error("an IP literal never reached a resolver; do not claim a DNS fault")
		}
	})

	// The sentinel's text opens every message, so wrapping it must not leave a
	// stray duplicate phrase in the sentence the operator reads.
	t.Run("the sentinel reads as the opening of the message", func(t *testing.T) {
		msg := explainNodewardDialFailure("nodeward.runos.com", dialErr, servfail).Error()
		if !strings.HasPrefix(msg, ErrNodewardDNS.Error()+" ") {
			t.Errorf("want the message to open with %q\ngot: %s", ErrNodewardDNS.Error(), msg)
		}
		if strings.Count(msg, ErrNodewardDNS.Error()) != 1 {
			t.Errorf("the sentinel text must appear exactly once\ngot: %s", msg)
		}
	})
}

// `runos status` does not read the dial error directly. testConnection wraps it
// as "connection failed: %w" before gatherReport tests it, so the sentinel has to
// survive one more layer than the tests above exercise. This reproduces the whole
// chain, because a sentinel that gets lost here silently restores the old wrong
// remedy with every test still green.
func TestExplainNodewardDialFailureSurvivesTheStatusWrap(t *testing.T) {
	dialErr := context.DeadlineExceeded

	enriched := explainNodewardDialFailure("nodeward.runos.com", dialErr, servfail)
	connErr := fmt.Errorf("connection failed: %w", enriched) // testConnection's wrap

	if !errors.Is(connErr, ErrNodewardDNS) {
		t.Error("`runos status` cannot tell a DNS fault apart after its own wrap, " +
			"so it would offer `runos register` again")
	}
	if !errors.Is(connErr, dialErr) {
		t.Error("the dial error must stay matchable through both wraps")
	}
	if !strings.Contains(connErr.Error(), "resolvectl dns") {
		t.Errorf("the repair must survive to the terminal\ngot: %s", connErr.Error())
	}
}

// cmd/preflight/preflight_dns.go measured the cost of the RunOS fault that leaves
// a dead DNS= server in /etc/systemd/resolved.conf: 5.078s per lookup, because Go
// waits the dead server out before falling back. A budget that cannot hold two of
// those waits reports a slow-but-working resolver as an absent one, which is the
// false alarm this whole file exists to avoid. preflight chose 12s for the same
// reason; this test fails if someone shortens it.
func TestDNSDiagnosisBudgetHoldsTwoDeadServerWaits(t *testing.T) {
	const measuredDeadServerCost = 5078 * time.Millisecond

	if dnsDiagnosisTimeout < 2*measuredDeadServerCost {
		t.Errorf("budget %s is shorter than two measured dead-server waits (%s); "+
			"a slow resolver would be reported as no resolver",
			dnsDiagnosisTimeout, 2*measuredDeadServerCost)
	}

	var deadline time.Time
	var ok bool
	capture := func(ctx context.Context, _ string) ([]string, error) {
		deadline, ok = ctx.Deadline()
		return []string{"10.0.0.1"}, nil
	}
	explainNodewardDialFailure("nodeward.runos.com", context.DeadlineExceeded, capture)
	if !ok {
		t.Fatal("the lookup must be bounded; it runs on a path that has already failed")
	}
	if remaining := time.Until(deadline); remaining < 2*measuredDeadServerCost {
		t.Errorf("the resolver got %s, want at least %s", remaining, 2*measuredDeadServerCost)
	}
}
