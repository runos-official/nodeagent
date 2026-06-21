package preflight

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/net/http/httpproxy"

	"github.com/runos-official/nodeagent/config"
	"github.com/runos-official/nodeagent/roslog"
)

// netNodewardPorts are the two high ports the agent's gRPC channels use:
// 9191 = L1Sec (registration, TLS), 9192 = L2Sec (operations, mTLS). Neither
// goes through an HTTP proxy, so they must have a working DIRECT TCP route.
var netNodewardPorts = []struct {
	port int
	name string
}{
	{9191, "registration (L1Sec)"},
	{9192, "operations (L2Sec)"},
}

// netEgressEndpoints is the full set of HTTPS hosts the install pulls from. Each
// entry has a probe path; ghcr.io/quay.io reliably answer 401 at /v2/ (which we
// treat as reachable) so we don't need credentials.
var netEgressEndpoints = []struct {
	host string
	path string
	why  string
}{
	{"github.com", "/", "the node binary release"},
	{"objects.githubusercontent.com", "/", "the node binary / release assets"},
	{"pkgs.k8s.io", "/", "the Kubernetes apt repository"},
	{"registry.k8s.io", "/", "Kubernetes container images"},
	{"registry-1.docker.io", "/v2/", "Docker Hub container images"},
	{"ghcr.io", "/v2/", "GitHub container images"},
	{"quay.io", "/v2/", "Quay container images"},
	{"helm.cilium.io", "/", "the Cilium Helm chart"},
}

// netDialTimeout bounds every raw TCP probe.
const netDialTimeout = 5 * time.Second

// netHTTPTimeout bounds every HTTPS probe (connect + TLS + first byte).
const netHTTPTimeout = 8 * time.Second

// netDial does a single bounded raw TCP dial (no proxy), returning nil on a
// successful connect. Used for the gRPC high ports, which never proxy.
func netDial(host string, port int) error {
	addr := net.JoinHostPort(host, fmt.Sprintf("%d", port))
	conn, err := net.DialTimeout("tcp", addr, netDialTimeout)
	if err != nil {
		return err
	}
	conn.Close()
	return nil
}

// netHTTPClient builds an http.Client that resolves proxy settings exactly the
// way the agent's own HTTP egress does (ProxyFromEnvironment), so a preflight
// verdict matches real behaviour. follow controls redirect following.
func netHTTPClient(follow bool) *http.Client {
	tr := &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout: netDialTimeout,
		}).DialContext,
		TLSHandshakeTimeout:   netDialTimeout,
		ResponseHeaderTimeout: netHTTPTimeout,
	}
	c := &http.Client{Timeout: netHTTPTimeout, Transport: tr}
	if !follow {
		c.CheckRedirect = func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		}
	}
	return c
}

// netProbeHTTPS does a GET to https://host+path and returns (statusCode, err).
// A transport-level failure returns err != nil; otherwise the HTTP status is
// returned and the caller decides reachability (any non-5xx = reachable).
func netProbeHTTPS(host, path string) (int, error) {
	u := "https://" + host + path
	ctx, cancel := context.WithTimeout(context.Background(), netHTTPTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return 0, err
	}
	req.Header.Set("User-Agent", "runos-preflight/1")
	resp, err := netHTTPClient(true).Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	return resp.StatusCode, nil
}

// netNodewardHost resolves the Nodeward host to probe: the explicit --server
// flag if set, otherwise the configured/default Nodeward host. Returns "" only
// if both are empty (which should not happen given a baked-in default).
func netNodewardHost() string {
	if h := strings.TrimSpace(server); h != "" {
		return h
	}
	return strings.TrimSpace(config.GetNodewardHost())
}

// netCDNHost extracts the host from cdnURL (--cdn). Returns "" if unset or
// unparseable.
func netCDNHost() string {
	raw := strings.TrimSpace(cdnURL)
	if raw == "" {
		return ""
	}
	if !strings.Contains(raw, "://") {
		raw = "https://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return ""
	}
	return u.Hostname()
}

