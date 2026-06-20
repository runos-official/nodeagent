package vip

import (
	"fmt"
	"net"
	"sync"

	"github.com/runos-official/nodeagent/commons"
	"github.com/runos-official/nodeagent/roslog"
)

const (
	wg0Interface = "wg0"
	vipOctet1    = 172
	vipOctet2    = 24
	// vipHostOctet is the last usable host in the /24 overlay subnet
	// (.255 is broadcast). The VIP is 172.24.<cluster_idx>.254/32.
	vipHostOctet = 254
)

var (
	mu                    sync.Mutex
	lastAppliedGeneration int64
)

// ClusterIdx parses the 172.24.X.Y node address on wg0 and returns X.
func ClusterIdx() (int, error) {
	iface, err := net.InterfaceByName(wg0Interface)
	if err != nil {
		return 0, fmt.Errorf("interface %s not found: %w", wg0Interface, err)
	}
	addrs, err := iface.Addrs()
	if err != nil {
		return 0, fmt.Errorf("failed to list addresses on %s: %w", wg0Interface, err)
	}
	return parseClusterIdx(addrs)
}

// parseClusterIdx is the pure, testable core of ClusterIdx. Given a list of
// interface addresses it returns the cluster index (the 3rd octet of the
// 172.24.X.Y node address). The VIP address (172.24.X.200) is ignored so that
// this still works when called while the VIP is bound.
func parseClusterIdx(addrs []net.Addr) (int, error) {
	for _, addr := range addrs {
		ipNet, ok := addr.(*net.IPNet)
		if !ok {
			continue
		}
		ip4 := ipNet.IP.To4()
		if ip4 == nil {
			continue
		}
		if ip4[0] != vipOctet1 || ip4[1] != vipOctet2 {
			continue
		}
		if int(ip4[3]) == vipHostOctet {
			continue
		}
		return int(ip4[2]), nil
	}
	return 0, fmt.Errorf("no %d.%d.X.Y node address found on %s", vipOctet1, vipOctet2, wg0Interface)
}

// Cidr returns the VIP CIDR for the given cluster index, e.g. "172.24.7.254/32".
func Cidr(idx int) string {
	return fmt.Sprintf("%d.%d.%d.%d/32", vipOctet1, vipOctet2, idx, vipHostOctet)
}

// IsBound returns true if cidr is currently bound to wg0.
func IsBound(cidr string) (bool, error) {
	wantIP, wantNet, err := net.ParseCIDR(cidr)
	if err != nil {
		return false, fmt.Errorf("invalid cidr %q: %w", cidr, err)
	}
	iface, err := net.InterfaceByName(wg0Interface)
	if err != nil {
		return false, fmt.Errorf("interface %s not found: %w", wg0Interface, err)
	}
	addrs, err := iface.Addrs()
	if err != nil {
		return false, fmt.Errorf("failed to list addresses on %s: %w", wg0Interface, err)
	}
	wantPrefix, _ := wantNet.Mask.Size()
	for _, addr := range addrs {
		ipNet, ok := addr.(*net.IPNet)
		if !ok {
			continue
		}
		prefix, _ := ipNet.Mask.Size()
		if ipNet.IP.Equal(wantIP) && prefix == wantPrefix {
			return true, nil
		}
	}
	return false, nil
}

// Apply reconciles wg0 to the desired VIP-bound state. When enforceFreshness is
// true and generation <= lastAppliedGeneration, the instruction is rejected as
// stale and (false, nil) is returned. Otherwise the wg0 state is converged and
// lastAppliedGeneration is advanced to generation.
//
// Startup reconcile calls this with enforceFreshness=false because the
// AmIVIPHolder response is authoritative; inbound VIP_ASSIGN / VIP_RELEASE pass
// true so replayed messages are dropped.
func Apply(desired bool, generation int64, enforceFreshness bool) (bool, error) {
	mu.Lock()
	defer mu.Unlock()

	if enforceFreshness && generation <= lastAppliedGeneration {
		roslog.I("Ignoring stale VIP instruction",
			"generation", generation, "last_applied", lastAppliedGeneration, "desired", desired)
		return false, nil
	}

	idx, err := ClusterIdx()
	if err != nil {
		return false, err
	}
	cidr := Cidr(idx)

	bound, err := IsBound(cidr)
	if err != nil {
		return false, err
	}

	switch {
	case desired && !bound:
		if err := bindVIP(cidr); err != nil {
			return false, err
		}
		roslog.I("VIP bound", "cidr", cidr, "generation", generation)
	case !desired && bound:
		if err := unbindVIP(cidr); err != nil {
			return false, err
		}
		roslog.I("VIP unbound", "cidr", cidr, "generation", generation)
	default:
		roslog.D("VIP already in desired state", "cidr", cidr, "desired", desired, "generation", generation)
	}

	lastAppliedGeneration = generation
	return true, nil
}

// ForceDrop removes the VIP from wg0 if currently bound. It does not advance
// lastAppliedGeneration, so a subsequent VIP_ASSIGN from Nodeward with a higher
// generation will re-bind cleanly. Used by the heartbeat self-drop path.
func ForceDrop() error {
	mu.Lock()
	defer mu.Unlock()

	idx, err := ClusterIdx()
	if err != nil {
		roslog.D("Skipping VIP force-drop, cannot determine cluster idx", "error", err.Error())
		return nil
	}
	cidr := Cidr(idx)

	bound, err := IsBound(cidr)
	if err != nil {
		return err
	}
	if !bound {
		return nil
	}
	if err := unbindVIP(cidr); err != nil {
		return err
	}
	roslog.I("VIP force-dropped", "cidr", cidr)
	return nil
}

func bindVIP(cidr string) error {
	cmd := fmt.Sprintf("ip addr add %s dev %s", cidr, wg0Interface)
	if _, err := commons.ExecuteCommandGetResponse2(cmd); err != nil {
		return fmt.Errorf("failed to bind VIP %s: %w", cidr, err)
	}
	return nil
}

func unbindVIP(cidr string) error {
	cmd := fmt.Sprintf("ip addr del %s dev %s", cidr, wg0Interface)
	if _, err := commons.ExecuteCommandGetResponse2(cmd); err != nil {
		return fmt.Errorf("failed to unbind VIP %s: %w", cidr, err)
	}
	return nil
}
