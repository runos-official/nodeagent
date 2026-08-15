package commons

import (
	"net"
	"reflect"
	"testing"
)

func mustPrefix(t *testing.T, cidr string) *net.IPNet {
	t.Helper()
	_, n, err := net.ParseCIDR(cidr)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

// A peering meshes whole clusters, so the far /24 is allowed, one rule per peer cluster. A peer
// inside this node's own wg0 range is local and needs no rule (goal 27, peering-loose-ends).
func TestDesiredPeerRangesCollapsesToFarSlash24s(t *testing.T) {
	own := []*net.IPNet{mustPrefix(t, "10.128.252.0/24")}
	peers := []WgPeer{
		{PubKey: "a", AllowedIP: "10.128.252.5"}, // local, ignored
		{PubKey: "b", AllowedIP: "172.24.32.1"},  // peered cluster
		{PubKey: "c", AllowedIP: "172.24.32.9"},  // same peered cluster -> one rule
		{PubKey: "d", AllowedIP: "10.59.204.7"},  // another peered cluster
		{PubKey: "e", AllowedIP: ""},             // identity only, ignored
	}
	got := desiredPeerRanges(own, peers)
	want := []string{"10.59.204.0/24", "172.24.32.0/24"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v want %v", got, want)
	}
}

// Only rules carrying the marker comment are the converge's own; an operator's allow is invisible
// to it and never removed. The active flag gates the whole converge.
func TestParseUfwPeerRanges(t *testing.T) {
	out := []byte(`Status: active

To                         Action      From
--                         ------      ----
Anywhere                   ALLOW       10.59.204.0/24             # RunOS peered cluster range
Anywhere                   ALLOW       172.24.0.0/16              # WireGuard VPN network
22/tcp                     ALLOW       Anywhere
Anywhere                   ALLOW       172.24.32.0/24             # RunOS peered cluster range
`)
	active, ranges := parseUfwPeerRanges(out)
	if !active {
		t.Fatal("status active not parsed")
	}
	want := []string{"10.59.204.0/24", "172.24.32.0/24"}
	if !reflect.DeepEqual(ranges, want) {
		t.Fatalf("ranges = %v want %v (must skip the operator's own 172.24/16 rule)", ranges, want)
	}
	inactive, _ := parseUfwPeerRanges([]byte("Status: inactive\n"))
	if inactive {
		t.Fatal("inactive status read as active")
	}
}

func TestPlanUfwPeerRanges(t *testing.T) {
	add, remove := planUfwPeerRanges(
		[]string{"172.24.32.0/24", "10.10.0.0/24"},
		[]string{"172.24.32.0/24", "10.59.204.0/24"},
	)
	if !reflect.DeepEqual(add, []string{"10.59.204.0/24"}) {
		t.Fatalf("add = %v", add)
	}
	if !reflect.DeepEqual(remove, []string{"10.10.0.0/24"}) {
		t.Fatalf("remove = %v", remove)
	}
}

// A far VM group's /24 needs its own ufw allow rule exactly like the far cluster's /24 does.
func TestDesiredPeerRangesIncludesExtraAllowedIPRanges(t *testing.T) {
	own := []*net.IPNet{mustPrefix(t, "10.128.252.0/24")}
	peers := []WgPeer{{PubKey: "k", AllowedIP: "172.24.32.1", ExtraAllowedIPs: []string{"10.77.5.2", "10.77.5.9"}}}
	got := desiredPeerRanges(own, peers)
	want := []string{"10.77.5.0/24", "172.24.32.0/24"}
	if len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("got %v want %v", got, want)
	}
}
