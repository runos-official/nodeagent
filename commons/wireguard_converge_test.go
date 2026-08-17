package commons

import (
	"reflect"
	"sort"
	"testing"
	"time"
)

// Goal 27, vpn-peer-protocol item 3. The peer update was purely additive, so desired state did
// not mean desired state: a retired node kept a working key forever, and a peer given the wrong
// endpoint kept it forever because there was no way to send "no endpoint". A declaration you can
// toggle one way but not back is not a declaration.

const peerA = "AAAAEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopq="
const peerB = "BBBBEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopq="
const peerC = "CCCCEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopq="

// testNow is a fixed clock. Every case states handshake ages relative to it, so no test depends
// on when it runs.
var testNow = time.Unix(1_800_000_000, 0)

// stale is a handshake old enough that no live session could have it.
func stale() int64 { return testNow.Add(-10 * time.Minute).Unix() }

// fresh is a handshake a keepalive-driven session would show.
func fresh() int64 { return testNow.Add(-8 * time.Second).Unix() }

func sorted(s []string) []string {
	out := append([]string(nil), s...)
	sort.Strings(out)
	return out
}

func TestPlanRemovesAPeerNobodyAskedFor(t *testing.T) {
	// The defect: a node removed from the cluster stayed a peer, so its key kept working.
	current := map[string]WgPeerState{
		peerA: {Endpoint: "203.0.113.1", LastHandshakeUnix: fresh()},
		peerB: {Endpoint: "203.0.113.2", LastHandshakeUnix: fresh()},
	}
	desired := []WgPeer{{PubKey: peerA, AllowedIP: "10.0.0.1", EndpointIP: "203.0.113.1"}}

	plan := PlanPeerConvergence(current, desired, testNow)

	if !reflect.DeepEqual(plan.Remove, []string{peerB}) {
		t.Fatalf("Remove = %v, want the retired peer %s", plan.Remove, peerB)
	}
}

func TestPlanClearsAStaleEndpointByReAddingThePeer(t *testing.T) {
	// WireGuard has no command that unsets an endpoint, so the only way to clear one is to remove
	// the peer and add it back. Without this a node that moved behind NAT would keep being dialled
	// at an address that no longer answers. "No longer answers" is the operative part: the session
	// on that address is dead, which is what makes clearing it right.
	current := map[string]WgPeerState{peerA: {Endpoint: "203.0.113.1", LastHandshakeUnix: stale()}}
	desired := []WgPeer{{PubKey: peerA, AllowedIP: "10.0.0.1", EndpointIP: ""}}

	plan := PlanPeerConvergence(current, desired, testNow)

	if !reflect.DeepEqual(plan.Remove, []string{peerA}) {
		t.Fatalf("Remove = %v, want the peer to be dropped so its endpoint goes with it", plan.Remove)
	}
	if len(plan.Set) != 1 || plan.Set[0].PubKey != peerA || plan.Set[0].EndpointIP != "" {
		t.Fatalf("Set = %v, want the peer re-added with no endpoint", plan.Set)
	}
}

func TestPlanNeverEverHandshakenEndpointIsCleared(t *testing.T) {
	// An endpoint that has never completed a handshake is exactly the stuck-declaration case: it
	// was configured, it does not work, and nothing else will remove it.
	current := map[string]WgPeerState{peerA: {Endpoint: "203.0.113.1", LastHandshakeUnix: 0}}
	desired := []WgPeer{{PubKey: peerA, AllowedIP: "10.0.0.1", EndpointIP: ""}}

	if plan := PlanPeerConvergence(current, desired, testNow); len(plan.Remove) != 1 {
		t.Fatalf("Remove = %v, want the never-working endpoint cleared", plan.Remove)
	}
}