// netProxyConfig reads proxy settings from the process environment AND the
// system-wide locations that the installer-invoked agent inherits but an
// interactive shell may not show: /etc/environment, /etc/apt/apt.conf.d/*proxy*,
// and the runos systemd drop-ins. Returns the merged httpproxy.Config and a
// human label of where the first https/http proxy came from.
func netProxyConfig() (*httpproxy.Config, string) {
	get := func(keys ...string) string {
		for _, k := range keys {
			if v := strings.TrimSpace(os.Getenv(k)); v != "" {
				return v
			}
		}
		return ""
	}
	cfg := &httpproxy.Config{
		HTTPProxy:  get("HTTP_PROXY", "http_proxy"),
		HTTPSProxy: get("HTTPS_PROXY", "https_proxy"),
		NoProxy:    get("NO_PROXY", "no_proxy"),
	}
	source := ""
	if cfg.HTTPSProxy != "" || cfg.HTTPProxy != "" {
		source = "environment"
	}

	// Layer in system files only to fill gaps the env didn't provide, so an
	// installer that exports the proxy still wins but a proxy configured only in
	// /etc/environment is still detected.
	fileVals := netProxyFromFiles()
	if cfg.HTTPProxy == "" {
		cfg.HTTPProxy = fileVals["http_proxy"]
	}
	if cfg.HTTPSProxy == "" {
		cfg.HTTPSProxy = fileVals["https_proxy"]
	}
	if cfg.NoProxy == "" {
		cfg.NoProxy = fileVals["no_proxy"]
	}
	if source == "" && (cfg.HTTPSProxy != "" || cfg.HTTPProxy != "") {
		source = fileVals["__source__"]
	}
	return cfg, source
}

// netProxyFromFiles scans the system files an installer-invoked process may
// inherit proxy settings from. Best-effort: any unreadable/missing file is
// silently skipped (robustness over completeness). Keys are lower-cased
// http_proxy/https_proxy/no_proxy; "__source__" names the file a proxy came
// from.
func netProxyFromFiles() map[string]string {
	out := map[string]string{}
	setIfEmpty := func(key, val, src string) {
		val = strings.Trim(strings.TrimSpace(val), `"'`)
		if val == "" {
			return
		}
		if _, ok := out[key]; !ok {
			out[key] = val
			if (key == "http_proxy" || key == "https_proxy") && out["__source__"] == "" {
				out["__source__"] = src
			}
		}
	}

	// /etc/environment: KEY=VALUE or KEY="VALUE", possibly with `export `.
	if data, err := os.ReadFile("/etc/environment"); err == nil {
		for _, ln := range strings.Split(string(data), "\n") {
			ln = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(ln), "export "))
			if ln == "" || strings.HasPrefix(ln, "#") {
				continue
			}
			k, v, ok := strings.Cut(ln, "=")
			if !ok {
				continue
			}
			switch strings.ToLower(strings.TrimSpace(k)) {
			case "http_proxy":
				setIfEmpty("http_proxy", v, "/etc/environment")
			case "https_proxy":
				setIfEmpty("https_proxy", v, "/etc/environment")
			case "no_proxy":
				setIfEmpty("no_proxy", v, "/etc/environment")
			}
		}
	}

	// systemd drop-ins for the runos service: Environment="https_proxy=...".
	if matches, _ := filepath.Glob("/etc/systemd/system/runos.service.d/*"); len(matches) > 0 {
		for _, f := range matches {
			data, err := os.ReadFile(f)
			if err != nil {
				continue
			}
			for _, ln := range strings.Split(string(data), "\n") {
				ln = strings.TrimSpace(ln)
				v := strings.TrimPrefix(ln, "Environment=")
				if v == ln {
					continue
				}
				v = strings.Trim(v, `"'`)
				k, val, ok := strings.Cut(v, "=")
				if !ok {
					continue
				}
				switch strings.ToLower(strings.TrimSpace(k)) {
				case "http_proxy":
					setIfEmpty("http_proxy", val, f)
				case "https_proxy":
					setIfEmpty("https_proxy", val, f)
				case "no_proxy":
					setIfEmpty("no_proxy", val, f)
				}
			}
		}
	}

	// apt proxy config: Acquire::http::Proxy "http://proxy:3128";.
	if matches, _ := filepath.Glob("/etc/apt/apt.conf.d/*"); len(matches) > 0 {
		for _, f := range matches {
			name := strings.ToLower(filepath.Base(f))
			if !strings.Contains(name, "proxy") {
				continue
			}
			data, err := os.ReadFile(f)
			if err != nil {
				continue
			}
			for _, ln := range strings.Split(string(data), "\n") {
				low := strings.ToLower(ln)
				val := netAptProxyValue(ln)
				if val == "" {
					continue
				}
				if strings.Contains(low, "::https::proxy") {
					setIfEmpty("https_proxy", val, f)
				} else if strings.Contains(low, "::http::proxy") {
					setIfEmpty("http_proxy", val, f)
				}
			}
		}
	}
	return out
}

