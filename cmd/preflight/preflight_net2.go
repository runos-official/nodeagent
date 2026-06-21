package preflight

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/runos-official/nodeagent/commons"
	"github.com/runos-official/nodeagent/roslog"
)

// --- shared local constants/helpers for the net2 group (prefix: nw) ---

// nwExecTimeout bounds every external command this group runs so a hung apt /
// firewall tool can never stall preflight.
const nwExecTimeout = 8 * time.Second

// nwNetTimeout bounds network probes (HEAD requests, UDP round-trips).
const nwNetTimeout = 6 * time.Second

// nwRun executes name+args under a timeout and returns combined stdout+stderr
// plus the error. Missing binaries / timeouts return a non-nil error; callers
// MUST treat that as "could not determine" and not block.
func nwRun(name string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), nwExecTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, name, args...).CombinedOutput()
	return string(out), err
}

// nwHave reports whether a binary is on PATH.
func nwHave(name string) bool {
	_, err := exec.LookPath(name)
	return err == nil
}

// nwReadFile reads a small file, returning "" on any error.
func nwReadFile(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return string(b)
}

// nwHTTPClient returns an http.Client that honours the environment proxy (so a
// probe matches what the agent/apt actually does) with a bounded timeout.
func nwHTTPClient() *http.Client {
	return &http.Client{
		Timeout: nwNetTimeout,
		Transport: &http.Transport{
			Proxy:             http.ProxyFromEnvironment,
			DisableKeepAlives: true,
		},
		// Follow redirects (mirrors often 30x to a regional host).
	}
}

