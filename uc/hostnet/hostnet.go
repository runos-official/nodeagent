// Package hostnet reports the networks this machine already has.
//
// It exists so the control plane can pick a cluster overlay range that avoids them (ADR-0003).
// The list travels with node registration: for a cluster's FIRST node it decides which range is
// handed out, and for a later node it is checked against the range the cluster already holds.
package hostnet

import (
	"encoding/json"
	"fmt"
	"net"
	"os/exec"
	"sort"
	"strings"
	"time"
)

// managedPrefixes are interfaces RunOS or its CNI creates.
//
// EXCLUDING THEM IS LOAD-BEARING, not tidiness. wg0 holds this node's address INSIDE the cluster's
// overlay range, and cilium_host holds one inside the pod range. Reporting them would make every
// reinstall of an existing node look like a collision with its own cluster, and the machine would
// be refused forever by state RunOS itself created.
var managedPrefixes = []string{"wg", "cilium_", "lxc"}

// managedNames are the remaining CNI interfaces, matched exactly rather than by prefix.
var managedNames = map[string]bool{"cni0": true, "kube-ipvs0": true, "lo": true}

// routeTimeout bounds the one external command. Registration is a person waiting on an install, so
// a hung `ip` must degrade to "no routes reported" rather than stall the join.
const routeTimeout = 5 * time.Second

// Collect returns the machine's own networks as CIDR strings, deduplicated and sorted.
//
// SORTED so the value is stable across calls. An unstable list would make two registrations of the
// same machine look like different machines to anything that compares them.
//
// BEST EFFORT THROUGHOUT. Every failure yields fewer entries, never an error. A machine that
// cannot enumerate its own interfaces still has to be able to join; the cost is that the range is
// chosen with less information, which is exactly what an older agent already does.
func Collect() []string {
	seen := map[string]bool{}

	for _, cidr := range interfaceNetworks() {
		seen[cidr] = true
	}
	for _, cidr := range routeNetworks() {
		seen[cidr] = true
	}

	out := make([]string, 0, len(seen))
	for cidr := range seen {
		out = append(out, cidr)
	}
	sort.Strings(out)
	return out
}

// interfaceNetworks reads addresses from the standard library rather than by shelling out, so the
// common case needs no external binary at all.
//
// It reports the NETWORK, not the address: 198.51.100.225/24 becomes 198.51.100.0/24, which is what
// a range has to be compared against.
func interfaceNetworks() []string {
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil
	}

	var out []string
	for _, iface := range ifaces {
		if isManaged(iface.Name) {
			continue
		}
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, addr := range addrs {
			ipNet, ok := addr.(*net.IPNet)
			if !ok || ipNet.IP.To4() == nil {
				// IPv4 only: RunOS allocates no IPv6, so an IPv6 network can never collide.
				continue
			}
			out = append(out, (&net.IPNet{IP: ipNet.IP.Mask(ipNet.Mask), Mask: ipNet.Mask}).String())
		}
	}
	return out
}

type routeEntry struct {
	Dst string `json:"dst"`
	Dev string `json:"dev"`
}

// routeNetworks reads route destinations, which catch what interface addresses miss: a network
// this machine reaches through a gateway without holding an address on it, which is how a VPN or a
// second site shows up.
func routeNetworks() []string {
	out, err := run("ip", "-j", "route")
	if err != nil {
		return nil
	}
	return parseRouteNetworks(out)
}

// parseRouteNetworks is the pure half of routeNetworks, split so the filtering can be tested
// without an `ip` binary or a real routing table.
func parseRouteNetworks(out string) []string {
	var routes []routeEntry
	if err := json.Unmarshal([]byte(out), &routes); err != nil {
		return nil
	}

	var networks []string
	for _, r := range routes {
		if isManaged(r.Dev) {
			continue
		}
		if r.Dst == "" || r.Dst == "default" {
			continue
		}
		dst := r.Dst
		if !strings.Contains(dst, "/") {
			// A host route. Written as a /32 so it parses like everything else.
			dst += "/32"
		}
		_, network, err := net.ParseCIDR(dst)
		if err != nil || network.IP.To4() == nil {
			continue
		}
		networks = append(networks, network.String())
	}
	return networks
}

func isManaged(dev string) bool {
	if managedNames[dev] {
		return true
	}
	for _, p := range managedPrefixes {
		if strings.HasPrefix(dev, p) {
			return true
		}
	}
	return false
}

func run(name string, args ...string) (string, error) {
	cmd := exec.Command(name, args...)
	done := make(chan struct{})
	var out []byte
	var err error
	go func() {
		out, err = cmd.Output()
		close(done)
	}()
	select {
	case <-done:
		if err != nil {
			return "", err
		}
		return string(out), nil
	case <-time.After(routeTimeout):
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
		return "", fmt.Errorf("%s timed out", name)
	}
}