// netAptProxyValue pulls the quoted value out of an apt Acquire::*::Proxy line,
// returning "" if there isn't one (or it's the explicit DIRECT/empty form).
func netAptProxyValue(line string) string {
	start := strings.Index(line, `"`)
	if start < 0 {
		return ""
	}
	rest := line[start+1:]
	end := strings.Index(rest, `"`)
	if end < 0 {
		return ""
	}
	v := strings.TrimSpace(rest[:end])
	if v == "" || strings.EqualFold(v, "DIRECT") {
		return ""
	}
	return v
}

// netProxyReachable confirms the proxy authority itself accepts TCP, so we can
// tell "proxy is set but down" apart from "proxy works, high ports blocked".
// Returns true if reachable or if the proxy URL can't be parsed (don't block on
// an ambiguous value).
func netProxyReachable(proxyURL string) bool {
	raw := strings.TrimSpace(proxyURL)
	if raw == "" {
		return true
	}
	if !strings.Contains(raw, "://") {
		raw = "http://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return true
	}
	host := u.Hostname()
	port := u.Port()
	if port == "" {
		port = "80"
		if strings.EqualFold(u.Scheme, "https") {
			port = "443"
		}
	}
	conn, err := net.DialTimeout("tcp", net.JoinHostPort(host, port), netDialTimeout)
	if err != nil {
		return false
	}
	conn.Close()
	return true
}

// checkProxyAndNodewardDirectPath verifies that when an HTTP proxy is configured
// (the common enterprise/cloud-image case), the Nodeward gRPC high ports still
// have a working DIRECT route. It prevents the cryptic failure where HTTPS works
// through the proxy so the network "looks fine", but node registration hangs
// because gRPC to <host>:9191/9192 never traverses an HTTP proxy and the only
// permitted egress is proxied.
func checkProxyAndNodewardDirectPath() error {
	cfg, source := netProxyConfig()
	proxyVal := cfg.HTTPSProxy
	if proxyVal == "" {
		proxyVal = cfg.HTTPProxy
	}
	if proxyVal == "" {
		// No proxy configured -> this check has nothing to assert.
		return nil
	}

	host := netNodewardHost()
	if host == "" {
		roslog.W("proxy is configured but no Nodeward host is known; skipping direct-path probe", nil)
		return nil
	}

	// Is the proxy itself even up? If not, that's a different problem; note it
	// but don't claim a direct-path issue.
	if !netProxyReachable(proxyVal) {
		roslog.W("configured HTTP proxy did not accept a TCP connection", nil, "proxy", proxyVal)
	}

	// Does no_proxy already exempt the Nodeward host? If so a direct path is the
	// intended behaviour; we still verify the ports actually connect below.
	inNoProxy := netHostInNoProxy(cfg, host, 9191)

	// Raw, direct dials (gRPC never proxies) to both high ports.
	var failed []int
	for _, p := range netNodewardPorts {
		if err := netDial(host, p.port); err != nil {
			failed = append(failed, p.port)
		}
	}

	if len(failed) == 0 {
		// Direct path works regardless of the proxy: nothing to block on.
		return nil
	}

	noProxyHint := ""
	if !inNoProxy {
		noProxyHint = fmt.Sprintf("\n  - add %s to no_proxy so tooling that DOES consult it stays direct", host)
	}
	srcHint := ""
	if source != "" {
		srcHint = fmt.Sprintf(" (from %s)", source)
	}
	return fmt.Errorf(
		"An HTTP proxy is configured%s (proxy=%s), but the RunOS agent connects to Nodeward %s on ports %s over gRPC, which does NOT use an HTTP proxy. Direct (no proxy) TCP to those ports FAILED.\n\nThis network appears to only allow proxied egress, so the agent cannot register. Provide a DIRECT route to Nodeward:\n  - allow outbound TCP to %s:9191 and %s:9192 without the proxy%s\nVerify with: nc -vz %s 9191 && nc -vz %s 9192\nThen re-run 'sudo runos preflight'.",
		srcHint, proxyVal, host, netPortList(failed), host, host, noProxyHint, host, host)
}