// checkAptSourcesUsable verifies the apt layer this node will use is actually
// usable end to end, going well beyond a bare `apt-get update`. It prevents the
// classic mid-install failure where `apt-get install wireguard containerd ...`
// dies with a cryptic apt error: an unreachable Ubuntu mirror, a third-party
// repo whose GPG key is missing/expired (which poisons ALL of `apt-get update`),
// a clock-driven "Release is not valid yet" rejection (a clock bug, not a repo
// bug), a k8s repo that serves HTML instead of a key, dpkg left half-configured,
// broken dependencies, kube*/containerd pinned on hold, or `universe` disabled
// so wireguard/dnsmasq have no install candidate. Block only on a confidently
// determined cause; anything ambiguous returns nil.
func checkAptSourcesUsable() error {
	// Non-apt systems: nothing to assert.
	if !nwHave("apt-get") {
		roslog.W("apt-get not found; skipping apt-sources check", nil)
		return nil
	}

	// (5a) dpkg half-configured -> deterministic, name the exact remedy.
	if nwHave("dpkg") {
		if out, err := nwRun("dpkg", "--audit"); err == nil {
			if strings.TrimSpace(out) != "" {
				return fmt.Errorf("dpkg has half-configured/half-installed packages; the install's apt-get steps will abort\n\ndpkg --audit reported:\n%s\nFix with:\n  sudo dpkg --configure -a\nThen run 'sudo apt-get -f install' and re-run preflight.",
					nwIndentBlock(out))
			}
		}
	}

	// (5b) Broken dependencies -> apt-get check exits non-zero.
	if out, err := nwRun("apt-get", "check"); err != nil {
		// Non-zero exit from apt-get check means broken deps (or a transient
		// lock; the package-locks check covers locks, so attribute to deps but
		// only when output actually names unmet dependencies).
		low := strings.ToLower(out)
		if strings.Contains(low, "unmet dependencies") || strings.Contains(low, "broken") {
			return fmt.Errorf("apt reports broken/unmet dependencies; the install's apt-get steps will fail\n\napt-get check reported:\n%s\nFix with:\n  sudo apt-get -f install\nThen re-run preflight.",
				nwIndentBlock(out))
		}
		// Otherwise ambiguous (could be a lock or transient) -> don't block here.
	}

	// (6) kube*/containerd held -> apt-get install silently skips them.
	if nwHave("apt-mark") {
		if out, err := nwRun("apt-mark", "showhold"); err == nil {
			var held []string
			for _, ln := range strings.Split(out, "\n") {
				p := strings.TrimSpace(ln)
				if p == "" {
					continue
				}
				if strings.HasPrefix(p, "kube") || strings.Contains(p, "containerd") || strings.Contains(p, "cri-tools") {
					held = append(held, p)
				}
			}
			if len(held) > 0 {
				return fmt.Errorf("these packages are pinned on apt hold, so the install cannot upgrade/install them: %s\n\nRelease them with:\n  sudo apt-mark unhold %s\nThen re-run preflight.",
					strings.Join(held, ", "), strings.Join(held, " "))
			}
		}
	}

	// (1) Reachability of the node's REAL configured mirrors. We HEAD each
	// distinct mirror root; a mirror that times out / DNS-fails is a confident
	// block. We deliberately do NOT block on a single 404 of the root (some
	// mirrors 403/404 a bare GET of the root) — only on no-response.
	for _, host := range nwAptMirrorURLs() {
		if reason, blocked := nwProbeMirrorUnreachable(host); blocked {
			return fmt.Errorf("the apt mirror this node is configured to use is unreachable: %s (%s)\n\nThis breaks every 'apt-get update'/'apt-get install' during the install. Check egress/DNS to this host (and any HTTP(S) proxy), then run 'sudo apt-get update' until clean and re-run preflight.\nThis is pre-existing apt egress on your machine.",
				host, reason)
		}
	}

	// (4) The exact k8s Release.key apt will fetch must look like a PGP key, not
	// an HTML error/captive-portal page. Only block when we get a clear,
	// non-empty NON-key 200 body; network errors are inconclusive -> skip.
	if v := nwK8sRepoVersion(); v != "" {
		keyURL := fmt.Sprintf("https://pkgs.k8s.io/core:/stable:/%s/deb/Release.key", v)
		if verdict := nwClassifyKeyURL(keyURL); verdict != "" {
			return fmt.Errorf("the Kubernetes apt repo key URL is not serving a usable signing key: %s\n\nURL: %s\nIf a proxy or captive portal is rewriting HTTPS responses, fix that. Otherwise verify the configured Kubernetes minor version. Then run 'sudo apt-get update' until clean and re-run preflight.",
				verdict, keyURL)
		}
	}

	// (2)+(3) Run a real, bounded `apt-get update` and classify its failure
	// class precisely (GPG vs clock vs 404 vs proxy). This is the supersede of
	// the old checkBrokenAptSources.
	out, err := nwRun("apt-get", "update", "-qq")
	if err != nil {
		low := strings.ToLower(out)
		switch {
		// (3) Clock-driven: attribute to the clock, not the repo.
		case nwContainsAny(low, "not valid yet", "is in the future", "invalid for another", "updates are not yet"):
			return fmt.Errorf("apt is rejecting a repository Release file as 'not valid yet/in the future' — this is a CLOCK problem on this node, not a broken repo\n\napt said:\n%s\nFix the clock, then re-run:\n  sudo timedatectl set-ntp true\n  sudo systemctl restart systemd-timesyncd 2>/dev/null || true\nWait ~10s, confirm 'timedatectl' shows synchronized, then re-run preflight.",
				nwIndentBlock(nwTrimAptNoise(out)))
		// (2) GPG/signing class: one bad third-party key breaks ALL updates.
		case nwContainsAny(low, "no_pubkey", "expkeysig", "keyexpired", "not signed", "no_data", "badsig", "is not signed"):
			return fmt.Errorf("a repository signing key is missing/expired, which makes EVERY 'apt-get update' fail (so the install cannot fetch any package)\n\napt said:\n%s\nFind the offending repo line and fix or remove it. For a missing key (NO_PUBKEY <KEYID>):\n  sudo gpg --keyserver keyserver.ubuntu.com --recv-keys <KEYID> && sudo gpg --export <KEYID> | sudo tee /etc/apt/trusted.gpg.d/<name>.gpg >/dev/null\nFor an expired/removed third-party repo, delete its file under /etc/apt/sources.list.d/. Then run 'sudo apt-get update' until clean and re-run preflight.",
				nwIndentBlock(nwTrimAptNoise(out)))
		// Proxy/auth class.
		case nwContainsAny(low, "407 ", "proxy authentication", "could not resolve 'proxy", "tunnel connection failed"):
			return fmt.Errorf("apt cannot use the configured HTTP(S) proxy (authentication/tunnel failure), so package fetches will fail\n\napt said:\n%s\nFix the proxy settings apt uses (env http_proxy/https_proxy and /etc/apt/apt.conf.d/*proxy*), then run 'sudo apt-get update' until clean and re-run preflight.",
				nwIndentBlock(nwTrimAptNoise(out)))
		// 404 / missing Release file class.
		case nwContainsAny(low, "does not have a release file", "404  not found", "404 not found", "failed to fetch"):
			return fmt.Errorf("apt cannot fetch a repository's Release file (404 / missing), so 'apt-get update' fails for the install\n\napt said:\n%s\nA repo under /etc/apt/sources.list.d/ points at a path/suite that no longer exists; correct or remove it, then run 'sudo apt-get update' until clean and re-run preflight.",
				nwIndentBlock(nwTrimAptNoise(out)))
		default:
			// apt-get update failed but we cannot confidently classify why.
			// Returning a vague block risks a false positive (transient mirror
			// blip, momentary lock). Warn softly and let other network checks
			// speak; do NOT block.
			roslog.W("apt-get update returned non-zero but the cause was unclear; not blocking", err, "output", nwTrimAptNoise(out))
			return nil
		}
	}

	// (7) universe must be enabled or wireguard/dnsmasq have no candidate. Only
	// assert when apt-cache exists AND apt-get update succeeded above (otherwise
	// the cache is stale and "no candidate" would be a false positive).
	if nwHave("apt-cache") {
		var missing []string
		for _, pkg := range []string{"wireguard", "dnsmasq"} {
			out, err := nwRun("apt-cache", "policy", pkg)
			if err != nil {
				continue // inconclusive
			}
			if nwNoCandidate(out) {
				missing = append(missing, pkg)
			}
		}
		// Require BOTH absent before blaming universe; a single missing package
		// could be a renamed metapackage and we don't want a false positive.
		if len(missing) >= 2 {
			return fmt.Errorf("required packages have no install candidate (%s), which usually means the 'universe' component is disabled\n\nEnable it and refresh:\n  sudo add-apt-repository -y universe || sudo sed -i 's/^# *\\(deb .*universe\\)/\\1/' /etc/apt/sources.list\n  sudo apt-get update\nThen re-run preflight.",
				strings.Join(missing, ", "))
		}
	}

	return nil
}

