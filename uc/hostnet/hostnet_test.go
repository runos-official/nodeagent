package hostnet

import (
	"slices"
	"testing"
)

// THE REINSTALL TRAP. wg0 holds this node's address inside its own cluster's overlay range, and
// cilium_host holds one inside the pod range. Reporting either would make every reinstall of an
// existing node look like a collision with its own cluster, and the machine would be refused
// forever on state RunOS itself created.
func TestManagedInterfacesAreNeverReported(t *testing.T) {
	for _, dev := range []string{"wg0", "wg1", "cilium_host", "cilium_net", "lxc9172513642eb", "cni0", "kube-ipvs0", "lo"} {
		if !isManaged(dev) {
			t.Errorf("%s is a RunOS-managed interface and must not be reported as the host's own network", dev)
		}
	}
}

// A real network card must be reported, or the control plane picks a range blind.
func TestOrdinaryInterfacesAreReported(t *testing.T) {
	for _, dev := range []string{"eth0", "eno1", "enp3s0", "ens18", "docker0", "virbr0", "bond0"} {
		if isManaged(dev) {
			t.Errorf("%s is one of the host's own interfaces and must be reported", dev)
		}
	}
}

// Route destinations catch what interface addresses miss: a network reached through a gateway
// with no local address on it, which is how a site-to-site VPN shows up.
func TestParseRouteNetworksKeepsRealDestinations(t *testing.T) {
	out := `[
	  {"dst":"default","dev":"eth0"},
	  {"dst":"192.168.9.0/24","dev":"eth0"},
	  {"dst":"10.50.0.0/16","dev":"tun0"},
	  {"dst":"172.25.0.0/24","dev":"cilium_host"},
	  {"dst":"10.128.252.0/24","dev":"wg0"}
	]`

	got := parseRouteNetworks(out)

	if !slices.Contains(got, "192.168.9.0/24") || !slices.Contains(got, "10.50.0.0/16") {
		t.Errorf("parseRouteNetworks = %v, want the LAN and the VPN route", got)
	}
	// RunOS's own routes are its own business, for the reason in TestManagedInterfacesAreNeverReported.
	for _, unwanted := range []string{"172.25.0.0/24", "10.128.252.0/24"} {
		if slices.Contains(got, unwanted) {
			t.Errorf("parseRouteNetworks = %v, must not report RunOS's own route %s", got, unwanted)
		}
	}
}

// A host route has no prefix in `ip -j route` output. Writing it as a /32 keeps every entry
// parseable by the same code on the far side.
func TestParseRouteNetworksNormalisesAHostRoute(t *testing.T) {
	got := parseRouteNetworks(`[{"dst":"10.5.5.5","dev":"eth0"}]`)
	if !slices.Contains(got, "10.5.5.5/32") {
		t.Errorf("parseRouteNetworks = %v, want the host route as a /32", got)
	}
}

// Unparseable output degrades to "no routes reported" rather than failing the registration. A
// machine that cannot describe its own routes still has to be able to join.
func TestParseRouteNetworksIsSilentOnRubbish(t *testing.T) {
	for _, out := range []string{"", "not json", "{}"} {
		if got := parseRouteNetworks(out); len(got) != 0 {
			t.Errorf("parseRouteNetworks(%q) = %v, want none", out, got)
		}
	}
}

// Collect must be stable: an unstable list makes two registrations of the same machine look like
// different machines to anything that compares them.
func TestCollectIsSortedAndDeduplicated(t *testing.T) {
	got := Collect()
	if !slices.IsSorted(got) {
		t.Errorf("Collect() = %v, want sorted", got)
	}
	for i := 1; i < len(got); i++ {
		if got[i] == got[i-1] {
			t.Errorf("Collect() = %v, contains the duplicate %s", got, got[i])
		}
	}
}