// netHostInNoProxy reports whether host:port is exempted by the merged no_proxy
// rules, using Go's own httpproxy matching so it agrees with the agent's HTTP
// client. On any parse trouble it returns false (safer to probe than to assume
// exemption).
func netHostInNoProxy(cfg *httpproxy.Config, host string, port int) bool {
	if cfg == nil || strings.TrimSpace(cfg.NoProxy) == "" {
		return false
	}
	u := &url.URL{Scheme: "https", Host: net.JoinHostPort(host, fmt.Sprintf("%d", port))}
	proxyURL, err := cfg.ProxyFunc()(u)
	if err != nil {
		return false
	}
	// ProxyFunc returns nil when the host is exempt (goes direct).
	return proxyURL == nil
}

// netPortList renders a list of ports like "9191, 9192".
func netPortList(ports []int) string {
	parts := make([]string, 0, len(ports))
	for _, p := range ports {
		parts = append(parts, fmt.Sprintf("%d", p))
	}
	return strings.Join(parts, ", ")
}

// checkNodewardHighPortVs443 distinguishes a high-port egress firewall from a
// down/unreachable host. If <host>:443 connects but :9191/:9192 time out, the
// host is plainly routable and the only thing wrong is that the firewall blocks
// the non-standard gRPC ports. This prevents operators from chasing "Nodeward is
// down" when the real cause is a security-group/NACL that only permits 443.
func checkNodewardHighPortVs443() error {
	host := netNodewardHost()
	if host == "" {
		// Installer path must know the host; with a baked-in default this should
		// not happen. Be conservative and don't block on a missing value.
		roslog.W("no Nodeward host known (no --server and no configured default); skipping high-port classification", nil)
		return nil
	}

	var highFailed []int
	for _, p := range netNodewardPorts {
		if err := netDial(host, p.port); err != nil {
			highFailed = append(highFailed, p.port)
		}
	}
	if len(highFailed) == 0 {
		// High ports are fine; nothing for this check to classify.
		return nil
	}

	// High ports failed. Is 443 reachable? If so, this is specifically a
	// high-port block, which is the actionable finding here.
	if err := netDial(host, 443); err != nil {
		// 443 also unreachable -> host-down / total egress block. The generic
		// nodeward-reachable check already reports that; don't double-report a
		// misleading "high port" cause.
		roslog.W("Nodeward unreachable on both 443 and the high ports; deferring to the reachability check", nil, "host", host)
		return nil
	}

	return fmt.Errorf(
		"Nodeward %s is reachable on 443 but NOT on %s (connection timed out). Your egress firewall blocks these non-standard high ports.\n\nRunOS needs outbound TCP to %s:9191 (registration) and %s:9192 (operations). Open them with your network/security team (or the cloud security group / NACL).\nVerify with: nc -vz %s 9191 && nc -vz %s 9192\nThen re-run 'sudo runos preflight'.",
		host, netPortList(highFailed), host, host, host, host)
}

