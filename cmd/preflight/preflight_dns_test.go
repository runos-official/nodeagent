package preflight

import (
	"strings"
	"testing"
	"time"
)

// Goal 23 F4. On a box that had belonged to a RunOS cluster, preflight reported
//
//	✗ BLOCKED [egress-endpoints]: Cannot reach required HTTPS endpoint(s) on 443:
//	    - github.com (the node binary release): timed out (likely firewall/proxy block)
//	  your firewall or proxy allowlist is missing one or more.
//
// Nothing was blocked. `curl https://github.com --resolve github.com:443:<github-ip>` returned
// 200 in 0.070s and the same URL with DNS returned 200 in 5.078s, because
// /etc/systemd/resolved.conf still carried DNS=<vpn-address>, the WireGuard address of a dnsmasq
// belonging to a cluster that no longer existed. Every lookup waited out the dead server before
// falling back. Eight endpoints times five seconds is what blew the check's budget.
//
// The wording pointed at infrastructure the operator often does not control, for a problem that
// was one line in a file on the machine in front of them. RunOS writes that line itself and
// nothing removes it, so the operator most likely to meet it is the bare-metal customer reusing
// their own hardware.

func TestNetClassifyEgressFailureNamesDnsWhenResolutionFailed(t *testing.T) {
	got := netClassifyEgressFailure(netEgressDiagnosis{
		resolved:      false,
		resolveErr:    "lookup github.com: no such host",
		resolveTime:   20 * time.Millisecond,
		probeErrorMsg: "dial tcp: lookup github.com: no such host",
	})
	if !strings.Contains(got, "did not resolve") {
		t.Fatalf("expected a name-resolution verdict, got %q", got)
	}
	if strings.Contains(got, "likely firewall/proxy block") {
		t.Fatalf("a name that did not resolve must not be reported as a firewall block: %q", got)
	}
}

func TestNetClassifyEgressFailureNamesSlowResolverRatherThanFirewall(t *testing.T) {
	// The exact measured shape: the name DOES resolve, after waiting out a dead server, and the
	// connect then runs out of budget. This is the case that was mislabelled.
	got := netClassifyEgressFailure(netEgressDiagnosis{
		resolved:      true,
		resolveTime:   5078 * time.Millisecond,
		probeErrorMsg: "Client.Timeout exceeded while awaiting headers",
	})
	if !strings.Contains(strings.ToLower(got), "dns") {
		t.Fatalf("expected the slow resolver to be named, got %q", got)
	}
	if strings.Contains(got, "likely firewall/proxy block") {
		t.Fatalf("a five-second lookup must not be reported as a firewall block: %q", got)
	}
}

func TestNetClassifyEgressFailureStillBlamesTheFirewallWhenItIsOne(t *testing.T) {
	// Resolution was instant and the connect timed out anyway. The original wording is right
	// here and must not be lost.
	got := netClassifyEgressFailure(netEgressDiagnosis{
		resolved:      true,
		resolveTime:   2 * time.Millisecond,
		probeErrorMsg: "Client.Timeout exceeded while awaiting headers",
	})
	if !strings.Contains(got, "likely firewall/proxy block") {
		t.Fatalf("expected the firewall verdict for a clean lookup and a dead connect, got %q", got)
	}
}

func TestNetClassifyEgressFailureKeepsConnectionRefused(t *testing.T) {
	got := netClassifyEgressFailure(netEgressDiagnosis{
		resolved:      true,
		resolveTime:   time.Millisecond,
		probeErrorMsg: "dial tcp 1.2.3.4:443: connect: connection refused",
	})
	if !strings.Contains(got, "connection refused") {
		t.Fatalf("expected the refused verdict, got %q", got)
	}
}

func TestNetParseConfiguredResolversReadsResolvedConf(t *testing.T) {
	// The line that caused this, verbatim from the box.
	got := netParseConfiguredResolvers("[Resolve]\n#DNS=\nDNS=10.0.0.1\nDomains=~example.internal\n")
	if len(got) != 1 || got[0] != "10.0.0.1" {
		t.Fatalf("expected the configured resolver, got %v", got)
	}
}

func TestNetParseConfiguredResolversIgnoresCommentsAndBlanks(t *testing.T) {
	got := netParseConfiguredResolvers("[Resolve]\n# DNS=9.9.9.9\nDNS=\n")
	if len(got) != 0 {
		t.Fatalf("expected no configured resolver, got %v", got)
	}
}
