package commons

import (
	"reflect"
	"sort"
	"testing"
)

// Goal 27, vpn-peer-protocol item 3. The peer update was purely additive, so desired state did
// not mean desired state: a retired node kept a working key forever, and a peer given the wrong
// endpoint kept it forever because there was no way to send "no endpoint". A declaration you can
// toggle one way but not back is not a declaration.

const peerA = "AAAAEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopq="
const peerB = "BBBBEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopq="
const peerC = "CCCCEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopq="

func sorted(s []string) []string {
	out := append([]string(nil), s...)
	sort.Strings(out)
	return out
}

func TestPlanRemovesAPeerNobodyAskedFor(t *testing.T) {
	// The defect: a node removed from the cluster stayed a peer, so its key kept working.
	current := map[string]string{peerA: "203.0.113.1", peerB: "203.0.113.2"}
	desired := []WgPeer{{PubKey: peerA, AllowedIP: "10.0.0.1", EndpointIP: "203.0.113.1"}}

	plan := PlanPeerConvergence(current, desired)

	if !reflect.DeepEqual(plan.Remove, []string{peerB}) {
		t.Fatalf("Remove = %v, want the retired peer %s", plan.Remove, peerB)
	}
}

func TestPlanClearsAnEndpointByReAddingThePeer(t *testing.T) {
	// WireGuard has no command that unsets an endpoint, so the only way to clear one is to remove
	// the peer and add it back. Without this a node that moved behind NAT would keep being dialled
	// at an address that no longer answers.
	current := map[string]string{peerA: "203.0.113.1"}
	desired := []WgPeer{{PubKey: peerA, AllowedIP: "10.0.0.1", EndpointIP: ""}}

	plan := PlanPeerConvergence(current, desired)

	if !reflect.DeepEqual(plan.Remove, []string{peerA}) {
		t.Fatalf("Remove = %v, want the peer to be dropped so its endpoint goes with it", plan.Remove)
	}
	if len(plan.Set) != 1 || plan.Set[0].PubKey != peerA || plan.Set[0].EndpointIP != "" {
		t.Fatalf("Set = %v, want the peer re-added with no endpoint", plan.Set)
	}
}

func TestPlanDoesNotChurnAPeerThatIsAlreadyEndpointless(t *testing.T) {
	// It is already right. Removing and re-adding it would drop a live session for nothing.
	current := map[string]string{peerA: ""}
	desired := []WgPeer{{PubKey: peerA, AllowedIP: "10.0.0.1", EndpointIP: ""}}

	if plan := PlanPeerConvergence(current, desired); len(plan.Remove) != 0 {
		t.Fatalf("Remove = %v, want nothing removed", plan.Remove)
	}
}

func TestPlanAddsANewPeerWithoutRemovingAnything(t *testing.T) {
	current := map[string]string{peerA: "203.0.113.1"}
	desired := []WgPeer{
		{PubKey: peerA, AllowedIP: "10.0.0.1", EndpointIP: "203.0.113.1"},
		{PubKey: peerB, AllowedIP: "10.0.0.2", EndpointIP: ""},
	}

	plan := PlanPeerConvergence(current, desired)

	if len(plan.Remove) != 0 {
		t.Fatalf("Remove = %v, want nothing removed", plan.Remove)
	}
	if len(plan.Set) != 2 {
		t.Fatalf("Set = %v, want both peers configured", plan.Set)
	}
}

// Toggling the declaration BOTH ways has to converge, which is the requirement the section states.
func TestPlanConvergesToggledBothDirections(t *testing.T) {
	// endpoint -> none
	off := PlanPeerConvergence(
		map[string]string{peerA: "203.0.113.1"},
		[]WgPeer{{PubKey: peerA, AllowedIP: "10.0.0.1"}},
	)
	if len(off.Remove) != 1 {
		t.Fatalf("clearing an endpoint must remove first, got %v", off.Remove)
	}

	// none -> endpoint. `wg set` overwrites an endpoint, so no removal is needed this way round.
	on := PlanPeerConvergence(
		map[string]string{peerA: ""},
		[]WgPeer{{PubKey: peerA, AllowedIP: "10.0.0.1", EndpointIP: "203.0.113.9"}},
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

	plan := PlanPeerConvergence(map[string]string{}, desired)

	if len(plan.Remove) != 0 {
		t.Fatalf("Remove = %v, want nothing removed on an unknown current state", plan.Remove)
	}
	if len(plan.Set) != 1 {
		t.Fatalf("Set = %v, want the desired peer still applied", plan.Set)
	}
}

func TestPlanEmptyDesiredRemovesEveryPeer(t *testing.T) {
	// The last node leaving the cluster is a real state, not a bad input.
	current := map[string]string{peerA: "203.0.113.1", peerB: ""}

	plan := PlanPeerConvergence(current, nil)

	if !reflect.DeepEqual(sorted(plan.Remove), sorted([]string{peerA, peerB})) {
		t.Fatalf("Remove = %v, want every peer", plan.Remove)
	}
	if len(plan.Set) != 0 {
		t.Fatalf("Set = %v, want nothing configured", plan.Set)
	}
}

func TestParseWgEndpoints(t *testing.T) {
	out := peerA + "\t203.0.113.1:51820\n" +
		peerB + "\t(none)\n" +
		peerC + "\t[2001:db8::1]:51820\n"

	got := ParseWgEndpoints(out)

	want := map[string]string{
		peerA: "203.0.113.1",
		peerB: "",
		// Split from the right, or an IPv6 literal loses most of itself to its own colons.
		peerC: "2001:db8::1",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ParseWgEndpoints = %v, want %v", got, want)
	}
}

func TestParseWgEndpointsHandlesNoPeers(t *testing.T) {
	// A node whose peer set is empty prints nothing. That is zero peers, not a parse failure.
	for _, out := range []string{"", "\n", "   \n\n"} {
		if got := ParseWgEndpoints(out); len(got) != 0 {
			t.Fatalf("ParseWgEndpoints(%q) = %v, want empty", out, got)
		}
	}
}