// checkNodewardTlsHandshakePinned performs the SAME pinned TLS handshake the
// L1Sec registration channel uses (TLS1.2+, ServerName=<host>, RootCAs = ONLY
// the pinned RunOS public CA). It catches a TLS-intercepting proxy / corporate
// MITM that a plain TCP probe cannot see: interception breaks registration and
// can silently corrupt the integrity-checked binary download. It BLOCKS only on
// an actual handshake/verify failure; a missing CA file or an unreachable port
// degrades to a warning so it never produces a false positive.
func checkNodewardTlsHandshakePinned() error {
	host := strings.TrimSpace(server)
	if host == "" {
		roslog.W("preflight ran without --server; skipping pinned TLS handshake probe", nil)
		return nil
	}

	caPath := config.GetPublicCACertPath()
	pinned := false
	var roots *x509.CertPool
	if caPath != "" {
		if pem, err := os.ReadFile(caPath); err == nil && len(pem) > 0 {
			pool := x509.NewCertPool()
			if pool.AppendCertsFromPEM(pem) {
				roots = pool
				pinned = true
			}
		}
	}
	if !pinned {
		// No usable pinned CA on disk (checked separately by l1sec-ca-file). We
		// can't enforce the pin, so don't manufacture a MITM verdict.
		roslog.W("pinned L1Sec CA not available; skipping pinned-handshake verification", nil, "path", caPath)
		return nil
	}

	addr := net.JoinHostPort(host, "9191")
	dialer := &net.Dialer{Timeout: netDialTimeout}
	rawConn, err := dialer.Dial("tcp", addr)
	if err != nil {
		// Port unreachable is a connectivity problem reported by the reachability
		// / high-port checks, not a TLS-interception finding.
		roslog.W("could not open TCP to Nodeward 9191 for the TLS probe; deferring to reachability checks", err, "addr", addr)
		return nil
	}
	defer rawConn.Close()
	_ = rawConn.SetDeadline(time.Now().Add(netDialTimeout))

	tlsConn := tls.Client(rawConn, &tls.Config{
		MinVersion:         tls.VersionTLS12,
		ServerName:         host,
		RootCAs:            roots,
		InsecureSkipVerify: false,
	})
	if err := tlsConn.Handshake(); err != nil {
		cdnHost := netCDNHost()
		if cdnHost == "" {
			cdnHost = "<cdn-host>"
		}
		return fmt.Errorf(
			"Reached Nodeward on 9191 but the secure handshake FAILED: %v\n\nThe presented certificate is not trusted by the pinned RunOS CA (or its hostname does not match), which indicates a TLS-intercepting proxy or a network MITM. RunOS pins the Nodeward L1Sec CA, so interception breaks registration and can corrupt the integrity-checked binary download.\n\nExempt these hosts from TLS inspection: %s, github.com, objects.githubusercontent.com, ghcr.io, quay.io, pkgs.k8s.io, %s\nThen re-run preflight over a clean path.",
			err, host, cdnHost)
	}
	tlsConn.Close()
	return nil
}

// checkEgressEndpointSetComplete probes EVERY HTTPS endpoint the install pulls
// from (not a sampled subset), via Go net/http with ProxyFromEnvironment so the
// verdict matches the agent's real egress. It prevents the late, opaque failure
// where one missing firewall/proxy allowlist entry (e.g. registry-1.docker.io or
// the CDN) lets registration succeed but image/binary pulls fail mid-install.
// Any non-5xx response counts as reachable (ghcr.io/quay.io answer 401 at /v2/);
// 429 is noted as rate-limiting but not treated as unreachable.
func checkEgressEndpointSetComplete() error {
	type target struct{ host, path, why string }
	targets := make([]target, 0, len(netEgressEndpoints)+1)
	for _, e := range netEgressEndpoints {
		targets = append(targets, target{e.host, e.path, e.why})
	}
	if cdn := netCDNHost(); cdn != "" {
		targets = append(targets, target{cdn, "/", "the L1Sec CA / artifacts from the CDN"})
	} else {
		roslog.W("no --cdn provided; CDN host not probed in egress check", nil)
	}

	var unreachable []string
	var rateLimited []string
	for _, t := range targets {
		code, err := netProbeHTTPS(t.host, t.path)
		if err != nil {
			unreachable = append(unreachable, fmt.Sprintf("%s (%s): %s", t.host, t.why, netSummarizeErr(err)))
			continue
		}
		if code == 429 {
			rateLimited = append(rateLimited, t.host)
			continue
		}
		if code >= 500 {
			// A 5xx is the endpoint's own problem, not an allowlist gap. Don't
			// block egress on a transient upstream error.
			roslog.W("egress endpoint returned a server error; not treated as blocked", nil, "host", t.host, "code", code)
			continue
		}
		// Any other status (200/301/401/403/...) proves we reached it.
	}

	if len(rateLimited) > 0 {
		roslog.W("egress endpoints returned 429 (rate-limited); reachable but throttled", nil, "hosts", strings.Join(rateLimited, ", "))
	}

	if len(unreachable) == 0 {
		return nil
	}

	return fmt.Errorf(
		"Cannot reach required HTTPS endpoint(s) on 443:\n  - %s\n\nThe install needs these to pull the node binary, the L1Sec CA, and container images; your firewall or proxy allowlist is missing one or more. Allow HTTPS egress to ALL of: github.com, objects.githubusercontent.com, pkgs.k8s.io, registry.k8s.io, registry-1.docker.io, ghcr.io, quay.io, helm.cilium.io, and the CDN host.\nThen re-run 'sudo runos preflight'.",
		strings.Join(unreachable, "\n  - "))
}

