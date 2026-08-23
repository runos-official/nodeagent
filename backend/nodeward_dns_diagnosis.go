package backend

import (
	"context"
	"errors"
	"fmt"
	"net"
	"time"
)

// Turning a dead resolver into a sentence the operator can act on (FCR 148, F16).
//
// A node that rejoined came up with no DNS scope on any link: `resolvectl status`
// showed "Current Scopes: none" on eno1..eno4 and wg0, so nothing forwarded and
// the node resolved nothing at all. The only thing the agent said was
// "did not connect to nodeward L2: context deadline exceeded", repeated forever.
// That sentence names a symptom (a dial that ran out of time) and never names the
// cause, so the operator has no next step. The step that unblocked the node was
// one command: `resolvectl dns eno1 192.168.0.1`.
//
// The dial itself is left exactly as it was. grpc.DialContext retries internally
// for the whole l2secDialTimeout window, so it survives a resolver that is merely
// slow. A pre-dial gate would turn that survivable case into a failed install, so
// the lookup runs only AFTER the dial has already failed, purely to explain why.
//
// Each branch claims only what its error shape proves. The shapes below are
// measured, not assumed. A real net.Resolver drove a fake DNS server and a
// black-holed address, CGO_ENABLED=0, which is what the shipped Linux binary uses
// (.github/workflows/release.yml sets CGO_ENABLED=0, so the pure Go resolver runs):
//
//	SERVFAIL             -> DNSError{Err: "server misbehaving",       IsTemporary: true}
//	connection refused   -> DNSError{Err: "...connection refused",    IsTemporary: true}
//	unreachable resolver -> DNSError{Err: "...i/o timeout",           IsTimeout:   true}
//	NXDOMAIN             -> DNSError{Err: "no such host",             IsNotFound:  true}
//	NOERROR, 0 answers   -> DNSError{Err: "no such host",             IsNotFound:  true}
//
// Two of those measurements decide the code.
//
// First, an unreachable resolver reports IsTimeout and does NOT satisfy
// errors.Is(err, context.DeadlineExceeded), so a deadline test alone would miss it
// and fall through to the strongest message. A lookup that ran out of time proves
// nothing about whether a resolver exists: a node with no uplink at all fails this
// lookup and the dial for one shared reason. That case gets its own branch and
// never claims the node has no resolver.
//
// Second, NXDOMAIN and NOERROR-with-no-answers are the same error in Go. A
// resolver with nothing to forward to can answer "no such host" for a name that
// does exist, so the not-found branch cannot blame the configured host alone.

// dnsDiagnosisTimeout bounds the explanatory lookup. It matches the budget
// cmd/preflight/preflight_dns.go measured for this exact fault class: a dead
// systemd-resolved server costs about 5s per lookup before Go falls back to the
// next server, so a node carrying one dead server ahead of a live one still
// answers well inside 12s and takes the "DNS works" path. A shorter budget turns
// a slow-but-working resolver into a false "no resolver" report.
const dnsDiagnosisTimeout = 12 * time.Second

// ErrNodewardDNS marks a dial failure this file attached a name-resolution
// diagnosis to. Every message below already names the check and the repair, so a
// caller that would otherwise append its own generic remedy tests for this and
// suppresses it. Without that test `runos status` sends an operator whose
// resolver is dead to `runos register`, which cannot resolve a host either.
//
// The sentinel's text opens each message, so wrapping it costs no extra words.
var ErrNodewardDNS = errors.New("could not resolve the Nodeward host")

// hostResolver looks a host name up. net.DefaultResolver.LookupHost satisfies it;
// a test substitutes its own.
type hostResolver func(ctx context.Context, host string) ([]string, error)

// resolverName names the server Go actually asked, so the operator repairs the
// resolver in front of them rather than one they have to guess at. Go fills
// DNSError.Server from the nameserver it queried; it is empty when the failure
// happened before any server was chosen.
//
// The article is left to the caller, because two of the three messages open a
// sentence with this phrase and one uses it mid-sentence.
func resolverName(dnsErr *net.DNSError) string {
	if dnsErr == nil || dnsErr.Server == "" {
		return "resolver this node is configured to ask"
	}
	return "resolver at " + dnsErr.Server
}

