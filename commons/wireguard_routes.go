package commons

import (
	"encoding/json"
	"fmt"
	"net"
	"os/exec"
	"sort"

	"github.com/runos-official/nodeagent/roslog"
)

// Kernel routes for peers OUTSIDE this node's own overlay range (goal 27, design-peering-mesh).
//
// WHY THIS EXISTS. wg0 is brought up with `Address = <vpnIp>/24`, which gives the kernel one
// connected route: the cluster's own /24 goes to wg0. Peers are then added with `wg set`, which
// configures WireGuard's cryptokey routing (allowed-ips) and adds NO kernel routes at all. That
// was fine while every peer lived inside the /24. A peer from a PEERED CLUSTER holds an address
// in that cluster's range, so the kernel has no route for it: traffic for it leaves by the
// default gateway and vanishes, and WireGuard never sees it. The handshake can still complete,
// because the far side may initiate, which makes the tunnel look up while carrying nothing.
//
// So every peer whose allowed IP is outside wg0's own prefixes gets a /32 route on wg0, and the
// routes converge to EXACTLY that set: a peer that is withdrawn loses its route. The routes are
// tagged with their own routing-protocol number so they can be listed and removed without ever
// touching the connected route or anything wg-quick or the operator added.
//
// Same-cluster peers need nothing here, and get nothing: their addresses are inside the
// connected /24. On a cluster with no peerings this whole file is a no-op.

// PeerRouteProto is the rtnetlink protocol number the peer routes carry. Any number in 1..255
// that is not a well-known one (kernel=2, boot=3, static=4, dhcp=16, ...) will do; the value only
// has to be stable so the routes can be found again. It is written and read as a number, so it
// needs no entry in /etc/iproute2/rt_protos.
const PeerRouteProto = "201"

// wgInterface is the interface the routes hang off.
const wgInterface = "wg0"

// PlanPeerRoutes works out which /32 routes to add and remove so that exactly the desired
// out-of-prefix peers are routed to wg0.
//
// ownPrefixes are wg0's own addresses with their masks; a peer inside any of them needs no
// route. current is the set of peer routes already on the interface (destination addresses,
// as returned by CurrentPeerRoutes). Pure, so it can be tested without an interface.
//
// The add list is sorted so the same inputs always apply in the same order, which keeps logs
// comparable between passes.
func PlanPeerRoutes(ownPrefixes []*net.IPNet, desired []WgPeer, current []string) (add []string, remove []string) {
	want := make(map[string]bool)
	for _, p := range desired {
		// The peer's own address plus the VM pool addresses it hosts (goal 27,
		// vm-group-range-routing): every out-of-prefix /32 the tunnel must carry gets a route.
		for _, addr := range append([]string{p.AllowedIP}, p.ExtraAllowedIPs...) {
			ip := net.ParseIP(addr)
			if ip == nil || ip.To4() == nil {
				continue
			}
			if insideAny(ip, ownPrefixes) {
				continue
			}
			want[ip.String()] = true
		}
	}

	have := make(map[string]bool, len(current))
	for _, dst := range current {
		have[dst] = true
	}

	for dst := range want {
		if !have[dst] {
			add = append(add, dst)
		}
	}
	for dst := range have {
		if !want[dst] {
			remove = append(remove, dst)
		}
	}
	sort.Strings(add)
	sort.Strings(remove)
	return add, remove
}

func insideAny(ip net.IP, prefixes []*net.IPNet) bool {
	for _, p := range prefixes {
		if p != nil && p.Contains(ip) {
			return true
		}
	}
	return false
}

// Wg0Prefixes returns wg0's own IPv4 addresses with their masks.
func Wg0Prefixes() ([]*net.IPNet, error) {
	iface, err := net.InterfaceByName(wgInterface)
	if err != nil {
		return nil, fmt.Errorf("%s not found: %w", wgInterface, err)
	}
	addrs, err := iface.Addrs()
	if err != nil {
		return nil, fmt.Errorf("could not read %s addresses: %w", wgInterface, err)
	}
	var out []*net.IPNet
	for _, a := range addrs {
		if n, ok := a.(*net.IPNet); ok && n.IP.To4() != nil {
			out = append(out, n)
		}
	}
	return out, nil
}