// checkOutboundUdpForWireguard verifies that outbound UDP egress (the transport
// WireGuard uses on UDP 51820-51821 for the node-to-node mesh) is not obviously
// blocked, and that no local firewall default-denies outbound UDP. It is a WARN
// only: a single short UDP probe can false-negative, the real WireGuard peer
// cannot be contacted at preflight, and single-node installs never need this.
// It catches the silent failure mode where TCP egress works (so every other
// check passes) but UDP is dropped, and multi-node clustering later fails to
// form the WireGuard mesh with no clear error.
func checkOutboundUdpForWireguard() error {
	// 1) Best-effort UDP round trip to public responders. Success on ANY one
	// means UDP egress + return path works; we then only need to inspect local
	// firewall posture for an explicit 51820 block.
	udpWorks := nwUDPEgressWorks()

	// 2) Local firewall posture: explicit default-deny outbound UDP without a
	// 51820 allow.
	fwDeniesUDP, fwDetail := nwFirewallDeniesOutboundUDP()

	if udpWorks && !fwDeniesUDP {
		return nil // healthy
	}

	var cause string
	switch {
	case fwDeniesUDP && !udpWorks:
		cause = fmt.Sprintf("no UDP probe (STUN/DNS/NTP) got a reply AND a local firewall default-denies outbound UDP (%s)", fwDetail)
	case fwDeniesUDP:
		cause = fmt.Sprintf("a local firewall default-denies outbound UDP with no allow for 51820-51821/udp (%s)", fwDetail)
	default:
		// Only UDP probe failed. This is the most false-negative-prone signal,
		// so phrase it as "appears" and keep it a warning.
		cause = "no outbound UDP probe (STUN/DNS/NTP) got a reply, though TCP egress works (a short UDP probe can false-negative)"
	}

	return fmt.Errorf("outbound UDP egress appears restricted: %s\n\nRunOS nodes peer over WireGuard on UDP 51820-51821; if your firewall or cloud security group blocks outbound (and, for control planes, inbound) UDP, multi-node clustering will fail even though this node installs fine. Allow it, then re-run:\n  sudo ufw allow out 51820:51821/udp   # ufw\n  # firewalld: sudo firewall-cmd --permanent --add-port=51820-51821/udp && sudo firewall-cmd --reload\n  # cloud: open UDP 51820-51821 egress (and inbound on control planes) in the security group\nSingle-node installs are unaffected.",
		cause)
}