// THE DEFECT THIS TEST EXISTS FOR, measured on dev 2026-08-17. Two home nodes declared
// no-public-ingress dial out to a third; the third's kernel learns their addresses by roaming.
// The removal rule could not tell that learned address from a stale configured one, so every
// convergence pass dropped and re-added both peers, destroying a working session and the address
// with it, roughly every three minutes, for 30 to 70 seconds each time.
func TestPlanLeavesALiveRoamedEndpointAlone(t *testing.T) {
	current := map[string]WgPeerState{peerA: {Endpoint: "169.1.210.215", LastHandshakeUnix: fresh()}}
	desired := []WgPeer{{PubKey: peerA, AllowedIP: "10.0.0.1", EndpointIP: ""}}

	plan := PlanPeerConvergence(current, desired, testNow)

	if len(plan.Remove) != 0 {
		t.Fatalf("Remove = %v, want a live roamed endpoint left alone", plan.Remove)
	}
	if len(plan.Set) != 1 {
		t.Fatalf("Set = %v, want the peer still applied", plan.Set)
	}
}

func TestPlanClearsARoamedEndpointOnceItsSessionDies(t *testing.T) {
	// The other half of the rule: leaving a live address alone must not become leaving every
	// address alone, or the declaration stops converging again.
	current := map[string]WgPeerState{
		peerA: {Endpoint: "169.1.210.215", LastHandshakeUnix: testNow.Add(-wgRoamedEndpointLiveness - time.Second).Unix()},
	}
	desired := []WgPeer{{PubKey: peerA, AllowedIP: "10.0.0.1", EndpointIP: ""}}

	if plan := PlanPeerConvergence(current, desired, testNow); len(plan.Remove) != 1 {
		t.Fatalf("Remove = %v, want the dead session's endpoint cleared", plan.Remove)
	}
}

func TestPlanDoesNotChurnAPeerThatIsAlreadyEndpointless(t *testing.T) {
	// It is already right. Removing and re-adding it would drop a live session for nothing.
	current := map[string]WgPeerState{peerA: {Endpoint: "", LastHandshakeUnix: fresh()}}
	desired := []WgPeer{{PubKey: peerA, AllowedIP: "10.0.0.1", EndpointIP: ""}}

	if plan := PlanPeerConvergence(current, desired, testNow); len(plan.Remove) != 0 {
		t.Fatalf("Remove = %v, want nothing removed", plan.Remove)
	}
}

func TestPlanAddsANewPeerWithoutRemovingAnything(t *testing.T) {
	current := map[string]WgPeerState{peerA: {Endpoint: "203.0.113.1", LastHandshakeUnix: fresh()}}
	desired := []WgPeer{
		{PubKey: peerA, AllowedIP: "10.0.0.1", EndpointIP: "203.0.113.1"},
		{PubKey: peerB, AllowedIP: "10.0.0.2", EndpointIP: ""},
	}

	plan := PlanPeerConvergence(current, desired, testNow)

	if len(plan.Remove) != 0 {
		t.Fatalf("Remove = %v, want nothing removed", plan.Remove)
	}
	if len(plan.Set) != 2 {
		t.Fatalf("Set = %v, want both peers configured", plan.Set)
	}
}

// Toggling the declaration BOTH ways has to converge, which is the requirement the section states.
func TestPlanConvergesToggledBothDirections(t *testing.T) {
	// endpoint -> none. The endpoint being cleared is the one we configured, and the far side has
	// gone quiet on it, which is what a node moving behind NAT looks like.
	off := PlanPeerConvergence(
		map[string]WgPeerState{peerA: {Endpoint: "203.0.113.1", LastHandshakeUnix: stale()}},
		[]WgPeer{{PubKey: peerA, AllowedIP: "10.0.0.1"}},
		testNow,
	)
	if len(off.Remove) != 1 {
		t.Fatalf("clearing an endpoint must remove first, got %v", off.Remove)
	}

	// none -> endpoint. `wg set` overwrites an endpoint, so no removal is needed this way round.
	on := PlanPeerConvergence(
		map[string]WgPeerState{peerA: {Endpoint: ""}},
		[]WgPeer{{PubKey: peerA, AllowedIP: "10.0.0.1", EndpointIP: "203.0.113.9"}},
		testNow,
	)
	if len(on.Remove) != 0 {
		t.Fatalf("setting an endpoint needs no removal, got %v", on.Remove)
	}
	if on.Set[0].EndpointIP != "203.0.113.9" {
		t.Fatalf("Set = %v, want the new endpoint", on.Set)
	}
}

