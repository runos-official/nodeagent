package agentstream

import (
	"fmt"
	"net"
	"net/url"
	"strings"
)

// netguard provides conservative SSRF protections shared by the handlers that
// fetch attacker-influenceable URLs as root (RUN_WEB_REQUEST, INSTALL_HELM_CHART).
//
// Policy (deliberately narrow so legitimate in-cluster traffic keeps working):
//   - the scheme must be http or https,
//   - the resolved IP(s) must not be loopback, link-local, or the cloud
//     metadata address (169.254.169.254).
//
// We do NOT block general RFC1918 / private ranges: in-cluster calls to pod,
// service and node IPs are legitimate and routine for this agent.

// metadataIP is the well-known cloud instance-metadata address. It is covered by
// the link-local /16 block below, but is called out explicitly for clarity.
var metadataIP = net.ParseIP("169.254.169.254")

// isBlockedIP reports whether ip is an address we must never dial: loopback
// (127.0.0.0/8, ::1), link-local IPv4 (169.254.0.0/16, which includes the
// metadata IP), or link-local IPv6 (fe80::/10). nil is treated as blocked.
func isBlockedIP(ip net.IP) bool {
	if ip == nil {
		return true
	}
	if ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() {
		return true
	}
	if ip.Equal(metadataIP) {
		return true
	}
	return false
}

// resolveHostIPs returns the IPs a host resolves to. A host that is already a
// literal IP resolves to itself. The bool reports whether resolution happened
// against DNS (true) vs the host already being a literal (false); callers use it
// to decide whether re-pinning the dial to the validated IP is meaningful.
func resolveHostIPs(host string) (ips []net.IP, wasLiteral bool, err error) {
	if ip := net.ParseIP(host); ip != nil {
		return []net.IP{ip}, true, nil
	}
	resolved, err := net.LookupIP(host)
	if err != nil {
		return nil, false, err
	}
	if len(resolved) == 0 {
		return nil, false, fmt.Errorf("host %q resolved to no addresses", host)
	}
	return resolved, false, nil
}

// validateOutboundURL parses rawURL, enforces an http/https scheme, resolves the
// host, and rejects the request if ANY resolved IP is blocked (loopback,
// link-local, or metadata). It returns the parsed URL and the resolved IPs so a
// caller can pin its dial to a validated IP (defeating DNS-rebinding).
//
// requireHTTPS, when true, additionally rejects plain http:// (used for helm
// repo/values URLs which must always be TLS).
func validateOutboundURL(rawURL string, requireHTTPS bool) (*url.URL, []net.IP, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, nil, fmt.Errorf("invalid URL: %w", err)
	}

	scheme := strings.ToLower(u.Scheme)
	if requireHTTPS {
		if scheme != "https" {
			return nil, nil, fmt.Errorf("URL scheme must be https, got %q", u.Scheme)
		}
	} else if scheme != "http" && scheme != "https" {
		return nil, nil, fmt.Errorf("URL scheme must be http or https, got %q", u.Scheme)
	}

	host := u.Hostname()
	if host == "" {
		return nil, nil, fmt.Errorf("URL has no host")
	}

	ips, _, err := resolveHostIPs(host)
	if err != nil {
		return nil, nil, fmt.Errorf("could not resolve host %q: %w", host, err)
	}

	for _, ip := range ips {
		if isBlockedIP(ip) {
			return nil, nil, fmt.Errorf("refusing request to internal/metadata address %s (host %q)", ip, host)
		}
	}

	return u, ips, nil
}