// netSummarizeErr turns a probe error into a short operator-readable cause.
func netSummarizeErr(err error) string {
	if err == nil {
		return ""
	}
	msg := err.Error()
	switch {
	case strings.Contains(msg, "no such host") || strings.Contains(msg, "name resolution") || strings.Contains(msg, "server misbehaving"):
		return "DNS resolution failed"
	case strings.Contains(msg, "connection refused"):
		return "connection refused"
	case strings.Contains(msg, "timeout") || strings.Contains(msg, "deadline exceeded") || strings.Contains(msg, "Client.Timeout"):
		return "timed out (likely firewall/proxy block)"
	case strings.Contains(msg, "certificate") || strings.Contains(msg, "x509"):
		return "TLS certificate error (possible interception)"
	case strings.Contains(msg, "proxyconnect"):
		return "could not connect through the configured proxy"
	default:
		return msg
	}
}

// checkCaptivePortalContentCanary validates response CONTENT, not just a status
// code: it fetches an endpoint whose answer is a known empty 204, and a known
// plain-text checksum file, and flags an HTML/login interstitial or unexpected
// redirect. It prevents the insidious case where plain "reachability" passes but
// a captive portal substitutes its login page for downloads, so artifacts later
// fail sha256/CA-parse with no obvious network cause.
func checkCaptivePortalContentCanary() error {
	// 204 canaries: a correct network returns 204 with an empty body. A portal
	// returns 200 + HTML, or redirects to its login host.
	canaries := []string{
		"http://connectivity-check.ubuntu.com/",
		"https://www.google.com/generate_204",
	}
	for _, c := range canaries {
		verdict, ok := netCanary204(c)
		if !ok {
			// Could not get a clear read (transport error, ambiguous) -> try the
			// next canary rather than blocking.
			continue
		}
		if verdict != "" {
			return fmt.Errorf(
				"A captive portal or login gateway is intercepting traffic: %s\n\nDownloads will be replaced by the portal's login page and fail integrity checks (sha256 / CA parse) even though plain reachability appears to 'pass'. Complete the network login in a browser on this network, or move the node to a network with unauthenticated egress, then re-run 'sudo runos preflight'.",
				verdict)
		}
		// Got a clean 204 from at least one canary -> egress is unauthenticated.
		break
	}

	// Content sanity on a real artifact path: GitHub release checksums should be
	// plain text, never HTML.
	if body, ok := netFetchSmall("https://raw.githubusercontent.com/runos-official/cli/main/README.md"); ok {
		if strings.Contains(strings.ToLower(body), "<html") || strings.Contains(strings.ToLower(body), "<!doctype html") {
			return fmt.Errorf(
				"A captive portal or proxy returned an HTML page where a plain-text file was expected (raw.githubusercontent.com).\n\nDownloads will be replaced by an interstitial and fail integrity checks. Complete any network login in a browser, or move to a network with unauthenticated egress, then re-run 'sudo runos preflight'.")
		}
	}

	return nil
}

// netCanary204 fetches url (no redirect following) and classifies the result.
// It returns (verdict, ok): ok=false means the result was ambiguous (skip);
// ok=true with verdict=="" means a clean 204 (good); verdict!="" describes a
// detected portal.
func netCanary204(rawURL string) (string, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), netHTTPTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return "", false
	}
	req.Header.Set("User-Agent", "runos-preflight/1")
	resp, err := netHTTPClient(false).Do(req)
	if err != nil {
		return "", false
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 8192))

	// A redirect where a 204 was expected, pointing somewhere other than the
	// canary host, is a strong portal signal.
	if resp.StatusCode >= 300 && resp.StatusCode < 400 {
		loc := resp.Header.Get("Location")
		if loc != "" && !netSameHost(loc, rawURL) {
			return fmt.Sprintf("a request to %s that should return an empty 204 was redirected to %s (a login gateway)", rawURL, netHostOf(loc)), true
		}
		// Ambiguous redirect (e.g. http->https same host): don't decide.
		return "", false
	}

	if resp.StatusCode == 204 && len(strings.TrimSpace(string(body))) == 0 {
		return "", true
	}

	if resp.StatusCode == 200 {
		low := strings.ToLower(string(body))
		if strings.Contains(low, "<html") || strings.Contains(low, "<!doctype html") || len(strings.TrimSpace(string(body))) > 0 {
			return fmt.Sprintf("a request to %s that should return an empty 204 returned HTTP 200 with a non-empty/HTML body", rawURL), true
		}
	}

	// Other statuses (e.g. blocked, 5xx): ambiguous, not a portal proof.
	return "", false
}

