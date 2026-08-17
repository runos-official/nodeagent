package commons

import (
	"errors"
	"os/exec"
	"strconv"
	"strings"
	"sync"
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

// wgRoamedEndpointLiveness is how recently a peer must have completed a handshake for its endpoint
// to count as a session that still works.
//
// 180 s is WireGuard's REJECT_AFTER_TIME: past it the keypair is definitionally unusable, so an
// endpoint that has not handshaked inside it is not carrying traffic whatever else is true. That is
// the honest justification, and it is deliberately NOT "our keepalive keeps it fresh".
//
// OUR KEEPALIVE IS NOT WHAT REFRESHES THIS. `keep_key_fresh` rekeys only on the side that
// INITIATED, and for an endpointless peer the far side is necessarily the initiator, because this
// node has no address to dial. So our handshake age is refreshed by the far side's cadence, not by
// our persistent-keepalive 5. A healthy session's age therefore cycles up to REKEY_AFTER_TIME
// (120 s) and back, which is the margin this constant leaves, and it holds only while the far side
// is a current agent. A peer that has gone quiet for other reasons is covered by the two-pass rule
// in Plan rather than by this number.
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

// endpointLiveness is what one reading of a peer's handshake tells us about its session.
type endpointLiveness int

const (
	// endpointDead: no endpoint, never handshaked, or handshaked longer ago than a keypair lives.
	endpointDead endpointLiveness = iota
	// endpointLive: handshaked recently enough to still be carrying traffic.
	endpointLive
	// endpointUnmeasurable: the reading cannot be believed, so it must not be acted on either way.
	endpointUnmeasurable
)

// liveness reports whether this peer's endpoint belongs to a session that is working right now.
//
// A NEGATIVE age is not "very fresh", it is a clock that moved. `wg` reports the handshake as wall
// clock and `now` is wall clock, so an NTP step, a resumed machine or a box with a dead RTC moves
// one without moving the other. Read as fresh it would keep a dead endpoint for ever; read as stale
// it would wipe every live roamed endpoint on the node in a single pass, which is R8 again for all
// peers at once. Neither is a measurement, so it is neither.
func (s WgPeerState) liveness(now time.Time) endpointLiveness {
	if s.Endpoint == "" {
		return endpointDead
	}
	if s.LastHandshakeUnix == 0 {
		return endpointDead
	}
	age := now.Sub(time.Unix(s.LastHandshakeUnix, 0))
	if age < 0 {
		return endpointUnmeasurable
	}
	if age < wgRoamedEndpointLiveness {
		return endpointLive
	}
	return endpointDead
}

// PeerConvergencePlan is what to do to the interface to reach the desired set.
type PeerConvergencePlan struct {
	// Remove holds public keys to drop, both retired peers and peers being re-added to clear
	// their endpoint.
	Remove []string
	// Set holds the peers to configure afterwards, in the order they were supplied.
	Set []WgPeer
}

// PeerMemory is what this agent remembers between convergence passes.
//
// IT EXISTS BECAUSE THE HANDSHAKE CANNOT ANSWER THE QUESTION ON ITS OWN, and the first version of
// this fix pretended it could. `wg show` reports one endpoint and never says whether the kernel got
// it from us or learned it by roaming. Liveness separates a WORKING address from a dead one, which
// is what R8 needed, but it does not separate OURS from THEIRS. So a peer we had configured with a
// public endpoint, re-declared as having no inbound path while that tunnel is up, reads live for
// ever and its endpoint is never cleared: the declaration converges one way and not back, which is
// the exact failure the header of this file was written about.
//
// What this agent DOES know is what it last applied. A peer whose applied endpoint was non-empty
// and is now empty has had its declaration changed, and that clears unconditionally. A peer that
// was already endpointless can only be holding a roamed address, and that is where liveness rules.
//
// In memory only, for the life of the process. After a restart nothing is remembered, and an
// unremembered peer is treated as already-endpointless, which is the conservative direction: a
// changed declaration then waits for the liveness rule instead of converging at once, rather than a
// live session being torn down on every agent start.
type PeerMemory struct {
	mu sync.Mutex
	// appliedEndpoint is the desired endpoint this agent last SET for each peer key.
	appliedEndpoint map[string]string
	// deadReadings counts consecutive passes in which a peer's endpoint read dead.
	deadReadings map[string]int
}

// NewPeerMemory returns an empty memory. Tests build their own so no case depends on another.
func NewPeerMemory() *PeerMemory {
	return &PeerMemory{appliedEndpoint: map[string]string{}, deadReadings: map[string]int{}}
}

// wgDeadReadingsBeforeClearing is how many consecutive dead readings clear a roamed endpoint.
//
// TWO, not one, and the second one is hysteresis rather than caution. A single reading can be wrong
// for reasons that have nothing to do with the peer: a clock that stepped, a pass that raced a
// rekey, a far side that was briefly quiet. Acting on one costs the 30 to 70 second outage R8 was
// filed about; waiting for a second costs one convergence interval on a declaration that is not
// urgent, because the endpoint being cleared is one nothing is reaching anyway.
const wgDeadReadingsBeforeClearing = 2

// defaultPeerMemory is what the two production call sites share. One agent, one interface, one
// memory.
var defaultPeerMemory = NewPeerMemory()

// PlanPeerConvergence works out how to get from the peers currently on the interface to exactly
// the desired set, as of `now`, using this agent's shared memory of earlier passes.
func PlanPeerConvergence(current map[string]WgPeerState, desired []WgPeer, now time.Time) PeerConvergencePlan {
	return defaultPeerMemory.Plan(current, desired, now)
}

// Plan is PlanPeerConvergence against a specific memory.
//
// current maps each configured peer's public key to what the interface holds for it.
//
// CLEARING AN ENDPOINT MEANS REMOVING THE PEER AND ADDING IT BACK. WireGuard has no command that
// unsets an endpoint: `wg set` can overwrite one, never remove one. So a peer that has an endpoint
// today and should have none tomorrow appears in BOTH lists, deliberately. Without that, a node
// that moved behind NAT would keep being dialled at its old address for ever.
//
// A LIVE ROAMED ENDPOINT IS LEFT ALONE, which is R8. An endpointless peer is a node with no inbound
// path: it dials out, and the kernel learns where it dialled from. That learned address used to be
// indistinguishable from a configured one, so the removal rule fired on it every single pass.
// Measured on dev 2026-08-17 with two home nodes declared no-public-ingress: the third node dropped
// and re-added both peers roughly every three minutes, each time destroying the session and the
// learned address, and each time taking 30 to 70 seconds to re-learn them. For that whole window
// the node could not START a conversation with either peer, which is a control plane losing its
// path to two etcd members on a timer.
//
// A peer that already has the endpoint it should have is still re-set, which costs nothing: `wg
// set` is idempotent, and the alternative is a second diffing rule that can disagree with this one.
func (m *PeerMemory) Plan(current map[string]WgPeerState, desired []WgPeer, now time.Time) PeerConvergencePlan {
	m.mu.Lock()
	defer m.mu.Unlock()

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
			delete(m.deadReadings, p.PubKey)
			continue
		}
		state, configured := current[p.PubKey]
		if !configured || state.Endpoint == "" {
			delete(m.deadReadings, p.PubKey)
			continue
		}

		// The declaration itself changed: this agent applied an endpoint for this peer and is now
		// being told it has none. Nothing about the current session makes that stale, so it clears
		// at once rather than waiting for a tunnel we were told to stop using to fall over.
		if applied, known := m.appliedEndpoint[p.PubKey]; known && applied != "" {
			remove = append(remove, p.PubKey)
			delete(m.deadReadings, p.PubKey)
			continue
		}

		switch state.liveness(now) {
		case endpointLive:
			delete(m.deadReadings, p.PubKey)
		case endpointUnmeasurable:
			// Say nothing and change nothing. The count is left where it is so a run of bad
			// clock readings neither clears the endpoint nor resets progress towards clearing it.
		case endpointDead:
			m.deadReadings[p.PubKey]++
			if m.deadReadings[p.PubKey] >= wgDeadReadingsBeforeClearing {
				remove = append(remove, p.PubKey)
				delete(m.deadReadings, p.PubKey)
			}
		}
	}

	// Record what this pass is about to apply, and forget peers nobody asked for.
	applied := make(map[string]string, len(desired))
	for _, p := range desired {
		applied[p.PubKey] = p.EndpointIP
	}
	m.appliedEndpoint = applied
	for key := range m.deadReadings {
		if _, ok := wanted[key]; !ok {
			delete(m.deadReadings, key)
		}
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
	// THIS OUTPUT IS SECRET. Line 1 of `wg show <iface> dump` is the interface, and its first field
	// is the node's WireGuard PRIVATE KEY. `wg show wg0 endpoints`, which this replaced, carried no
	// such thing. Never log it, and never put it in an error message.
	out, err := exec.Command("wg", "show", "wg0", "dump").CombinedOutput()
	if err != nil {
		return map[string]WgPeerState{}, err
	}
	peers := ParseWgDump(string(out))

	// AN OUTPUT THIS PARSER DOES NOT RECOGNISE MUST NOT READ AS "NO PEERS". An empty map plans no
	// removals, so a format change would silently retire the rule that a removed node's key stops
	// working, and nothing would say so. Counting the lines is enough to tell "this node has no
	// peers" from "this node has peers and none of them parsed".
	if len(peers) == 0 && countNonEmptyLines(out) > 1 {
		return map[string]WgPeerState{}, errors.New(
			"wg show wg0 dump produced output this agent could not parse as peers; no peer was removed",
		)
	}
	return peers, nil
}

func countNonEmptyLines(out []byte) int {
	n := 0
	for _, line := range strings.Split(string(out), "\n") {
		if strings.TrimSpace(line) != "" {
			n++
		}
	}
	return n
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
