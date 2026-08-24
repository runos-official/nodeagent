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

// nwAptUpdateTimeout bounds the real `apt-get update` run. It is deliberately
// much larger than nwExecTimeout: apt retries slow mirrors internally, and this
// command is the authoritative decider for the apt-sources check (a synthetic
// probe blip must not abort an install that apt itself would survive).
const nwAptUpdateTimeout = 75 * time.Second

// nwRun executes name+args under a timeout and returns combined stdout+stderr
// plus the error. Missing binaries / timeouts return a non-nil error; callers
// MUST treat that as "could not determine" and not block.
func nwRun(name string, args ...string) (string, error) {
	return nwRunTimeout(nwExecTimeout, name, args...)
}

// nwRunTimeout is nwRun with a caller-chosen timeout, for the few commands
// (the real `apt-get update`) that legitimately need more than nwExecTimeout.
func nwRunTimeout(d time.Duration, name string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), d)
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
		// Redirects are followed by default (the k8s key URL check needs the
		// final body); the mirror probe overrides CheckRedirect to NOT follow.
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
	// distinct mirror root; any HTTP response (even 403/404, mirror roots
	// commonly reject a bare request) proves reachability. A probe failure
	// alone does NOT block: the probe is a single 6s shot that can lose to
	// early-boot networking still converging (observed on fresh cloud nodes),
	// so it only ATTRIBUTES; the real `apt-get update` below is the decider.
	var unreachableMirrors []string
	for _, host := range nwAptMirrorURLs() {
		if reason, blocked := nwProbeMirrorUnreachable(host); blocked {
			unreachableMirrors = append(unreachableMirrors, fmt.Sprintf("%s (%s)", host, reason))
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

	// (2)+(3) Run the real `apt-get update` and classify its failure class
	// precisely (GPG vs clock vs mirror egress vs 404 vs proxy). This is the
	// decider for mirror reachability (hence its own generous timeout: apt
	// retries slow mirrors internally); the probe above only attributes.
	out, err := nwRunTimeout(nwAptUpdateTimeout, "apt-get", "update", "-qq")
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
		// Mirror egress class: the root probe already saw these hosts fail
		// (timeout/DNS/refused) and the real apt-get update failed too, so
		// attribute to egress rather than letting the output fall into the
		// misleading 'failed to fetch' 404 class below.
		case len(unreachableMirrors) > 0:
			return fmt.Errorf("the apt mirror(s) this node is configured to use are unreachable: %s\n\n'apt-get update' also failed, so this is a real apt egress problem, not a probe blip. It breaks every 'apt-get update'/'apt-get install' during the install. Check egress/DNS to these hosts (and any HTTP(S) proxy), then run 'sudo apt-get update' until clean and re-run preflight.\nThis is pre-existing apt egress on your machine.\n\napt said:\n%s",
				strings.Join(unreachableMirrors, ", "), nwIndentBlock(nwTrimAptNoise(out)))
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

	// The synthetic probe lost but the real apt-get update succeeded: that is
	// a transient blip (typically early-boot networking on a fresh cloud
	// node), not an egress problem. Log it and proceed.
	if len(unreachableMirrors) > 0 {
		roslog.W("apt mirror root probe failed but 'apt-get update' succeeded; treating the probe failure as transient", nil,
			"mirrors", strings.Join(unreachableMirrors, ", "))
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

// These are the seams the two endpoint checks call through, so a test can stand in fakes and prove
// the printed text without a NAT, without a second NIC and without a private network. The value of
// these checks is the text, so the text has to be reachable from a test.
var (
	nwPrimaryPrivateIPv4Fn    = nwPrimaryPrivateIPv4
	nwExternalIPFn            = commons.GetExternalIPAddress
	nwIfaceHoldingIPv4Fn      = nwIfaceHoldingIPv4
	nwPrivateIPv4CandidatesFn = nwPrivateIPv4Candidates
)

// nwEndpoint is what both endpoint checks read: the node's primary RFC1918 address, the address the
// outside world sees, the local interface that holds that outside address (empty when no interface
// on this host holds it), and the private addresses that could plausibly carry a RunOS private
// network. The third field is the one that separates a NAT'd host from a multi-homed one, and it
// was missing until 2026-08-24. See nwEndpointFacts.
type nwEndpoint struct {
	private string
	public  string
	dev     string
	// privCandidates is read ONLY by checkMultiHomedEndpoint. private stays the NAT branch's
	// input, unchanged. See nwPrivateIPv4Candidates for why the two questions differ.
	privCandidates []nwIfaceAddr
}

// ONE ANSWER PER PREFLIGHT RUN. Both endpoint checks read these three variables, so the two cannot
// be handed different answers and cannot contradict each other.
//
// Until 2026-08-24 each check called the gatherer itself. The public-IP probe therefore ran TWICE
// and the two answers were never required to agree. Driven with two different answers, BOTH
// warnings printed one screen apart saying opposite things: nat-collision called the node NAT'd,
// multi-homed-endpoint said it was NOT behind NAT. The shape is not rare. Dual-WAN or failover
// egress, a rotating CGNAT pool, or a transient failure of the first provider sending the second
// call to a different provider all produce it, and commons.GetExternalIPAddress asks the
// DUAL-STACK ipify endpoint first, so an IPv6-preferring host falls through to provider two
// routinely.
//
// It also halves the probe cost, which is the whole cost of these two checks. Measured 2026-08-24
// on a developer machine, both checks end to end, six runs before and five after: two probes 2.30
// to 6.88 s against one probe 0.84 to 2.46 s. With curl and dig replaced by shims that never
// answer, two probes 30.02 s against one probe 15.01 s, because commons.GetExternalIPAddress tries
// three providers on a 5 s budget each.
//
// Preflight runs its checks sequentially in one goroutine, so a plain flag is enough here.
var (
	nwEndpointGathered bool
	nwEndpointCache    nwEndpoint
	nwEndpointCacheOK  bool
)

// nwEndpointFacts returns the facts for this preflight run, gathering them on first use.
func nwEndpointFacts() (nwEndpoint, bool) {
	if !nwEndpointGathered {
		nwEndpointCache, nwEndpointCacheOK = nwGatherEndpointFacts()
		nwEndpointGathered = true
	}
	return nwEndpointCache, nwEndpointCacheOK
}

// nwResetEndpointFacts drops the memo. runChecksSkipping calls it at the start of every preflight
// run, so "once per run" means what it says and a second run inside one process cannot read the
// first run's answer. Tests call it whenever they change the fakes.
func nwResetEndpointFacts() {
	nwEndpointGathered, nwEndpointCache, nwEndpointCacheOK = false, nwEndpoint{}, false
}

// nwGatherEndpointFacts reads the host facts checkNATEndpointCollision and checkMultiHomedEndpoint
// share, and reports false when the answer is inconclusive so that NEITHER check speaks. Three
// cases are inconclusive, and all three must stay silent, because a warning that fires on a healthy
// machine trains operators to ignore every warning:
//
//   - No RFC1918 primary address. The node is directly addressed and there is nothing to say.
//   - No externally observed address. Either there is no egress to the IP echo services or the node
//     is offline; both are already covered by the blocking egress checks.
//   - The externally observed address IS the primary private address. Not NAT, and no separate
//     public address to advise about either.
//
// Call it through nwEndpointFacts, never directly: it is the expensive half.
func nwGatherEndpointFacts() (nwEndpoint, bool) {
	private := nwPrimaryPrivateIPv4Fn()
	if private == "" {
		return nwEndpoint{}, false
	}

	public, err := nwExternalIPFn()
	if err != nil || strings.TrimSpace(public) == "" {
		roslog.W("could not determine external IP; skipping the endpoint checks", err)
		return nwEndpoint{}, false
	}
	public = strings.TrimSpace(public)

	if public == private {
		return nwEndpoint{}, false
	}

	return nwEndpoint{
		private:        private,
		public:         public,
		dev:            nwIfaceHoldingIPv4Fn(public),
		privCandidates: nwPrivateIPv4CandidatesFn(),
	}, true
}

// checkNATEndpointCollision detects that this node sits behind NAT (its primary
// RFC1918 interface address differs from its detected public IP, AND no interface
// on this host holds that public IP) and warns about the WireGuard
// endpoint-collision foot-gun: WireGuard keys peers by their public endpoint, so
// two RunOS nodes behind the SAME public IP collide and only one tunnel stays up,
// and same-NAT peers additionally need NAT hairpin support. It is a WARN: a single
// node behind NAT is perfectly fine and peers cannot be enumerated at preflight.
// Returns nil when the node is directly routable or the public IP cannot be
// determined (inconclusive must not block).
//
// THE HOST MUST NOT HOLD THE PUBLIC ADDRESS ITSELF, and that is a SEPARATE question
// from "is the primary address private". Measured on RunOS dev 2026-08-24: three
// Hetzner Cloud servers, each with its own routable address on eth0 and a private
// address on enp7s0 for a Hetzner private network, were all told they were behind
// NAT and at risk of colliding tunnels. Both claims were false. The host held the
// public address, and the three nodes had three DIFFERENT public addresses, so no
// endpoint could collide. The old equal-IP guard could not catch this, because it
// compared the public address to the PRIVATE one and never to the host's other
// interfaces, which made it dead code on a dual-homed host. checkMultiHomedEndpoint
// now owns that case and this check declines it.
//
// THE REMEDY IS ORDERED, and the message says so (FCR 148, F8). Preflight runs
// BEFORE `runos register` in the installer, so this node has no nid yet and the
// control plane refuses `clusters networks join` with a 409 until it does. The
// network itself is cluster-scoped, so `clusters networks create` IS runnable
// now. Printing both commands without that split sent operators to a command
// that could only fail at the moment they read it.
//
// THE REMEDY NEEDS EVERY NODE BEHIND THE NAT, and the message says that too.
// Endpoint resolution reads a self-join over network_memberships
// (nodeward/persist/network/shared.go:53-61: m1 JOIN m2 ON m2.network_id =
// m1.network_id AND m2.nid <> m1.nid WHERE m1.nid = ?), so a membership held by
// ONE node returns zero rows for both peers, rule 2 in
// nodeward/persist/node/endpoint.go never fires, and the collision stays. An
// earlier draft called the single join "the whole remedy", which was a no-op
// instruction. Confirmed on hardware 2026-08-23: WireGuard peered over the LAN
// address only after BOTH lab nodes held a membership in one network.
//
// THE RECOVERY PATH IS `sudo runos install`, NOT THE INSTALLER SCRIPT. An
// earlier draft told the operator to re-run the installer, which cannot work on
// a node that has already registered: the registration token inside it is
// single-use (nodeward/persist/rtoken/special.go:15 stamps spent_at and
// find_by.go:11 then skips it) and lasts 20 minutes (add.go:22), so register
// exits non-zero and templates/install.sh:231 stops the run. Worse, the obvious
// way out of that dead end undoes the remedy: a fresh join command mints a NEW
// nid (rtoken/add.go:11), so the membership made in step 3 stays on the old nid
// and the collision returns with every call still reporting success. The
// message therefore names the node-agent command instead, which is what the
// agent's own failure hints already tell operators to re-run
// (cmd/install/root.go:80,86,93).
//
// THE PRINTED CLI COMMANDS DO NOT RUN ON THIS NODE. cmd/root.go registers only
// the node-agent subcommands, so `runos clusters ...` on this box answers
// `unknown command "clusters" for "runos"`. The message names where each command
// runs, because an operator standing on the node reads "the runos CLI" as the
// binary that just printed the warning.
func checkNATEndpointCollision() error {
	ep, ok := nwEndpointFacts()
	if !ok {
		return nil
	}
	if ep.dev != "" {
		// This host holds the public address on one of its own interfaces, so it is
		// multi-homed, not NAT'd. checkMultiHomedEndpoint owns that case.
		return nil
	}
	ifaceIP, extIP := ep.private, ep.public

	return fmt.Errorf("this node is behind NAT (private %s vs public %s)\n\n"+
		"WireGuard identifies peers by their public UDP endpoint. If you place more than one RunOS node behind this same NAT/public IP, their tunnels collide and only one stays up, and same-NAT peers also need NAT hairpin support. A single node behind NAT is fine.\n\n"+
		"The RunOS remedy, when the nodes CAN reach each other privately (one LAN, or guests on one VM host): declare that path and RunOS gives each peer the private address instead of the shared public one.\n\n"+
		"WHERE THESE COMMANDS RUN. The printed commands that start with 'sudo runos' are node-agent commands, and they run on THIS node. Every other printed command is a RunOS CLI command. The 'runos' on THIS node does NOT have those: it answers 'unknown command \"clusters\" for \"runos\"'. Run them from a workstation that has the RunOS CLI installed, or from the console.\n\n"+
		"Run the remedy in this order. Each step says when it becomes runnable:\n"+
		"  1. NOW, before or during this install. Create ONE network for this NAT. It needs only the cluster, so it works before this node exists, and creating a network that already exists returns the existing one:\n"+
		"       runos clusters networks create --cid <cid> --name <network-name> --json   # prints the network id\n"+
		"  2. AFTER this node has REGISTERED, read the node's nid. You may already hold it: the nid is reserved when you generate the join command. What register creates is the node ROW, and the control plane has no row for that nid until this node registers. On THIS node, after register:\n"+
		"       sudo runos status                                                         # prints \"Node ID (NID)\"\n"+
		"     From elsewhere, list the cluster and match the row by its hostname column:\n"+
		"       runos nodes list --cid <cid>                                              # prints EVERY node, not just this one\n"+
		"  3. Join EVERY node behind this NAT to the SAME network, each at its own private address. A membership on ONE node alone changes NOTHING: RunOS hands out the private address only when BOTH peers hold a membership in one network, so joining this node and stopping leaves the collision in place. For this node:\n"+
		"       runos clusters networks join --cid <cid> --network-id <networkId> --nid <nid> --address %s\n"+
		"     The control plane REFUSES this join with a 409 until that node has registered, because it looks the nid up inside the cluster first. That refusal is the reason for the order, not a fault.\n"+
		"     Check the member set before you stop, and confirm every node behind this NAT is listed. Use --json: the default table collapses the members column to '[N entries]' and hides the nids:\n"+
		"       runos clusters networks list --cid <cid> --json                           # prints each network with its members' nids and addresses\n"+
		"     Each join takes effect at once: the control plane re-sends the peer list to the whole cluster, so no agent restart and no manual VPN sync is needed.\n"+
		"  4. AFTER registration as well, and it needs the nid from step 2. Add this ONLY if the node has NO inbound path at all, because it also REMOVES the node from the cluster public DNS record:\n"+
		"       runos nodes ingress <nid> --cid <cid> --no-public-ingress\n"+
		"  5. ONLY IF this install has already gone past this check. THE INSTALLER DOES NOT WAIT for you: it runs register, and then the Kubernetes install, straight after this warning. Steps 1 to 3 still work on a node that is already installed. Do them for EVERY node behind this NAT first. Then, if this node already ran the Kubernetes join over the colliding endpoint or failed there, redo the install on THIS node. It runs only after register, because it needs the certificates register wrote:\n"+
		"       sudo runos install                                                        # redoes WireGuard, VPN sync and the Kubernetes join\n"+
		"     Do NOT re-run the installer script on a node that has already registered. Its registration token is single-use and lasts about 20 minutes, so register fails and the run stops before the install.\n"+
		"     Do NOT generate a new join command to get past that. A new join command mints a NEW nid. Your step 3 membership stays on the OLD nid, so the collision comes back while every command still reports success.\n\n"+
		"Proven on a two-node nested cluster 2026-08-18 and again 2026-08-19 (goal 28), and again on two lab nodes 2026-08-23: WireGuard peered over the LAN address only after BOTH nodes had registered and joined ONE network at their LAN addresses. Otherwise give each node a distinct routable IP, or a distinct inbound UDP 51820 port-forward per node.\n"+
		"This is a networking heads-up, not a RunOS limitation.",
		ifaceIP, extIP, ifaceIP)
}

// checkMultiHomedEndpoint is the other half of the split made on 2026-08-24. The node holds its own
// public address on a local interface AND has a private address as well, which is a MULTI-HOMED
// host, not a NAT'd one. Until the split, every such node was told it was behind NAT and that its
// tunnels would collide. Measured on RunOS dev 2026-08-24 on three Hetzner Cloud servers, each with
// a routable address on eth0 and a private address on enp7s0: all three read a security-flavoured
// alarm about a cluster that was working.
//
// THE TEXT IS DELIBERATELY SHORT, and it must stay short. There is nothing broken to repair here.
// The private network is an OPTIMISATION the operator may want, so the message states the fact,
// names the gain, gives the two commands and stops. It must never claim a collision, never say
// "only one stays up", and never send the operator to redo an install: an operator who reads a long
// alarm on a healthy cluster learns to ignore preflight.
//
// THE JOIN STILL WAITS FOR REGISTRATION. Preflight runs BEFORE `runos register` (templates
// install.sh runs preflight, then register), so this node has no row in the control plane yet and
// `clusters networks join` is refused with a 409 until it does. `clusters networks create` is
// cluster-scoped and is runnable at once. The message says so, for the same reason
// checkNATEndpointCollision does (FCR 148, F8).
//
// ONE MEMBERSHIP IS NOT A REMEDY, and the advisory has to say so even though it is short. Endpoint
// resolution reads a self-join over network_memberships, so RunOS hands out the private address
// only when BOTH peers hold a membership in ONE network. An earlier draft printed only THIS node's
// join, which is a silent no-op when followed literally: nothing changes and every command reports
// success. The NAT branch has said this since FCR 148 F8; this branch now says it too, in one
// sentence. The same sentence pair names where <networkId> and <nid> come from, because a
// placeholder with no stated source sends the operator looking.
//
// THE PRINTED CLI COMMANDS DO NOT RUN ON THIS NODE. cmd/root.go registers only the node-agent
// subcommands, so `runos clusters ...` on this box answers `unknown command "clusters" for "runos"`.
// The message names where the commands run, because an operator standing on the node reads "the
// runos CLI" as the binary that just printed the advisory.
func checkMultiHomedEndpoint() error {
	ep, ok := nwEndpointFacts()
	if !ok {
		return nil
	}
	if ep.dev == "" {
		// No interface holds the public address, so this node really is behind NAT.
		// checkNATEndpointCollision owns that case.
		return nil
	}
	if len(ep.privCandidates) == 0 {
		// Every RFC1918 address this host holds sits on a container, VM or CNI bridge, so there
		// is no private network to declare and nothing to advise. Measured on real Linux
		// 2026-08-24: a public NIC plus docker0 and virbr0 used to get this advisory, telling the
		// operator to declare a network for a bridge address.
		return nil
	}

	// ONE candidate is a fact and gets printed as `--address <literal>`. TWO OR MORE is a guess,
	// and a guess printed as a literal is exactly how the 2026-08-24 defect reached the operator:
	// the advisory named a docker bridge address as this node's private-network address. Where the
	// check is not confident, it names the interfaces and lets the operator supply the address.
	held, addrArg, pickNote := "", "<address>", ""
	if len(ep.privCandidates) == 1 {
		only := ep.privCandidates[0]
		held = fmt.Sprintf("a private address %s on %s", only.addr, only.dev)
		addrArg = only.addr
	} else {
		held = fmt.Sprintf("private addresses on %s", nwDevList(ep.privCandidates))
		pickNote = " This node holds a private address on more than one interface, so <address> is its address on the network you declare: read it with 'ip -4 addr show <interface>'."
	}

	return fmt.Errorf("this node holds its own public address %s on %s, and it also has %s. It is NOT behind NAT.\n\n"+
		"RunOS hands peers this node's public address unless a declared network says otherwise, so peers reach this node over the public path. If OTHER nodes of this cluster sit on that same private network, declaring the network makes those peers dial each other at their private addresses instead. This is an optimisation, not a repair: the cluster works either way and nothing here needs reinstalling.\n\n"+
		"EVERY node on that private network must join the SAME network, each at its own private address. RunOS hands out the private address only when BOTH peers hold a membership in one network, so joining this node and stopping changes nothing, and every command still reports success.\n\n"+
		"Run these from a workstation that has the RunOS CLI installed, or from the console. The 'runos' on THIS node is the node agent and has no 'clusters' command. The create command prints <networkId>. Each node prints its own <nid> with 'sudo runos status' after it registers, and this node has not registered yet, so run its join after it does.%s\n"+
		"  runos clusters networks create --cid <cid> --name <network-name> --json\n"+
		"  runos clusters networks join --cid <cid> --network-id <networkId> --nid <nid> --address %s",
		ep.public, ep.dev, held, pickNote, addrArg)
}

// nwDevList renders the interface names of candidates for the advisory, in enumeration order.
func nwDevList(candidates []nwIfaceAddr) string {
	devs := make([]string, 0, len(candidates))
	for _, c := range candidates {
		devs = append(devs, c.dev)
	}
	return strings.Join(devs, ", ")
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
// timeout). Any HTTP response (even 30x/403/404/5xx) proves reachability, so
// we do NOT block on those (mirror roots commonly 403 a bare request).
// Redirects are deliberately NOT followed: mirror roots often 30x to a host
// apt never contacts (security.ubuntu.com 301s to www.ubuntu.com), and
// following would spend the probe's single 6s budget on the wrong host.
func nwProbeMirrorUnreachable(root string) (string, bool) {
	client := nwHTTPClient()
	client.CheckRedirect = func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}
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

// nwPrimaryPrivateIPv4 returns the FIRST RFC1918 IPv4 this host holds on an up,
// non-loopback interface, in net.Interfaces() order, or "" when it holds none.
//
// IT IS NOT THE DEFAULT-ROUTE INTERFACE'S ADDRESS. This comment claimed that
// until 2026-08-24 and was wrong: the function reads no route table at all.
// Measured on real Linux that day, on a host holding a docker bridge address on
// eth0, a routable address on pub0, a libvirt virbr0 and the real
// private-network address on privnic, it answers the docker bridge address.
//
// The default-route interface would also be the WRONG answer for these callers:
// on a multi-homed cloud server the default route leaves by the PUBLIC
// interface, which holds no RFC1918 address, so that reading would return ""
// and silence both endpoint checks.
//
// checkNATEndpointCollision reads this, and its behaviour is deliberately
// unchanged: the NAT branch's text was hardened over several rounds and a node
// behind NAT holds one private address in practice. checkMultiHomedEndpoint
// reads nwPrivateIPv4Candidates instead, which asks the narrower question.
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

// nwIfaceAddr is one up, non-loopback interface and one IPv4 address it holds.
type nwIfaceAddr struct {
	dev  string
	addr string
}

// nwIfaceIPv4s returns every IPv4 address this host holds on an up, non-loopback
// interface, in net.Interfaces() order. It is the single enumeration the endpoint
// probes below share.
//
// nwPrimaryPrivateIPv4 above deliberately does NOT read it. That function feeds the
// NAT branch's text, which was hardened over several rounds, so it stays exactly as
// it was.
func nwIfaceIPv4s() []nwIfaceAddr {
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil
	}
	var held []nwIfaceAddr
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
			if ip4 := ip.To4(); ip4 != nil {
				held = append(held, nwIfaceAddr{dev: iface.Name, addr: ip4.String()})
			}
		}
	}
	return held
}

// nwIfaceHoldingIPv4 returns the name of the local, up, non-loopback interface that
// holds addr, or "" when no interface on this host holds it. It is the question
// that separates a NAT'd node from a multi-homed one: a NAT'd node never holds its
// public address, a dual-homed cloud server does (Hetzner Cloud puts the routable
// address on eth0 and the private-network address on enp7s0).
// Anything it cannot determine returns "", which routes the caller to the NAT
// branch, the conservative choice: that branch was already the behaviour before
// this probe existed.
func nwIfaceHoldingIPv4(addr string) string {
	want := net.ParseIP(strings.TrimSpace(addr))
	if want == nil || want.To4() == nil {
		return ""
	}
	for _, held := range nwIfaceIPv4s() {
		if ip := net.ParseIP(held.addr); ip != nil && ip.Equal(want) {
			return held.dev
		}
	}
	return ""
}

// nwPrivateIPv4Candidates returns every RFC1918 IPv4 this host holds on an interface
// that could plausibly carry a RunOS private network: up, non-loopback, and not a
// container, VM, CNI or RunOS link. checkMultiHomedEndpoint reads this instead of
// nwPrimaryPrivateIPv4, and 2026-08-24 measured why on real Linux.
//
// A host with a docker bridge address on eth0, a routable address on pub0, a libvirt
// virbr0 and the real private-network address on privnic made nwPrimaryPrivateIPv4
// answer the docker bridge address, and the advisory then told the operator to
// declare a network at that bridge. "First RFC1918 in enumeration order" is simply
// not the question the advisory is asking.
//
// An empty answer is meaningful and the caller acts on it: a host whose only RFC1918
// addresses sit on bridges has no private network to declare, so the advisory stays
// silent instead of pointing at docker0.
func nwPrivateIPv4Candidates() []nwIfaceAddr {
	var candidates []nwIfaceAddr
	for _, held := range nwIfaceIPv4s() {
		if nwIsBridgeOrVirtualInterface(held.dev) {
			continue
		}
		if ip := net.ParseIP(held.addr); ip != nil && ip.IsPrivate() {
			candidates = append(candidates, held)
		}
	}
	return candidates
}

// nwIsBridgeOrVirtualInterface reports whether dev is a link a container runtime, a
// hypervisor, a CNI or RunOS itself creates, rather than a NIC on a network an
// operator can declare. The test is the interface NAME, which is what the field
// gives us: the tools that create these links fix their names (docker0 and br-<hex>
// from Docker, virbr* from libvirt, vboxnet* and vmnet* from the desktop
// hypervisors), and RunOS's own links are already enumerated by
// idIsRunosManagedInterface.
//
// "br-" is listed and plain "br" is NOT. A bridge named br0 is commonly the
// operator's own bridged NIC on a KVM host, which is exactly the private path this
// advisory is about.
//
// Over-excluding here costs silence on an advisory. Under-excluding costs a printed
// address that is wrong, which is the defect this fixes, so the list leans towards
// silence.
func nwIsBridgeOrVirtualInterface(dev string) bool {
	if idIsRunosManagedInterface(dev) { // wg*, cilium_*, lxc*, cni0, kube-ipvs0
		return true
	}
	for _, prefix := range []string{"docker", "br-", "virbr", "veth", "vboxnet", "vmnet", "flannel", "cali", "kube-", "dummy"} {
		if strings.HasPrefix(dev, prefix) {
			return true
		}
	}
	return false
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
