package preflight

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Telling a DNS problem apart from a firewall block (goal 23, F4).
//
// The egress check reported "timed out (likely firewall/proxy block)" for eight hosts on a box
// where nothing was blocked. `curl https://github.com --resolve github.com:443:<github-ip>`
// answered 200 in 0.070s; the same URL with DNS answered 200 in 5.078s, because
// /etc/systemd/resolved.conf still carried DNS=<vpn-address>, the WireGuard address of a dnsmasq
// belonging to a cluster that had been deleted. Every lookup waited out the dead server before
// falling back to the link resolvers, and eight endpoints times five seconds blew the check's
// budget.
//
// "Likely firewall/proxy block" plus "your firewall or proxy allowlist is missing one or more" is
// a confident diagnosis pointing at infrastructure the operator often does not control. The
// honest next step it produces is a ticket to a network team, for a problem that is one line in a
// file on the machine in front of them. RunOS writes that line itself and nothing removes it, so
// the operator most likely to meet this is the bare-metal customer reusing their own hardware.
//
// A timeout has three distinct causes and they need three different sentences: the name did not
// resolve, resolution is slow because a configured resolver is unreachable, or the connect really
// was refused or dropped.

// netResolveTimeout bounds the resolution probe. Deliberately longer than the 5s a dead
// systemd-resolved server costs, so a slow lookup is MEASURED rather than turned into a failure.
const netResolveTimeout = 12 * time.Second

// netSlowResolveThreshold is the point past which a lookup is itself the problem. A working
// resolver answers in single-digit milliseconds; the measured dead-server penalty was 5.078s.
const netSlowResolveThreshold = 1500 * time.Millisecond

// netEgressDiagnosis is everything known about one failed endpoint probe.
type netEgressDiagnosis struct {
	// resolved is true when the host name produced at least one address.
	resolved bool
	// resolveErr is the resolver's own error. Empty when resolution succeeded.
	resolveErr string
	// resolveTime is how long the lookup took, successful or not.
	resolveTime time.Duration
	// probeErrorMsg is the HTTPS probe's error string.
	probeErrorMsg string
	// configuredResolvers are the servers this box is configured to ask, for the message.
	configuredResolvers []string
}

// netClassifyEgressFailure turns one failed probe into an operator-readable cause.
//
// Ordered by what the evidence actually proves, not by what is most common: a name that did not
// resolve was never a firewall question, and a lookup that took five seconds explains a timeout
// on its own.
func netClassifyEgressFailure(d netEgressDiagnosis) string {
	resolvers := ""
	if len(d.configuredResolvers) > 0 {
		resolvers = fmt.Sprintf(" (resolvers tried: %s)", strings.Join(d.configuredResolvers, ", "))
	}

	if !d.resolved {
		return fmt.Sprintf(
			"the name did not resolve%s: %s. This is DNS, not a firewall.",
			resolvers, d.resolveErr)
	}

	if d.resolveTime >= netSlowResolveThreshold {
		return fmt.Sprintf(
			"the name resolved, but the lookup took %.1fs%s, which is what ran the connection out of time. "+
				"A configured DNS server is not answering. Check /etc/systemd/resolved.conf: RunOS writes "+
				"the cluster's own resolver there and nothing removes it, so a node that used to belong to "+
				"a cluster waits out a server that no longer exists on every lookup. This is DNS, not a firewall.",
			d.resolveTime.Seconds(), resolvers)
	}

	msg := d.probeErrorMsg
	switch {
	case strings.Contains(msg, "connection refused"):
		return "connection refused"
	case strings.Contains(msg, "timeout") ||
		strings.Contains(msg, "deadline exceeded") ||
		strings.Contains(msg, "Client.Timeout"):
		return "resolved in " + fmt.Sprintf("%.0fms", float64(d.resolveTime.Milliseconds())) +
			" and then timed out connecting (likely firewall/proxy block)"
	case strings.Contains(msg, "certificate") || strings.Contains(msg, "x509"):
		return "TLS certificate error (possible interception)"
	case strings.Contains(msg, "proxyconnect"):
		return "could not connect through the configured proxy"
	default:
		return msg
	}
}

// netMeasureResolve looks the host up and reports how long it took, so a slow resolver can be
// distinguished from a blocked port. Never fails the caller: an error IS the answer here.
func netMeasureResolve(host string) (bool, string, time.Duration) {
	ctx, cancel := context.WithTimeout(context.Background(), netResolveTimeout)
	defer cancel()

	started := time.Now()
	addrs, err := net.DefaultResolver.LookupHost(ctx, host)
	elapsed := time.Since(started)

	if err != nil {
		return false, err.Error(), elapsed
	}
	if len(addrs) == 0 {
		return false, "the resolver returned no addresses", elapsed
	}
	return true, "", elapsed
}

// netConfiguredResolvers reads the DNS servers this box is configured to ask.
func netConfiguredResolvers() []string {
	return netConfiguredResolversFrom("/")
}

// netConfiguredResolversFrom is netConfiguredResolvers rooted at root, so a test can lay the
// files out in a temp dir.
//
// systemd-resolved's own config first, because that is the file RunOS writes and the one that
// carries a dead cluster resolver after the cluster is gone; then its resolved.conf.d drop-ins,
// where an admin override lives. /run/systemd/resolve/resolv.conf next: that is the effective
// upstream list resolved actually uses (goal 23 review, F4-b). /etc/resolv.conf last, which on
// a systemd box usually just points at the stub.
func netConfiguredResolversFrom(root string) []string {
	var out []string
	if body, err := os.ReadFile(filepath.Join(root, "etc/systemd/resolved.conf")); err == nil {
		out = append(out, netParseConfiguredResolvers(string(body))...)
	}
	if dropIns, err := filepath.Glob(filepath.Join(root, "etc/systemd/resolved.conf.d/*.conf")); err == nil {
		sort.Strings(dropIns)
		for _, p := range dropIns {
			if body, err := os.ReadFile(p); err == nil {
				out = append(out, netParseConfiguredResolvers(string(body))...)
			}
		}
	}
	for _, rel := range []string{"run/systemd/resolve/resolv.conf", "etc/resolv.conf"} {
		body, err := os.ReadFile(filepath.Join(root, rel))
		if err != nil {
			continue
		}
		for _, line := range strings.Split(string(body), "\n") {
			line = strings.TrimSpace(line)
			if strings.HasPrefix(line, "nameserver ") {
				if server := strings.TrimSpace(strings.TrimPrefix(line, "nameserver ")); server != "" {
					out = append(out, server)
				}
			}
		}
	}
	return out
}

// netParseConfiguredResolvers pulls the servers out of a resolved.conf body. `DNS=` may list
// several, space-separated; a commented or empty line contributes nothing.
func netParseConfiguredResolvers(body string) []string {
	var out []string
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";") {
			continue
		}
		if !strings.HasPrefix(line, "DNS=") {
			continue
		}
		for _, server := range strings.Fields(strings.TrimPrefix(line, "DNS=")) {
			if server != "" {
				out = append(out, server)
			}
		}
	}
	return out
}
