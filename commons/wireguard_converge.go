package commons

import (
	"os/exec"
	"strconv"
	"strings"
	"time"
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

// wgRoamedEndpointLiveness is how recently a peer must have completed a handshake for its
// endpoint to count as a LIVE roamed address rather than a stale configured one.
//
// Every peer this agent sets carries persistent-keepalive 5 (see wgKeepalive), so a working
// session re-handshakes well inside WireGuard's 120 s rekey interval and its handshake age never
// approaches this number. A session nobody answers ages past it within one convergence pass or
// two, which is when clearing the endpoint is the right move.
const wgRoamedEndpointLiveness = 180 * time.Second

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

// WgPeerState is what the interface currently holds for one peer.
//
// THE HANDSHAKE TIME IS HERE FOR A REASON. WireGuard reports one endpoint field and does not say
// whether the kernel got it from us or learned it from an inbound packet, so the endpoint alone
// cannot tell a stale address we configured from a live address a NAT'd peer is reaching us on.
// The handshake age separates them: only a live session has a recent one.
type WgPeerState struct {
	Endpoint string
	// LastHandshakeUnix is 0 when the peer has never completed a handshake.
	LastHandshakeUnix int64
}

// isLiveRoamedEndpoint reports whether this peer's endpoint belongs to a session that is working
// right now, which is what an endpointless peer's roamed address looks like.
func (s WgPeerState) isLiveRoamedEndpoint(now time.Time) bool {
	if s.Endpoint == "" || s.LastHandshakeUnix == 0 {
		return false
	}
	return now.Sub(time.Unix(s.LastHandshakeUnix, 0)) <= wgRoamedEndpointLiveness
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
// the desired set, as of `now`.
//
// current maps each configured peer's public key to what the interface holds for it.
//
// CLEARING AN ENDPOINT MEANS REMOVING THE PEER AND ADDING IT BACK. WireGuard has no command that
// unsets an endpoint: `wg set` can overwrite one, never remove one. So a peer that has an endpoint
// today and should have none tomorrow appears in BOTH lists, deliberately. Without that, a node
// that moved behind NAT would keep being dialled at its old address forever, which is exactly the
// stuck-declaration case above.
//
// A LIVE ROAMED ENDPOINT IS LEFT ALONE, and that exception is the whole point of the handshake
// field. An endpointless peer is a node with no inbound path: it dials out, and the kernel learns
// where it dialled from. That learned address is indistinguishable from a configured one in `wg
// show`, so the removal rule used to fire on it every single convergence pass. Measured on dev
// 2026-08-17 with two home nodes declared no-public-ingress: the third node dropped and re-added
// both peers roughly every three minutes, each time destroying the session and the learned
// address, and each time taking 30 to 70 seconds to re-learn them from the far side's keepalive.
// For that whole window the node could not START a conversation with either peer, which is a
// control plane losing its path to two etcd members on a timer.
//
// A peer that already has the endpoint it should have is still re-set, which costs nothing: `wg
// set` is idempotent, and the alternative is a second diffing rule that can disagree with this one.
func PlanPeerConvergence(current map[string]WgPeerState, desired []WgPeer, now time.Time) PeerConvergencePlan {
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
		state, configured := current[p.PubKey]
		if !configured || state.Endpoint == "" {
			continue
		}
		if state.isLiveRoamedEndpoint(now) {
			continue
		}
		remove = append(remove, p.PubKey)
	}

	return PeerConvergencePlan{Remove: remove, Set: desired}
}

// ParseWgDump reads `wg show wg0 dump` output into key -> current state.
//
// The first line describes the INTERFACE (private key, public key, listen port, fwmark) and has
// four fields; every later line is a peer with eight: public key, preshared key, endpoint,
// allowed ips, latest handshake, rx, tx, persistent keepalive. A peer with no endpoint prints
// "(none)" and one that has never handshaken prints "0".
//
// The port is dropped: everything here speaks in addresses, and the port is fixed.
func ParseWgDump(out string) map[string]WgPeerState {
	peers := make(map[string]WgPeerState)
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Split(strings.TrimRight(line, "\r"), "\t")
		if len(fields) < 8 {
			// The interface line, or a blank one. Neither is a peer.
			continue
		}
		key := strings.TrimSpace(fields[0])
		if key == "" {
			continue
		}
		endpoint := strings.TrimSpace(fields[2])
		if endpoint == "(none)" {
			endpoint = ""
		} else if i := strings.LastIndex(endpoint, ":"); i > 0 {
			// An endpoint is "host:port". Split from the RIGHT so an IPv6 literal, which is
			// full of colons, does not lose most of itself.
			endpoint = strings.Trim(endpoint[:i], "[]")
		}
		handshake, err := strconv.ParseInt(strings.TrimSpace(fields[4]), 10, 64)
		if err != nil {
			// An unreadable handshake reads as "never", which is the conservative direction:
			// it can only make the plan clear a stale endpoint, never keep one.
			handshake = 0
		}
		peers[key] = WgPeerState{Endpoint: endpoint, LastHandshakeUnix: handshake}
	}
	return peers
}

// CurrentWgPeerStates reads the peers configured on wg0 right now.
//
// A read failure returns an EMPTY map and the error. An empty map means the plan removes nothing,
// which is the safe direction: mistaking a failed read for "no peers are configured" would tear
// down every working tunnel on the node.
func CurrentWgPeerStates() (map[string]WgPeerState, error) {
	out, err := exec.Command("wg", "show", "wg0", "dump").CombinedOutput()
	if err != nil {
		return map[string]WgPeerState{}, err
	}
	return ParseWgDump(string(out)), nil
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