// ipRouteEntry is the slice of `ip -j route` output this reads.
type ipRouteEntry struct {
	Dst string `json:"dst"`
}

// ParsePeerRoutes reads `ip -j route show dev wg0 protocol <PeerRouteProto>` output into the list
// of destination addresses. A destination printed without a prefix length is a /32, which is the
// only shape this file ever writes; anything else on the interface under this protocol number is
// still returned so a stray route gets converged away rather than accumulating.
func ParsePeerRoutes(out []byte) ([]string, error) {
	if len(out) == 0 {
		return nil, nil
	}
	var entries []ipRouteEntry
	if err := json.Unmarshal(out, &entries); err != nil {
		return nil, fmt.Errorf("could not parse ip route output: %w", err)
	}
	var dsts []string
	for _, e := range entries {
		if e.Dst == "" {
			continue
		}
		if ip, _, err := net.ParseCIDR(e.Dst); err == nil {
			// A prefixed entry. Only a /32 is ours to have written; report the address so it is
			// removed and re-added correctly if the mask is wrong.
			dsts = append(dsts, ip.String())
			continue
		}
		if ip := net.ParseIP(e.Dst); ip != nil {
			dsts = append(dsts, ip.String())
		}
	}
	return dsts, nil
}

// CurrentPeerRoutes lists the peer routes on wg0 right now.
//
// A read failure returns an EMPTY list and the error, which plans NO removals: the same safe
// direction as CurrentWgPeers. Mistaking a failed read for "no routes" would remove nothing
// anyway, and the adds are idempotent (`ip route replace`), so a failed read costs at most a
// stale route that the next pass removes.
func CurrentPeerRoutes() ([]string, error) {
	out, err := exec.Command("ip", "-j", "route", "show", "dev", wgInterface, "protocol", PeerRouteProto).CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("ip route show failed: %w (%s)", err, string(out))
	}
	return ParsePeerRoutes(out)
}

// ApplyPeerRoutes converges wg0's peer routes to exactly the out-of-prefix addresses in desired.
//
// Every failure is logged and skipped rather than returned, for the same reason a failing peer
// is: the peers are already applied by the time this runs, and one route that cannot be written
// must not stop the others. Called by BOTH peer paths (the agent stream and the manual sync) so
// the routes cannot depend on which one delivered the peer set.
func ApplyPeerRoutes(desired []WgPeer) {
	prefixes, err := Wg0Prefixes()
	if err != nil {
		// Mid-install the peer set can arrive before wg0 exists. That is expected once per node,
		// not a fault: the next peer set converges the routes. Said at info level for that reason.
		roslog.I("wg0 is not up yet; peer routes converge with the next peer set", "reason", err.Error())
		return
	}
	current, err := CurrentPeerRoutes()
	if err != nil {
		roslog.E("Could not read the current peer routes; adding without removals", err)
	}

	add, remove := PlanPeerRoutes(prefixes, desired, current)
	for _, dst := range remove {
		if out, err := exec.Command("ip", "route", "del", dst+"/32", "dev", wgInterface, "protocol", PeerRouteProto).CombinedOutput(); err != nil {
			roslog.E("Could not remove a peer route", err, "dst", dst, "output", string(out))
			continue
		}
		roslog.I("Removed peer route", "dst", dst)
	}
	for _, dst := range add {
		if out, err := exec.Command("ip", "route", "replace", dst+"/32", "dev", wgInterface, "protocol", PeerRouteProto).CombinedOutput(); err != nil {
			roslog.E("Could not add a peer route", err, "dst", dst, "output", string(out))
			continue
		}
		roslog.I("Added peer route", "dst", dst)
	}
}