// checkNATEndpointCollision detects that this node sits behind NAT (its primary
// RFC1918 interface address differs from its detected public IP) and warns about
// the WireGuard endpoint-collision foot-gun: WireGuard keys peers by their public
// endpoint, so two RunOS nodes behind the SAME public IP collide and only one
// tunnel stays up, and same-NAT peers additionally need NAT hairpin support. It
// is a WARN: a single node behind NAT is perfectly fine and peers cannot be
// enumerated at preflight. Returns nil when the node is directly routable or the
// public IP cannot be determined (inconclusive must not block).
func checkNATEndpointCollision() error {
	ifaceIP := nwPrimaryPrivateIPv4()
	if ifaceIP == "" {
		// No RFC1918 primary -> node is likely directly addressed; nothing to warn.
		return nil
	}

	extIP, err := commons.GetExternalIPAddress()
	if err != nil || strings.TrimSpace(extIP) == "" {
		// Can't determine public IP (no egress to the IP echo services, or
		// offline). Inconclusive -> do not warn.
		roslog.W("could not determine external IP; skipping NAT-collision check", err)
		return nil
	}
	extIP = strings.TrimSpace(extIP)

	if extIP == ifaceIP {
		// Public IP is bound directly to the interface -> not behind NAT.
		return nil
	}

	return fmt.Errorf("this node is behind NAT (private %s vs public %s)\n\nWireGuard identifies peers by their public UDP endpoint. If you place more than one RunOS node behind this same NAT/public IP, their tunnels collide and only one stays up, and same-NAT peers also need NAT hairpin support. Give each node a distinct routable IP, or a distinct inbound UDP 51820 port-forward per node. A single node behind NAT is fine.\nThis is a networking heads-up, not a RunOS limitation.",
		ifaceIP, extIP)
}

// checkHostFirewallEgressPosture inspects (locally, no network) the host's
// firewall posture for a restrictive default that silently strangles Kubernetes
// runtime traffic AFTER install even though some HTTPS worked during install:
// ufw "Default: deny (outgoing)", firewalld active on a soon-to-be k8s node, an
// iptables/nft OUTPUT policy of DROP, a "-P FORWARD DROP" (which breaks pod
// forwarding before Cilium takes over), and stale cali-/cilium/KUBE- chains left
// by a previous CNI. WARN only (intent is ambiguous), but it enumerates which
// required ports lack an explicit allow so the operator knows exactly what to
// open. Prevents the maddening "install succeeded, cluster networking is dead"
// class of failure.
func checkHostFirewallEgressPosture() error {
	var issues []string

	// ufw default-deny outgoing.
	if nwHave("ufw") {
		if out, err := nwRun("ufw", "status", "verbose"); err == nil {
			low := strings.ToLower(out)
			if strings.Contains(low, "status: active") && nwUfwDeniesOutgoing(low) {
				issues = append(issues, "ufw default-deny outgoing")
			}
		}
	}

	// firewalld active on a node about to run k8s.
	if nwHave("firewall-cmd") {
		if out, err := nwRun("firewall-cmd", "--state"); err == nil && strings.Contains(strings.ToLower(out), "running") {
			issues = append(issues, "firewalld is active (its default zone can drop k8s traffic)")
		}
	}

	// iptables OUTPUT/FORWARD policy DROP and stale CNI chains.
	if nwHave("iptables") {
		if out, err := nwRun("iptables", "-S"); err == nil {
			if nwPolicyDrop(out, "OUTPUT") {
				issues = append(issues, "iptables OUTPUT policy is DROP")
			}
			if nwPolicyDrop(out, "FORWARD") {
				issues = append(issues, "iptables FORWARD policy is DROP (breaks pod forwarding)")
			}
			if nwHasStaleCNIChains(out) {
				issues = append(issues, "stale cali-/cilium/KUBE- chains from a prior CNI")
			}
		}
	} else if nwHave("nft") {
		if out, err := nwRun("nft", "list", "ruleset"); err == nil {
			if nwNftOutputPolicyDrop(out) {
				issues = append(issues, "nftables inet filter output policy is drop")
			}
		}
	}

	if len(issues) == 0 {
		return nil
	}

	required := "UDP 51820-51821 (WireGuard), UDP 8472 (Cilium VXLAN), TCP 6443/10250/2379/2380/6446 (Kubernetes), TCP 443 (registries/CDN), UDP+TCP 53 (DNS)"
	return fmt.Errorf("this host has a restrictive firewall posture that can silently drop Kubernetes traffic after install (even though some HTTPS worked): %s\n\nEither relax outbound for the install, or explicitly allow: %s.\nOn a clean host set 'sudo iptables -P FORWARD ACCEPT' (let Cilium manage forwarding) and flush any stale CNI chains, then re-run preflight.\nThis is host firewall configuration, not a RunOS bug.",
		strings.Join(issues, "; "), required)
}

