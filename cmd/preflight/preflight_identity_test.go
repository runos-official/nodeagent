package preflight

import (
	"strings"
	"testing"
)

// A node whose LAN sits in the Kubernetes pod range (172.25.0.0/16) or the
// service range (10.96.0.0/12) used to PASS preflight and then collide after
// install, when Cilium or kube-proxy claimed the same addresses. Finding out
// after install is far worse than being refused at join, so both ranges are
// guarded here (goal 27, W11 / defect E).
func TestReservedConflictsCoversPodAndServiceRanges(t *testing.T) {
	nets := idReservedNets()

	cases := []struct {
		name      string
		addr      string
		prefixLen int
		wantRange string
	}{
		{name: "pod range", addr: "172.25.4.10", prefixLen: 24, wantRange: "172.25.0.0/16"},
		{name: "service range", addr: "10.96.3.7", prefixLen: 24, wantRange: "10.96.0.0/12"},
		{name: "service range upper edge", addr: "10.111.255.254", prefixLen: 24, wantRange: "10.96.0.0/12"},
		{name: "wg0 node mesh", addr: "172.24.9.1", prefixLen: 24, wantRange: "172.24.0.0/16"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			addrs := []idAddrEntry{addrEntry("eth0", tc.addr, tc.prefixLen)}

			got := idReservedConflicts(addrs, nil, nets)
			if len(got) != 1 {
				t.Fatalf("conflicts = %v, want exactly one", got)
			}
			// The operator has to know WHICH interface to re-IP and WHICH range
			// it collided with. A message carrying only one of the two leaves
			// them guessing.
			if !strings.Contains(got[0], "eth0") {
				t.Errorf("conflict %q does not name the interface", got[0])
			}
			if !strings.Contains(got[0], tc.wantRange) {
				t.Errorf("conflict %q does not name range %s", got[0], tc.wantRange)
			}
		})
	}
}

// The wg1 user VPN range is a subset of the wg0 /16. Reporting the /16 for an
// address that is really in the /21 sends the operator to the wrong remedy, so
// the most specific range has to win.
func TestReservedConflictsReportsTheMostSpecificRange(t *testing.T) {
	addrs := []idAddrEntry{addrEntry("eth0", "172.24.201.5", 24)}

	got := idReservedConflicts(addrs, nil, idReservedNets())
	if len(got) != 1 {
		t.Fatalf("conflicts = %v, want exactly one", got)
	}
	if !strings.Contains(got[0], "172.24.200.0/21") {
		t.Errorf("conflict %q should name the wg1 range, not the enclosing /16", got[0])
	}
}

// RunOS's own CNI links hold pod-range addresses once a cluster is up. A
// re-install runs preflight again while cilium_host is still bound, so counting
// RunOS's own interfaces as a pre-existing conflict would block the node on the
// state RunOS itself created.
func TestReservedConflictsIgnoresRunosOwnInterfaces(t *testing.T) {
	addrs := []idAddrEntry{
		addrEntry("cilium_host", "172.25.0.203", 32),
		addrEntry("lxc9172513642eb", "172.25.0.44", 32),
		addrEntry("wg0", "172.24.33.1", 24),
	}
	routes := []idRouteEntry{
		{Dst: "172.25.0.0/24", Dev: "cilium_host", PrefSrc: "172.25.0.203"},
	}

	if got := idReservedConflicts(addrs, routes, idReservedNets()); len(got) != 0 {
		t.Errorf("conflicts = %v, want none for RunOS's own interfaces", got)
	}
}

// A LAN that merely CONTAINS a RunOS range is not a conflict: RunOS installs
// more specific routes and longest-prefix match sends the traffic the right
// way. Hetzner private networks hand out 10.x/8 addresses, so flagging a
// supernet would refuse nodes that work.
func TestReservedConflictsAllowsASupernetLan(t *testing.T) {
	addrs := []idAddrEntry{addrEntry("eth0", "10.0.0.5", 8)}

	if got := idReservedConflicts(addrs, nil, idReservedNets()); len(got) != 0 {
		t.Errorf("conflicts = %v, want none for a 10.0.0.5/8 LAN address", got)
	}
}