// netFetchSmall does a bounded GET and returns the first few KiB of the body.
func netFetchSmall(rawURL string) (string, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), netHTTPTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return "", false
	}
	req.Header.Set("User-Agent", "runos-preflight/1")
	resp, err := netHTTPClient(true).Do(req)
	if err != nil {
		return "", false
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return "", false
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8192))
	if err != nil {
		return "", false
	}
	return string(body), true
}

// netSameHost reports whether loc (which may be relative) targets the same host
// as base. A relative Location is same-host by definition.
func netSameHost(loc, base string) bool {
	lu, err := url.Parse(loc)
	if err != nil {
		return true
	}
	if lu.Host == "" {
		return true
	}
	bu, err := url.Parse(base)
	if err != nil {
		return false
	}
	return strings.EqualFold(lu.Hostname(), bu.Hostname())
}

// netHostOf returns the host part of a URL, or the raw string if unparseable.
func netHostOf(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Hostname() == "" {
		return raw
	}
	return u.Hostname()
}

// checkDnsAnswerSanity validates DNS ANSWERS, not merely that resolution
// returns something. It warns (never blocks: DNS topologies vary too much to be
// certain) when a guaranteed-nonexistent name resolves (NXDOMAIN hijack /
// captive resolver) or when a public endpoint resolves to an RFC1918 / loopback
// / link-local address (split-horizon or blackhole DNS). It prevents the failure
// where the install silently talks to the wrong host or resolves intermittently.
func checkDnsAnswerSanity() error {
	ctx, cancel := context.WithTimeout(context.Background(), netHTTPTimeout)
	defer cancel()
	resolver := net.DefaultResolver

	var notes []string

	// 1. NXDOMAIN hijack: a name that must not exist should NOT resolve.
	bogus := "nxdomain-rnos-preflight-zzq7.runos-preflight.invalid"
	if addrs, err := resolver.LookupHost(ctx, bogus); err == nil && len(addrs) > 0 {
		notes = append(notes, fmt.Sprintf("a guaranteed-nonexistent name resolved to %s (NXDOMAIN hijack / captive resolver)", strings.Join(netLimit(addrs, 3), ", ")))
	}

	// 2. Public endpoints resolving to private/loopback addresses.
	for _, host := range []string{"github.com", "registry-1.docker.io", "pkgs.k8s.io"} {
		addrs, err := resolver.LookupHost(ctx, host)
		if err != nil {
			// Resolution failure is reported by checkDNSResolution; skip here.
			continue
		}
		var bad []string
		for _, a := range addrs {
			if netIsPrivateOrLocal(a) {
				bad = append(bad, a)
			}
		}
		if len(bad) > 0 {
			notes = append(notes, fmt.Sprintf("%s resolved to a private/loopback address %s (split-horizon or blackhole DNS)", host, strings.Join(netLimit(bad, 3), ", ")))
		}
	}

	if len(notes) == 0 {
		return nil
	}

	return fmt.Errorf(
		"DNS is returning suspicious answers:\n  - %s\n\nThe install may connect to the wrong server or resolve intermittently. Use a trustworthy resolver (your cloud VPC resolver, or 1.1.1.1 / 8.8.8.8), allow UDP+TCP port 53, then confirm with 'dig github.com' and that 'dig %s' returns NXDOMAIN, and re-run 'sudo runos preflight'.",
		strings.Join(notes, "\n  - "), bogus)
}

// netLimit returns at most n elements of s (for compact messages).
func netLimit(s []string, n int) []string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// netIsPrivateOrLocal reports whether addr is an RFC1918/loopback/link-local IP.
// A non-IP or public address returns false.
func netIsPrivateOrLocal(addr string) bool {
	ip := net.ParseIP(addr)
	if ip == nil {
		return false
	}
	return ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsUnspecified()
}
