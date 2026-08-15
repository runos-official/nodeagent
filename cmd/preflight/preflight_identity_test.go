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

func addrEntry(ifname, local string, prefixLen int) idAddrEntry {
	e := idAddrEntry{IfName: ifname}
	e.AddrInfo = append(e.AddrInfo, struct {
		Local     string `json:"local"`
		PrefixLen int    `json:"prefixlen"`
	}{Local: local, PrefixLen: prefixLen})
	return e
}
