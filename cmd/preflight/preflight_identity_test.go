package preflight

import (
	"net"
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

// The wg1 user VPN range (172.24.200.0/21) is NO LONGER guarded on the node (goal 27,
// wg1-user-vpn-range-preflight). User VPN addressing is account-scoped and pooled, so the range
// is a conductor fact; conductor refuses a wg1 install onto a node whose networks overlap the
// account's real range. A machine on 172.24.200.0/24 must register without a preflight refusal.
func TestReservedConflictsNoLongerGuardsTheWg1Range(t *testing.T) {
	addrs := []idAddrEntry{addrEntry("eth0", "172.24.201.5", 24)}

	got := idReservedConflicts(addrs, nil, idReservedNets())
	if len(got) != 0 {
		t.Fatalf("conflicts = %v, want none: the wg1 range is not guarded on the node anymore", got)
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

func addrEntry(ifname, local string, prefixLen int) idAddrEntry {
	e := idAddrEntry{IfName: ifname}
	e.AddrInfo = append(e.AddrInfo, struct {
		Local     string `json:"local"`
		PrefixLen int    `json:"prefixlen"`
	}{Local: local, PrefixLen: prefixLen})
	return e
}

// idRouteOverlap's contract is "AT LEAST AS SPECIFIC as the reserved range", because only such a
// route wins longest-prefix match against RunOS's own. Found by the goal 28 adversarial review
// 2026-08-19: the code tested only whether the destination's base address fell inside the reserved
// range, so an ALIGNED SUPERNET was reported and blocked the install. 10.96.0.0/11 is the one
// destination in all of IPv4 that hits it: its base address is 10.96.0.0, which sits inside
// 10.96.0.0/12, while /11 is less specific and loses to RunOS's /12.
func TestIdRouteOverlapAppliesTheAtLeastAsSpecificRule(t *testing.T) {
	nets := idReservedNetsForTest(t)

	for _, tc := range []struct {
		dst  string
		want bool
		why  string
	}{
		{"10.96.0.10", true, "a host route inside the service range is the goal 28 blocker itself"},
		{"10.96.0.0/12", true, "the reserved range exactly"},
		{"10.96.0.0/16", true, "more specific, wins longest-prefix match"},
		{"10.96.0.0/11", false, "an ALIGNED SUPERNET is less specific and loses; reporting it false-blocks"},
		{"10.0.0.0/8", false, "a supernet whose base is outside the reserved range"},
		{"172.25.1.0/24", true, "more specific than the pod range"},
		{"172.25.0.0/15", false, "less specific than the pod range"},
		{"192.168.1.0/24", false, "nothing to do with the reserved ranges"},
	} {
		got := idRouteOverlap(tc.dst, nets) != nil
		if got != tc.want {
			t.Errorf("idRouteOverlap(%q) = %v, want %v (%s)", tc.dst, got, tc.want, tc.why)
		}
	}
}

// idReservedNetsForTest builds the two ranges the check guards, without depending on the
// production constructor's other inputs.
func idReservedNetsForTest(t *testing.T) []idReservedNet {
	t.Helper()
	var out []idReservedNet
	for _, c := range []struct{ cidr, label string }{
		{"10.96.0.0/12", "the Kubernetes service range"},
		{"172.25.0.0/16", "the Kubernetes pod range"},
	} {
		_, n, err := net.ParseCIDR(c.cidr)
		if err != nil {
			t.Fatalf("bad fixture %s: %v", c.cidr, err)
		}
		out = append(out, idReservedNet{CIDR: c.cidr, Label: c.label, Net: n})
	}
	return out
}
