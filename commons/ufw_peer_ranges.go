package commons

import (
	"bufio"
	"bytes"
	"fmt"
	"net"
	"os/exec"
	"sort"
	"strings"

	"github.com/runos-official/nodeagent/roslog"
)

// ufw allow rules for PEERED-CLUSTER ranges (goal 27, peering-loose-ends item 1).
//
// The install-time ufw rules (nodeward/uc/prep/05_networking.go) allow this node's OWN cluster
// range plus the legacy 172.24.0.0/16. A peered cluster's range is globally unique and no longer
// inside 172.24/16, so a node with ufw active drops every packet from the far cluster while the
// tunnel is up. The agent already converges the peer set and the peer routes on every
// SET_VPN_PEERS; it converges these ufw rules the same way, adding and removing with the peering.
//
// The far RANGE is the /24 that contains each out-of-prefix peer: a peering meshes whole clusters,
// so the whole /24 is allowed, not one /32 per node. Every rule carries a marker comment, so the
// converge reads back exactly its own rules and never touches an operator's hand-written allow.

// ufwPeerRangeComment marks every rule this converge owns. It appears verbatim in `ufw status`.
const ufwPeerRangeComment = "RunOS peered cluster range"

// desiredPeerRanges returns the sorted set of /24 networks that contain an out-of-prefix peer.
// A peer inside one of this node's own wg0 prefixes is on the local cluster and needs no rule.
// Pure, so it is testable without ufw.
func desiredPeerRanges(ownPrefixes []*net.IPNet, desired []WgPeer) []string {
	set := make(map[string]bool)
	for _, p := range desired {
		ip := net.ParseIP(p.AllowedIP)
		if ip == nil || ip.To4() == nil {
			continue
		}
		if insideAny(ip, ownPrefixes) {
			continue
		}
		masked := ip.Mask(net.CIDRMask(24, 32))
		set[fmt.Sprintf("%s/24", masked.String())] = true
	}
	ranges := make([]string, 0, len(set))
	for r := range set {
		ranges = append(ranges, r)
	}
	sort.Strings(ranges)
	return ranges
}

// parseUfwPeerRanges reads `ufw status` output and returns the source CIDRs of the rules this
// converge owns, identified by the marker comment. Rules without the comment are ignored, so an
// operator's own allow rules are never seen and never removed. Returns the active flag too: a
// converge is a no-op when ufw is inactive.
func parseUfwPeerRanges(out []byte) (active bool, ranges []string) {
	scanner := bufio.NewScanner(bytes.NewReader(out))
	seen := make(map[string]bool)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if strings.HasPrefix(line, "Status:") {
			active = strings.Contains(line, "active") && !strings.Contains(line, "inactive")
			continue
		}
		if !strings.Contains(line, "# "+ufwPeerRangeComment) {
			continue
		}
		// A source-CIDR allow rule reads: `Anywhere  ALLOW  <cidr>  # <comment>`. The source is the
		// field before the comment marker that parses as a CIDR.
		head := strings.SplitN(line, "#", 2)[0]
		for _, field := range strings.Fields(head) {
			if _, _, err := net.ParseCIDR(field); err == nil {
				if !seen[field] {
					seen[field] = true
					ranges = append(ranges, field)
				}
			}
		}
	}
	sort.Strings(ranges)
	return active, ranges
}

// planUfwPeerRanges works out which allow rules to add and delete to reach `want` from `have`.
// Pure and sorted, so a pass is deterministic and testable.
func planUfwPeerRanges(have, want []string) (add, remove []string) {
	haveSet := make(map[string]bool, len(have))
	for _, r := range have {
		haveSet[r] = true
	}
	wantSet := make(map[string]bool, len(want))
	for _, r := range want {
		wantSet[r] = true
	}
	for _, r := range want {
		if !haveSet[r] {
			add = append(add, r)
		}
	}
	for _, r := range have {
		if !wantSet[r] {
			remove = append(remove, r)
		}
	}
	sort.Strings(add)
	sort.Strings(remove)
	return add, remove
}

// ApplyPeerUfwRules converges the ufw allow rules for peered-cluster ranges to exactly the ranges
// the desired peer set implies. A no-op when ufw is not installed or not active: the tunnel still
// carries the traffic on a node with no firewall. Every mutation is logged.
func ApplyPeerUfwRules(desired []WgPeer) {
	if _, err := exec.LookPath("ufw"); err != nil {
		return // no ufw on this node; nothing to allow through
	}
	statusOut, err := exec.Command("ufw", "status").CombinedOutput()
	if err != nil {
		roslog.E("Could not read ufw status; leaving peer firewall rules unchanged", err, "output", string(statusOut))
		return
	}
	active, have := parseUfwPeerRanges(statusOut)
	if !active {
		return // ufw inactive: the install-time rules do not apply either, so there is nothing to converge
	}

	prefixes, err := Wg0Prefixes()
	if err != nil {
		roslog.I("wg0 is not up yet; peer ufw rules converge with the next peer set", "reason", err.Error())
		return
	}
	want := desiredPeerRanges(prefixes, desired)
	add, remove := planUfwPeerRanges(have, want)

	for _, r := range remove {
		if out, err := exec.Command("ufw", "delete", "allow", "from", r).CombinedOutput(); err != nil {
			roslog.E("Could not remove a peer ufw rule", err, "range", r, "output", string(out))
			continue
		}
		roslog.I("Removed peer ufw rule", "range", r)
	}
	for _, r := range add {
		if out, err := exec.Command("ufw", "allow", "from", r, "comment", ufwPeerRangeComment).CombinedOutput(); err != nil {
			roslog.E("Could not add a peer ufw rule", err, "range", r, "output", string(out))
			continue
		}
		roslog.I("Added peer ufw rule", "range", r)
	}
}