// A cluster's overlay range is drawn from the whole of RFC1918 since goal 27 W8,
// so a check that names 172.24.0.0/16 guards a range the cluster does not use and
// misses the range it does. The control plane passes the real one at join time.
//
// This is the OVER-REFUSAL half: a host on 172.24.x collides with nothing when its
// cluster sits on 10.223.80.0/24, and refusing it turns a working machine away.
func TestReservedNetsForClusterStopsRefusingTheLegacyRange(t *testing.T) {
	nets := idReservedNetsFor("10.223.80.0/24")
	addrs := []idAddrEntry{addrEntry("eno1", "172.24.9.1", 24)}

	if got := idReservedConflicts(addrs, nil, nets); len(got) != 0 {
		t.Errorf("conflicts = %v, want none: 172.24.9.1 does not collide with a cluster on 10.223.80.0/24", got)
	}
}

// The UNDER-PROTECTION half, and the collision the check exists to prevent. The
// host's LAN is the cluster's own overlay range, so wg0 addresses would duplicate
// LAN addresses once the node is up. The hardcoded list could never catch this,
// because the range is not known until the cluster is created.
func TestReservedNetsForClusterCatchesTheRealCollision(t *testing.T) {
	nets := idReservedNetsFor("10.223.80.0/24")
	addrs := []idAddrEntry{addrEntry("eno1", "10.223.80.5", 24)}

	got := idReservedConflicts(addrs, nil, nets)
	if len(got) != 1 {
		t.Fatalf("conflicts = %v, want exactly one", got)
	}
	if !strings.Contains(got[0], "10.223.80.0/24") {
		t.Errorf("conflict %q does not name the cluster range", got[0])
	}
	if !strings.Contains(got[0], "eno1") {
		t.Errorf("conflict %q does not name the interface", got[0])
	}
}

// A legacy cluster still holds a 172.24.<octet>.0/24, and passing it narrows the
// check rather than widening it: the node's OWN /24 is guarded, the rest of the
// /16 belongs to other clusters and is not this node's business.
func TestReservedNetsForClusterNarrowsALegacyRange(t *testing.T) {
	nets := idReservedNetsFor("172.24.5.0/24")

	own := []idAddrEntry{addrEntry("eno1", "172.24.5.9", 24)}
	if got := idReservedConflicts(own, nil, nets); len(got) != 1 {
		t.Errorf("conflicts = %v, want one for the cluster's own legacy range", got)
	}

	other := []idAddrEntry{addrEntry("eno1", "172.24.9.9", 24)}
	if got := idReservedConflicts(other, nil, nets); len(got) != 0 {
		t.Errorf("conflicts = %v, want none: 172.24.9.9 belongs to another cluster's range", got)
	}
}

// THE COMPATIBILITY GUARANTEE, and the reason this fix is safe to ship on the
// join path. An older control plane passes no range, and an unparseable one is
// treated the same way. Both must behave EXACTLY as the check did before, so no
// node that joins successfully today can start failing.
func TestReservedNetsForFallsBackToTheHardcodedList(t *testing.T) {
	for _, supplied := range []string{"", "not-a-cidr", "172.24.0.0/33"} {
		nets := idReservedNetsFor(supplied)

		if len(nets) != len(idRunosReservedCIDRs) {
			t.Fatalf("supplied %q: got %d ranges, want the %d hardcoded ones", supplied, len(nets), len(idRunosReservedCIDRs))
		}
		for i, want := range idRunosReservedCIDRs {
			if nets[i].CIDR != want.CIDR || nets[i].Label != want.Label {
				t.Errorf("supplied %q: range %d = %s/%s, want %s/%s", supplied, i, nets[i].CIDR, nets[i].Label, want.CIDR, want.Label)
			}
		}
	}
}

func addrEntry(ifname, local string, prefixLen int) idAddrEntry {
	e := idAddrEntry{IfName: ifname}
	e.AddrInfo = append(e.AddrInfo, struct {
		Local     string `json:"local"`
		PrefixLen int    `json:"prefixlen"`
	}{Local: local, PrefixLen: prefixLen})
	return e
}