// checkRpFilterAndMultiHome warns ONLY when both risky conditions hold at once:
// the node is multi-homed (more than one default route) AND strict reverse-path
// filtering is on (net.ipv4.conf.all.rp_filter=1). That exact combination makes
// the kernel drop WireGuard/Kubernetes replies that leave via a different NIC
// than they arrived on, breaking the mesh asymmetrically and intermittently. The
// scope is deliberately narrow to keep false positives near zero; either factor
// alone returns nil.
func checkRpFilterAndMultiHome() error {
	if !nwMultipleDefaultRoutes() {
		return nil
	}

	rp := strings.TrimSpace(nwReadFile("/proc/sys/net/ipv4/conf/all/rp_filter"))
	if rp != "1" {
		// 0 (off) or 2 (loose) are safe for asymmetric routing.
		return nil
	}

	return fmt.Errorf("this node is multi-homed (multiple default routes) with strict reverse-path filtering (net.ipv4.conf.all.rp_filter=1)\n\nThe kernel may drop WireGuard and Kubernetes replies that leave via a different NIC than they arrived on, breaking the mesh asymmetrically. Either set a single default route on the interface RunOS will use, or relax rp_filter to loose mode for the mesh interface, then re-run preflight:\n  sudo sysctl -w net.ipv4.conf.all.rp_filter=2\nThis is a host routing configuration issue.")
}

// ---------------- local helpers (prefix nw) ----------------

// nwIndentBlock indents a (possibly multi-line) block by two spaces so it nests
// under the message header in the reporter.
func nwIndentBlock(s string) string {
	s = strings.TrimRight(s, "\n")
	lines := strings.Split(s, "\n")
	for i, ln := range lines {
		lines[i] = "  " + ln
	}
	return strings.Join(lines, "\n") + "\n"
}

// nwContainsAny reports whether haystack contains any of the needles.
func nwContainsAny(haystack string, needles ...string) bool {
	for _, n := range needles {
		if strings.Contains(haystack, n) {
			return true
		}
	}
	return false
}

// nwTrimAptNoise keeps apt output short and signal-dense for the message: drop
// blank lines and progress, cap the number of lines.
func nwTrimAptNoise(s string) string {
	var keep []string
	for _, ln := range strings.Split(s, "\n") {
		t := strings.TrimSpace(ln)
		if t == "" {
			continue
		}
		keep = append(keep, t)
		if len(keep) >= 12 {
			keep = append(keep, "...")
			break
		}
	}
	return strings.Join(keep, "\n")
}

// nwAptSourceFiles returns the apt source files apt actually reads.
func nwAptSourceFiles() []string {
	var files []string
	if _, err := os.Stat("/etc/apt/sources.list"); err == nil {
		files = append(files, "/etc/apt/sources.list")
	}
	for _, dir := range []string{"/etc/apt/sources.list.d"} {
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if e.IsDir() {
				continue
			}
			ext := filepath.Ext(e.Name())
			if ext == ".list" || ext == ".sources" {
				files = append(files, filepath.Join(dir, e.Name()))
			}
		}
	}
	return files
}

// nwAptMirrorURLs parses the configured Ubuntu archive/security mirror hosts
// from apt sources (both classic one-line .list and deb822 .sources) and returns
// a small de-duplicated set of root URLs to HEAD. Cloud images use regional
// mirrors, so we probe the REAL host, not archive.ubuntu.com.
func nwAptMirrorURLs() []string {
	seen := map[string]bool{}
	var urls []string
	add := func(u string) {
		u = strings.TrimSpace(u)
		if u == "" || seen[u] {
			return
		}
		// Skip non-http(s) (cdrom:, file:, copy:) and mirror+file:// indirections.
		if !strings.HasPrefix(u, "http://") && !strings.HasPrefix(u, "https://") {
			return
		}
		seen[u] = true
		urls = append(urls, u)
	}

	uriRe := regexp.MustCompile(`https?://[^\s\]]+`)

	for _, f := range nwAptSourceFiles() {
		content := nwReadFile(f)
		if content == "" {
			continue
		}
		if strings.HasSuffix(f, ".sources") {
			// deb822: lines like "URIs: http://host/ubuntu".
			for _, ln := range strings.Split(content, "\n") {
				t := strings.TrimSpace(ln)
				if strings.HasPrefix(strings.ToLower(t), "uris:") {
					for _, m := range uriRe.FindAllString(t, -1) {
						add(nwMirrorRoot(m))
					}
				}
			}
			continue
		}
		// classic .list: "deb [opts] http://host/ubuntu suite comps".
		for _, ln := range strings.Split(content, "\n") {
			t := strings.TrimSpace(ln)
			if t == "" || strings.HasPrefix(t, "#") {
				continue
			}
			if !strings.HasPrefix(t, "deb") {
				continue
			}
			if m := uriRe.FindString(t); m != "" {
				add(nwMirrorRoot(m))
			}
		}
		if len(urls) >= 6 {
			break // bound the number of probes
		}
	}
	return urls
}

