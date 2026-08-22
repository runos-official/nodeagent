package commons

import (
	"net"
	"reflect"
	"testing"
)

// Goal 27, design-peering-mesh. A cross-cluster peer needs a kernel route or its traffic leaves
// by the default gateway while the tunnel looks up. These pin the plan: routes for exactly the
// out-of-prefix peers, none for same-cluster ones, and removals for peers that are gone.

func mustCIDR(t *testing.T, s string) *net.IPNet {
	t.Helper()
	_, n, err := net.ParseCIDR(s)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func TestOnlyPeersOutsideTheOwnPrefixGetRoutes(t *testing.T) {
	own := []*net.IPNet{mustCIDR(t, "10.128.252.1/24")}
	desired := []WgPeer{
		{PubKey: "a", AllowedIP: "10.128.252.7"}, // same cluster: covered by the connected route
		{PubKey: "b", AllowedIP: "10.137.65.2"},  // peered cluster
		{PubKey: "c", AllowedIP: "10.137.65.1"},  // peered cluster
	}
	add, remove := PlanPeerRoutes(own, nil, desired, nil)
	if !reflect.DeepEqual(add, []string{"10.137.65.1", "10.137.65.2"}) {
		t.Fatalf("add = %v, want the two far peers in sorted order", add)
	}
	if len(remove) != 0 {
		t.Fatalf("remove = %v, want nothing", remove)
	}
}

func TestRoutesConvergeToExactlyTheDesiredSet(t *testing.T) {
	own := []*net.IPNet{mustCIDR(t, "10.128.252.1/24")}
	desired := []WgPeer{{PubKey: "b", AllowedIP: "10.137.65.2"}}
	current := []string{"10.137.65.2", "10.137.65.9", "10.140.0.4"}
	add, remove := PlanPeerRoutes(own, nil, desired, current)
	if len(add) != 0 {
		t.Fatalf("add = %v, want nothing: the route is already there", add)
	}
	// A revoked peering must take its routes with it, or a stale /32 keeps sending traffic
	// for an address that now belongs to nobody we know into the tunnel.
	if !reflect.DeepEqual(remove, []string{"10.137.65.9", "10.140.0.4"}) {
		t.Fatalf("remove = %v, want the two routes for peers that are gone", remove)
	}
}

func TestNoPeeringsMeansNoRoutesEitherWay(t *testing.T) {
	own := []*net.IPNet{mustCIDR(t, "10.128.252.1/24")}
	desired := []WgPeer{{PubKey: "a", AllowedIP: "10.128.252.7"}, {PubKey: "z", AllowedIP: "10.128.252.200"}}
	add, remove := PlanPeerRoutes(own, nil, desired, nil)
	if len(add) != 0 || len(remove) != 0 {
		t.Fatalf("a cluster with no peerings must plan nothing, got add=%v remove=%v", add, remove)
	}
}

func TestGarbageAllowedIpsAreIgnored(t *testing.T) {
	own := []*net.IPNet{mustCIDR(t, "10.128.252.1/24")}
	add, _ := PlanPeerRoutes(own, nil, []WgPeer{{PubKey: "x", AllowedIP: "not-an-ip"}, {PubKey: "y", AllowedIP: ""}}, nil)
	if len(add) != 0 {
		t.Fatalf("unparseable allowed IPs must not become routes, got %v", add)
	}
}

func TestParsePeerRoutesReadsIpJsonOutput(t *testing.T) {
	// A /32 prints without a prefix length; anything else keeps it. Both are returned as bare
	// addresses so a wrongly-masked stray converges away.
	out := []byte(`[{"dst":"10.137.65.1","dev":"wg0","protocol":"201","scope":"link","flags":[]},{"dst":"10.140.0.0/30","dev":"wg0","protocol":"201","scope":"link","flags":[]}]`)
	got, err := ParsePeerRoutes(out)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, []string{"10.137.65.1", "10.140.0.0"}) {
		t.Fatalf("got %v", got)
	}
	// No routes at all prints nothing, not "[]".
	if got, err := ParsePeerRoutes(nil); err != nil || len(got) != 0 {
		t.Fatalf("empty output must parse to no routes, got %v %v", got, err)
	}
}