// A failed read of the current peers yields an empty map, and an empty map must remove NOTHING.
// Mistaking "I could not read" for "there are no peers" would tear down every tunnel on the node.
func TestPlanRemovesNothingWhenNothingIsKnown(t *testing.T) {
	desired := []WgPeer{{PubKey: peerA, AllowedIP: "10.0.0.1", EndpointIP: "203.0.113.1"}}

	plan := PlanPeerConvergence(map[string]WgPeerState{}, desired, testNow)

	if len(plan.Remove) != 0 {
		t.Fatalf("Remove = %v, want nothing removed on an unknown current state", plan.Remove)
	}
	if len(plan.Set) != 1 {
		t.Fatalf("Set = %v, want the desired peer still applied", plan.Set)
	}
}

func TestPlanEmptyDesiredRemovesEveryPeer(t *testing.T) {
	// The last node leaving the cluster is a real state, not a bad input. A live session does not
	// save a peer nobody asked for: that rule is about identity, not about endpoints.
	current := map[string]WgPeerState{
		peerA: {Endpoint: "203.0.113.1", LastHandshakeUnix: fresh()},
		peerB: {Endpoint: "", LastHandshakeUnix: fresh()},
	}

	plan := PlanPeerConvergence(current, nil, testNow)

	if !reflect.DeepEqual(sorted(plan.Remove), sorted([]string{peerA, peerB})) {
		t.Fatalf("Remove = %v, want every peer", plan.Remove)
	}
	if len(plan.Set) != 0 {
		t.Fatalf("Set = %v, want nothing configured", plan.Set)
	}
}

func TestParseWgDump(t *testing.T) {
	// Line 1 is the interface (4 fields). The rest are peers (8 fields).
	out := "PRIVKEY\tPUBKEY\t51820\toff\n" +
		peerA + "\t(none)\t203.0.113.1:51820\t10.0.0.1/32\t1799999992\t4050584\t10784788\t5\n" +
		peerB + "\t(none)\t(none)\t10.0.0.2/32\t0\t0\t0\t5\n" +
		peerC + "\t(none)\t[2001:db8::1]:51820\t10.0.0.3/32\t1799999000\t180\t2492\t5\n"

	got := ParseWgDump(out)

	want := map[string]WgPeerState{
		peerA: {Endpoint: "203.0.113.1", LastHandshakeUnix: 1799999992},
		peerB: {Endpoint: "", LastHandshakeUnix: 0},
		// Split from the right, or an IPv6 literal loses most of itself to its own colons.
		peerC: {Endpoint: "2001:db8::1", LastHandshakeUnix: 1799999000},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ParseWgDump = %v, want %v", got, want)
	}
}

func TestParseWgDumpHandlesNoPeers(t *testing.T) {
	// A node whose peer set is empty prints the interface line alone. That is zero peers, not a
	// parse failure.
	for _, out := range []string{"", "\n", "   \n\n", "PRIVKEY\tPUBKEY\t51820\toff\n"} {
		if got := ParseWgDump(out); len(got) != 0 {
			t.Fatalf("ParseWgDump(%q) = %v, want empty", out, got)
		}
	}
}

func TestParseWgDumpTreatsAnUnreadableHandshakeAsNever(t *testing.T) {
	// Conservative direction: it can only make the plan clear a stale endpoint, never keep one.
	out := peerA + "\t(none)\t203.0.113.1:51820\t10.0.0.1/32\tnonsense\t0\t0\t5\n"

	if got := ParseWgDump(out)[peerA].LastHandshakeUnix; got != 0 {
		t.Fatalf("LastHandshakeUnix = %d, want 0", got)
	}
}