// nwMirrorRoot reduces a mirror URI to scheme://host (root) for a HEAD probe.
func nwMirrorRoot(u string) string {
	// Strip after the host so we HEAD the mirror root rather than a deep path.
	rest := u
	for _, p := range []string{"https://", "http://"} {
		if strings.HasPrefix(rest, p) {
			host := strings.TrimPrefix(rest, p)
			if i := strings.IndexByte(host, '/'); i >= 0 {
				host = host[:i]
			}
			return p + host
		}
	}
	return u
}

// nwProbeMirrorUnreachable HEADs a mirror root and returns (reason, true) only
// when the host is confidently unreachable (DNS failure, connection refused,
// timeout). Any HTTP response — even 403/404/5xx — proves reachability, so we do
// NOT block on those (mirror roots commonly 403 a bare request).
func nwProbeMirrorUnreachable(root string) (string, bool) {
	client := nwHTTPClient()
	req, err := http.NewRequest(http.MethodHead, root, nil)
	if err != nil {
		return "", false // malformed URL -> inconclusive, don't block
	}
	resp, err := client.Do(req)
	if err == nil {
		resp.Body.Close()
		return "", false // any response = reachable
	}
	msg := err.Error()
	low := strings.ToLower(msg)
	switch {
	case strings.Contains(low, "no such host") || strings.Contains(low, "name resolution") || strings.Contains(low, "server misbehaving"):
		return "DNS resolution failed", true
	case strings.Contains(low, "connection refused"):
		return "connection refused", true
	case strings.Contains(low, "timeout") || strings.Contains(low, "deadline exceeded") || strings.Contains(low, "i/o timeout"):
		return "connection timed out (egress firewall or no route)", true
	default:
		// TLS errors, proxy errors, etc. are ambiguous -> don't block.
		return "", false
	}
}

// nwK8sRepoVersion extracts the Kubernetes minor (e.g. "v1.30") from a configured
// pkgs.k8s.io source line, returning "" if none is configured (so we skip the
// key probe rather than guessing a version).
func nwK8sRepoVersion() string {
	re := regexp.MustCompile(`pkgs\.k8s\.io/core:/stable:/(v\d+\.\d+)/deb`)
	for _, f := range nwAptSourceFiles() {
		content := nwReadFile(f)
		if m := re.FindStringSubmatch(content); len(m) == 2 {
			return m[1]
		}
	}
	return ""
}

// nwClassifyKeyURL fetches a Release.key URL and returns a non-empty verdict ONLY
// when it confidently serves something that is NOT a PGP key (e.g. an HTML
// captive-portal/error page) on a 200. Network errors / non-200 are inconclusive
// -> "" (don't block; the apt-get update probe will surface real fetch errors).
func nwClassifyKeyURL(url string) string {
	client := nwHTTPClient()
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return ""
	}
	resp, err := client.Do(req)
	if err != nil {
		return "" // inconclusive
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return "" // a 404 here is handled by apt-get update classification
	}
	// Read a bounded prefix of the body.
	buf := make([]byte, 4096)
	n, _ := resp.Body.Read(buf)
	body := strings.TrimSpace(string(buf[:n]))
	if body == "" {
		return "" // empty body is ambiguous
	}
	low := strings.ToLower(body)
	if strings.Contains(body, "-----BEGIN PGP PUBLIC KEY BLOCK-----") {
		return "" // a real key -> healthy
	}
	if strings.HasPrefix(low, "<!doctype") || strings.HasPrefix(low, "<html") || strings.Contains(low, "<head") {
		return "returns an HTML page (likely a proxy/captive portal or wrong path), not a PGP public key"
	}
	// 200 with a non-key, non-HTML body: still suspicious but be conservative.
	return ""
}

