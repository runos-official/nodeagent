package commons

import (
	"os/exec"
	"strings"
)

// Converging the WireGuard peer set to EXACTLY what was sent (goal 27, vpn-peer-protocol item 3).
//
// The peer update used to be purely additive: it looped over the peers it was given and set each
// one, and never removed anything. Two things followed, both permanent and both silent.
//
//   - A retired node stayed a peer forever. Its key kept working, so a machine that had been
//     removed from the cluster could still reach it.
//   - A peer that had once been given the WRONG endpoint kept it forever. Nothing overwrote it
//     with "no endpoint", because there was no such thing to send, so a node that moved behind
//     NAT was stuck being dialled at an address that no longer answered.
//
// Desired state has to mean desired state, or a declaration that can be toggled one way but not
// back is not a declaration at all.

// WgPeer is one peer's desired configuration. An empty EndpointIP means identity only.
type WgPeer struct {
	PubKey     string
	AllowedIP  string
	EndpointIP string
	// ExtraAllowedIPs are the pool addresses of the VMs the peer node hosts (goal 27,
	// vm-group-range-routing): each becomes another /32 in the peer's allowed-ips and a kernel
	// route. `wg set allowed-ips` REPLACES the list, so a dropped extra converges away on the
	// next set with no separate removal step.
	ExtraAllowedIPs []string
}

// PeerConvergencePlan is what to do to the interface to reach the desired set.
type PeerConvergencePlan struct {
	// Remove holds public keys to drop, both retired peers and peers being re-added to clear
	// their endpoint.
	Remove []string
	// Set holds the peers to configure afterwards, in the order they were supplied.
	Set []WgPeer
}

// PlanPeerConvergence works out how to get from the peers currently on the interface to exactly
// the desired set.
//
// current maps each configured peer's public key to its endpoint, empty when it has none.
//
// CLEARING AN ENDPOINT MEANS REMOVING THE PEER AND ADDING IT BACK. WireGuard has no command that
// unsets an endpoint: `wg set` can overwrite one, never remove one. So a peer that has an endpoint
// today and should have none tomorrow appears in BOTH lists, deliberately. Without that, a node
// that moved behind NAT would keep being dialled at its old address forever, which is exactly the
// stuck-declaration case above.
//
// A peer that already has the endpoint it should have is still re-set, which costs nothing: `wg
// set` is idempotent, and the alternative is a second diffing rule that can disagree with this one.
func PlanPeerConvergence(current map[string]string, desired []WgPeer) PeerConvergencePlan {
	wanted := make(map[string]WgPeer, len(desired))
	for _, p := range desired {
		wanted[p.PubKey] = p
	}

	var remove []string

	// Peers on the interface that nobody asked for. This is the half that never happened before,
	// so a retired node kept a working key.
	for key := range current {
		if _, ok := wanted[key]; !ok {
			remove = append(remove, key)
		}
	}

	// Peers that must lose an endpoint they currently hold.
	for _, p := range desired {
		if p.EndpointIP != "" {
			continue
		}
		if endpoint, configured := current[p.PubKey]; configured && endpoint != "" {
			remove = append(remove, p.PubKey)
		}
	}

	return PeerConvergencePlan{Remove: remove, Set: desired}
}

// ParseWgEndpoints reads `wg show wg0 endpoints` output into key -> endpoint IP.
//
// The output is one tab-separated "publickey<TAB>endpoint" line per peer, where a peer with no
// endpoint prints "(none)". The port is dropped: everything here speaks in addresses, and the
// port is fixed.
func ParseWgEndpoints(out string) map[string]string {
	peers := make(map[string]string)
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(strings.TrimSpace(line))
		if len(fields) == 0 {
			continue
		}
		key := fields[0]
		if len(fields) == 1 {
			peers[key] = ""
			continue
		}
		endpoint := fields[1]
		if endpoint == "(none)" {
			peers[key] = ""
			continue
		}
		// An endpoint is "host:port". Split from the RIGHT so an IPv6 literal, which is full of
		// colons, does not lose most of itself.
		if i := strings.LastIndex(endpoint, ":"); i > 0 {
			endpoint = endpoint[:i]
		}
		peers[key] = strings.Trim(endpoint, "[]")
	}
	return peers
}

// CurrentWgPeers reads the peers configured on wg0 right now.
//
// A read failure returns an EMPTY map and the error. An empty map means the plan removes nothing,
// which is the safe direction: mistaking a failed read for "no peers are configured" would tear
// down every working tunnel on the node.
func CurrentWgPeers() (map[string]string, error) {
	out, err := exec.Command("wg", "show", "wg0", "endpoints").CombinedOutput()
	if err != nil {
		return map[string]string{}, err
	}
	return ParseWgEndpoints(string(out)), nil
}

// RemoveWgPeer drops a peer from wg0.
func RemoveWgPeer(pubKey string) error {
	if err := validateWgPubKey(pubKey); err != nil {
		return err
	}
	cmd := exec.Command("wg", "set", "wg0", "peer", pubKey, "remove")
	if out, err := cmd.CombinedOutput(); err != nil {
		return &wgCommandError{op: "remove peer", output: string(out), err: err}
	}
	return nil
}

type wgCommandError struct {
	op     string
	output string
	err    error
}

func (e *wgCommandError) Error() string {
	return "wg " + e.op + " failed: " + e.err.Error() + " (" + e.output + ")"
}

func (e *wgCommandError) Unwrap() error { return e.err }