// explainNodewardDialFailure returns dialErr with a DNS explanation attached when
// the node cannot resolve host, and returns dialErr untouched otherwise.
//
// It never turns a working path into a failure: the caller has already failed, and
// every branch that cannot prove a DNS fault returns the original error unchanged.
func explainNodewardDialFailure(host string, dialErr error, resolve hostResolver) error {
	if dialErr == nil {
		return nil
	}
	// Nothing to look up: an unset host is a config fault the caller reports on its
	// own, and an IP literal never reaches a resolver.
	if host == "" || net.ParseIP(host) != nil {
		return dialErr
	}

	ctx, cancel := context.WithTimeout(context.Background(), dnsDiagnosisTimeout)
	defer cancel()

	addrs, err := resolve(ctx, host)
	if err == nil && len(addrs) > 0 {
		// DNS works. Whatever broke the dial, it was not name resolution.
		return dialErr
	}

	cause := "the lookup returned no addresses"
	if err != nil {
		cause = err.Error()
	}

	var dnsErr *net.DNSError
	isDNSErr := errors.As(err, &dnsErr)

	// The lookup ran out of time. That proves the resolver did not answer; it does
	// NOT prove the node has no resolver, and it does not prove DNS is the fault.
	// A node whose link is down or whose default route is gone fails this lookup
	// and the dial for the same reason, so the operator checks the link first.
	if (isDNSErr && dnsErr.IsTimeout) || errors.Is(err, context.DeadlineExceeded) {
		return fmt.Errorf(
			"%w %q within %s (%s). The %s did not answer in time, "+
				"so this node either cannot reach its resolver or has lost its network path entirely. "+
				"Check the link and the route first with 'ip -brief address' and 'ip route'; "+
				"a node with no uplink fails this lookup and the connection below for one shared reason. "+
				"If the link is up, check the resolver with 'resolvectl status'. "+
				"Underlying connection error: %w",
			ErrNodewardDNS, host, dnsDiagnosisTimeout, cause, resolverName(dnsErr), dialErr)
	}

	// The resolver responded, and its answer names no address. Two different
	// faults produce that, Go reports them identically, so the message names both
	// in the order the operator should check them.
	//
	// A lookup that returns an empty list with no error belongs here rather than
	// below: the resolver responded, so calling it dead would claim more than the
	// evidence carries. Go's own resolver does not produce that shape, so this
	// arm is defensive.
	if err == nil || (isDNSErr && dnsErr.IsNotFound) {
		return fmt.Errorf(
			"%w %q: the %s answered with no address (%s). "+
				"Two faults give that answer. A resolver with no upstream to forward to answers "+
				"\"no such host\" for a name that does exist, so run 'resolvectl status' first: a "+
				"link with no resolver reports 'Current Scopes: none'. If every link has a resolver, "+
				"the name itself is wrong, so check client.server.nodeward in /etc/runos/config.yaml. "+
				"Underlying connection error: %w",
			ErrNodewardDNS, host, resolverName(dnsErr), cause, dialErr)
	}

	// A resolver that was reached and still cannot answer (SERVFAIL, or nothing
	// listening on the configured address). This node resolves nothing, so the dial
	// could not have succeeded whatever else is true.
	return fmt.Errorf(
		"%w %q (%s). The %s is not answering, so this node has no "+
			"working DNS resolver and the connection error below is a symptom and not the cause. "+
			"RunOS clears the per-link DNS during install and points the node at its own resolver "+
			"(the DNS= value in /etc/systemd/resolved.conf, normally dnsmasq on the node's WireGuard "+
			"address). A reset restores that file and leaves the links cleared, so a rejoining node "+
			"can end up with no DNS at all. Run 'resolvectl status' to see what each link has: a link "+
			"with no resolver reports 'Current Scopes: none'. On a node that is still joined, check "+
			"'wg show' and 'systemctl status dnsmasq' as well. Give the node's primary link a resolver "+
			"with 'resolvectl dns <link> <dns-server>' (for example 'resolvectl dns eno1 192.168.0.1'), "+
			"then restart the agent with 'sudo systemctl restart runos.service'. "+
			"Underlying connection error: %w",
		ErrNodewardDNS, host, cause, resolverName(dnsErr), dialErr)
}