// nwNoCandidate reports whether `apt-cache policy <pkg>` shows no install
// candidate (Candidate: (none)).
func nwNoCandidate(out string) bool {
	for _, ln := range strings.Split(out, "\n") {
		t := strings.TrimSpace(ln)
		if strings.HasPrefix(t, "Candidate:") {
			val := strings.TrimSpace(strings.TrimPrefix(t, "Candidate:"))
			return val == "(none)" || val == "(none)\n" || val == ""
		}
	}
	// No Candidate line at all usually means the package is unknown -> no candidate.
	return strings.Contains(out, "Unable to locate package")
}

// nwUDPEgressWorks sends a real UDP datagram to well-known external responders
// (a DNS query to public resolvers, an NTP request, a STUN binding) and returns
// true if ANY reply comes back within the timeout. All probes are best-effort.
func nwUDPEgressWorks() bool {
	type probe struct {
		addr    string
		payload []byte
	}
	probes := []probe{
		{"8.8.8.8:53", nwDNSQuery()},
		{"1.1.1.1:53", nwDNSQuery()},
		{"time.google.com:123", nwNTPRequest()},
		{"stun.l.google.com:19302", nwSTUNRequest()},
	}
	for _, p := range probes {
		if nwUDPRoundTrip(p.addr, p.payload) {
			return true
		}
	}
	return false
}

// nwUDPRoundTrip dials addr over UDP, writes payload, and waits for any reply
// within nwNetTimeout. Returns true on a received reply.
func nwUDPRoundTrip(addr string, payload []byte) bool {
	conn, err := net.DialTimeout("udp", addr, nwNetTimeout)
	if err != nil {
		return false
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(nwNetTimeout))
	if _, err := conn.Write(payload); err != nil {
		return false
	}
	buf := make([]byte, 1024)
	n, err := conn.Read(buf)
	return err == nil && n > 0
}

// nwDNSQuery builds a minimal DNS A query for "." (root) good enough to elicit a
// reply from a public resolver.
func nwDNSQuery() []byte {
	// Transaction ID 0x1234, standard query, 1 question for "." A IN.
	return []byte{
		0x12, 0x34, // ID
		0x01, 0x00, // flags: RD
		0x00, 0x01, // QDCOUNT
		0x00, 0x00, // ANCOUNT
		0x00, 0x00, // NSCOUNT
		0x00, 0x00, // ARCOUNT
		0x00,       // root name
		0x00, 0x01, // QTYPE A
		0x00, 0x01, // QCLASS IN
	}
}

// nwNTPRequest builds a 48-byte client-mode NTP (v3) request.
func nwNTPRequest() []byte {
	req := make([]byte, 48)
	req[0] = 0x1b // LI=0, VN=3, Mode=3 (client)
	return req
}

// nwSTUNRequest builds a minimal STUN Binding Request (RFC 5389) with the magic
// cookie and a zeroed transaction id.
func nwSTUNRequest() []byte {
	msg := make([]byte, 20)
	// Message type 0x0001 (Binding Request).
	msg[0], msg[1] = 0x00, 0x01
	// Length 0.
	msg[2], msg[3] = 0x00, 0x00
	// Magic cookie 0x2112A442.
	msg[4], msg[5], msg[6], msg[7] = 0x21, 0x12, 0xA4, 0x42
	// Transaction ID (12 bytes) left zero.
	return msg
}

// nwFirewallDeniesOutboundUDP inspects local firewall tools for an explicit
// default-deny outbound posture that lacks a 51820/udp allow. Returns
// (true, detail) only when confidently default-deny; missing tools -> false.
func nwFirewallDeniesOutboundUDP() (bool, string) {
	if nwHave("ufw") {
		if out, err := nwRun("ufw", "status", "verbose"); err == nil {
			low := strings.ToLower(out)
			if strings.Contains(low, "status: active") && nwUfwDeniesOutgoing(low) {
				if !strings.Contains(low, "51820") {
					return true, "ufw default-deny outgoing, no 51820/udp allow"
				}
			}
		}
	}
	if nwHave("iptables") {
		if out, err := nwRun("iptables", "-S", "OUTPUT"); err == nil {
			if nwPolicyDrop(out, "OUTPUT") && !nwAllowsUDPPort(out, "51820") {
				return true, "iptables OUTPUT policy DROP, no udp dport 51820 accept"
			}
		}
	}
	return false, ""
}

// nwUfwDeniesOutgoing reports whether ufw's verbose status shows a default-deny
// (or default-reject) outgoing policy. Input is already lower-cased.
func nwUfwDeniesOutgoing(low string) bool {
	// Line looks like: "default: deny (incoming), allow (outgoing), ...".
	for _, ln := range strings.Split(low, "\n") {
		if strings.HasPrefix(strings.TrimSpace(ln), "default:") {
			if strings.Contains(ln, "deny (outgoing)") || strings.Contains(ln, "reject (outgoing)") {
				return true
			}
		}
	}
	return false
}