// A peer that hosts VMs contributes their pool /32s to the route plan too (goal 27,
// vm-group-range-routing); an extra inside this node's own range needs no route.
func TestPlanPeerRoutesIncludesExtraAllowedIPs(t *testing.T) {
	_, own, _ := net.ParseCIDR("10.128.252.0/24")
	desired := []WgPeer{{
		PubKey:          "k",
		AllowedIP:       "172.24.32.1",
		ExtraAllowedIPs: []string{"10.77.5.2", "10.128.252.9"},
	}}
	add, remove := PlanPeerRoutes([]*net.IPNet{own}, nil, desired, nil)
	if len(remove) != 0 {
		t.Fatalf("remove = %v", remove)
	}
	want := []string{"10.77.5.2", "172.24.32.1"}
	if len(add) != 2 || add[0] != want[0] || add[1] != want[1] {
		t.Fatalf("add = %v want %v (extras routed, own-range extra skipped)", add, want)
	}
}

// MEASURED on the lab 2026-08-22/23, and it breaks nested RunOS (goal 28) by a race.
//
// A RunOS VM that becomes a RunOS node sits on its VM group's pool subnet, say 10.158.31.0/24 on
// enp1s0, alongside the other guests it must peer with. The PARENT cluster's node legitimately
// advertises every guest address as an allowed-ip, because from the parent's side they are its VMs
// reachable over its overlay. The nested guest then installed:
//
//	10.158.31.0/24  dev enp1s0  proto kernel  scope link   <- its own direct path
//	10.158.31.2     dev wg0     proto 201                  <- MORE SPECIFIC, so it wins
//
// The /32 beats the connected route, so a WireGuard handshake addressed to 10.158.31.2:51820 is
// routed INTO the tunnel whose endpoint that address is. Circular, and the tunnel can never come
// up: tcpdump on the far guest saw zero packets, handshakes stayed at 0, and two worker joins died
// with INSTALL_ERROR. Deleting the route by hand had it re-added within seconds, and the neighbour
// went from 1.36 ms (measured before these guests were nodes) to 100% loss.
//
// It is a race rather than a hard failure: if the guest-to-guest handshake completes before the
// parent's /32s land, keepalives hold the session open and everything looks fine. The same
// procedure worked once and failed twice.
//
// A directly-connected neighbour must never be routed through a tunnel.
func TestPeerOnADirectlyConnectedSubnetGetsNoTunnelRoute(t *testing.T) {
	own := []*net.IPNet{mustCIDR(t, "10.128.252.3/24")}  // wg0, the nested overlay
	local := []*net.IPNet{mustCIDR(t, "10.158.31.3/24")} // enp1s0, the VM pool subnet
	desired := []WgPeer{
		{PubKey: "parent", AllowedIP: "10.58.72.1", ExtraAllowedIPs: []string{
			"10.158.31.2", // a guest on MY OWN subnet: must stay on enp1s0
			"10.158.31.3", // ME: must never be routed anywhere
			"10.158.31.4", // another guest on my subnet
			"10.140.46.2", // a guest on a DIFFERENT pool subnet: legitimately needs the tunnel
		}},
	}

	add, _ := PlanPeerRoutes(own, local, desired, nil)

	for _, bad := range []string{"10.158.31.2", "10.158.31.3", "10.158.31.4"} {
		for _, got := range add {
			if got == bad {
				t.Errorf("installed a tunnel route for %s, which is directly connected on enp1s0; "+
					"this is what makes the guest-to-guest handshake circular", bad)
			}
		}
	}
	want := map[string]bool{"10.58.72.1": true, "10.140.46.2": true}
	for _, got := range add {
		if !want[got] {
			t.Errorf("unexpected route %s", got)
		}
	}
	if len(add) != 2 {
		t.Fatalf("add = %v, want exactly the two off-subnet peers", add)
	}
}

// An existing circular route must be REMOVED, not merely not-added: the boxes that hit this had
// them already installed and the reconciler kept re-adding them.
func TestAnExistingCircularRouteIsRemoved(t *testing.T) {
	own := []*net.IPNet{mustCIDR(t, "10.128.252.3/24")}
	local := []*net.IPNet{mustCIDR(t, "10.158.31.3/24")}
	desired := []WgPeer{{PubKey: "parent", AllowedIP: "10.58.72.1", ExtraAllowedIPs: []string{"10.158.31.2"}}}

	_, remove := PlanPeerRoutes(own, local, desired, []string{"10.158.31.2", "10.58.72.1"})
	if len(remove) != 1 || remove[0] != "10.158.31.2" {
		t.Fatalf("remove = %v, want the circular route gone", remove)
	}
}