// nwAllowsUDPPort reports whether an iptables -S dump has an explicit ACCEPT for
// the given udp dport.
func nwAllowsUDPPort(dump, port string) bool {
	for _, ln := range strings.Split(dump, "\n") {
		low := strings.ToLower(ln)
		if strings.Contains(low, "-p udp") && strings.Contains(low, "--dport "+port) && strings.Contains(low, "-j accept") {
			return true
		}
		if strings.Contains(low, "-p udp") && strings.Contains(low, ":"+port) && strings.Contains(low, "-j accept") {
			return true
		}
	}
	return false
}

// nwPolicyDrop reports whether an `iptables -S` dump sets the named chain's
// default policy to DROP or REJECT.
func nwPolicyDrop(dump, chain string) bool {
	for _, ln := range strings.Split(dump, "\n") {
		t := strings.TrimSpace(ln)
		if t == "-P "+chain+" DROP" || t == "-P "+chain+" REJECT" {
			return true
		}
	}
	return false
}

// nwHasStaleCNIChains reports whether an `iptables -S` dump still has chains from
// a previous CNI install (Calico/Cilium/kube-proxy) that should be flushed on a
// clean host.
func nwHasStaleCNIChains(dump string) bool {
	for _, ln := range strings.Split(dump, "\n") {
		t := strings.TrimSpace(ln)
		if !strings.HasPrefix(t, "-N ") {
			continue
		}
		name := strings.TrimSpace(strings.TrimPrefix(t, "-N "))
		if strings.HasPrefix(name, "cali-") || strings.HasPrefix(name, "CILIUM_") ||
			strings.HasPrefix(name, "cilium") || strings.HasPrefix(name, "KUBE-") {
			return true
		}
	}
	return false
}

// nwNftOutputPolicyDrop reports whether an nft ruleset has a base output chain in
// the inet filter table with a drop policy.
func nwNftOutputPolicyDrop(ruleset string) bool {
	// Look for a chain ... { type filter hook output ... policy drop;
	sc := bufio.NewScanner(strings.NewReader(ruleset))
	inOutput := false
	for sc.Scan() {
		line := strings.ToLower(strings.TrimSpace(sc.Text()))
		if strings.Contains(line, "hook output") {
			inOutput = true
		}
		if inOutput && strings.Contains(line, "policy drop") {
			return true
		}
		if strings.HasPrefix(line, "}") {
			inOutput = false
		}
	}
	return false
}

// nwPrimaryPrivateIPv4 returns the RFC1918 IPv4 of the primary (non-loopback)
// interface used for the default route, or "" if none / the primary is public.
func nwPrimaryPrivateIPv4() string {
	ifaces, err := net.Interfaces()
	if err != nil {
		return ""
	}
	for _, iface := range ifaces {
		if iface.Flags&net.FlagLoopback != 0 || iface.Flags&net.FlagUp == 0 {
			continue
		}
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			var ip net.IP
			switch v := a.(type) {
			case *net.IPNet:
				ip = v.IP
			case *net.IPAddr:
				ip = v.IP
			}
			if ip == nil {
				continue
			}
			ip4 := ip.To4()
			if ip4 == nil {
				continue
			}
			if ip4.IsPrivate() {
				return ip4.String()
			}
		}
	}
	return ""
}

// nwMultipleDefaultRoutes reports whether the host has more than one IPv4 default
// route (multi-homed). Prefers `ip -j route`; falls back to text parsing. On any
// inability to determine, returns false (must not warn on uncertainty).
func nwMultipleDefaultRoutes() bool {
	if nwHave("ip") {
		// JSON form first.
		if out, err := nwRun("ip", "-j", "route", "show", "default"); err == nil {
			var routes []map[string]any
			if json.Unmarshal([]byte(out), &routes) == nil {
				if len(routes) > 1 {
					return true
				}
				// JSON parsed fine with <=1 default -> trust it.
				if len(routes) <= 1 {
					return false
				}
			}
		}
		// Text fallback.
		if out, err := nwRun("ip", "route", "show", "default"); err == nil {
			count := 0
			for _, ln := range strings.Split(out, "\n") {
				if strings.HasPrefix(strings.TrimSpace(ln), "default ") {
					count++
				}
			}
			return count > 1
		}
	}
	return false
}
