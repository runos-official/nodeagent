package preflight

import (
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// A mirror root that 301s to an unreachable host must still count as
// reachable: the response itself proves the mirror is up, and following the
// redirect would probe a host apt never contacts (the security.ubuntu.com ->
// www.ubuntu.com case that false-blocked a real install).
func TestNwProbeMirrorDoesNotFollowRedirect(t *testing.T) {
	// A listener we immediately close: guaranteed connection-refused target.
	dead, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	deadAddr := dead.Addr().String()
	dead.Close()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, fmt.Sprintf("http://%s/", deadAddr), http.StatusMovedPermanently)
	}))
	defer srv.Close()

	reason, blocked := nwProbeMirrorUnreachable(srv.URL)
	if blocked {
		t.Fatalf("301 response must prove reachability without following the redirect; got blocked with reason %q", reason)
	}
}

// Mirror roots commonly reject a bare request; any HTTP status is proof of
// reachability.
func TestNwProbeMirrorErrorStatusIsReachable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv.Close()

	if reason, blocked := nwProbeMirrorUnreachable(srv.URL); blocked {
		t.Fatalf("403 must count as reachable; got blocked with reason %q", reason)
	}
}

// A genuinely dead port must still be flagged.
func TestNwProbeMirrorConnectionRefused(t *testing.T) {
	dead, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	deadURL := "http://" + dead.Addr().String()
	dead.Close()

	reason, blocked := nwProbeMirrorUnreachable(deadURL)
	if !blocked {
		t.Fatal("closed port must be flagged unreachable")
	}
	if reason != "connection refused" {
		t.Fatalf("expected reason %q, got %q", "connection refused", reason)
	}
}

// Fixture addresses. CONTRIBUTING.md forbids real internal hostnames and IPs in the repo, so
// these are a documentation-range public address (RFC 5737 TEST-NET-3) and an RFC1918 address
// that belongs to no lab box.
const (
	fixturePrivateIP = "10.0.0.5"
	fixturePublicIP  = "203.0.113.7"
	// A second documentation-range public address (RFC 5737 TEST-NET-2), for the shapes where a
	// local interface HOLDS the externally observed address.
	fixtureHeldPublicIP = "198.51.100.7"
	// The interface the fixture private address sits on. A real NIC name, not a bridge.
	fixturePrivateDev = "enp7s0"
)

// fakeNATEnv stands in for the two host facts the NAT-collision check reads, so the printed
// remedy is reachable without a NAT. Pass privateIP "" for a directly routable host, which is
// what the real probe returns when the primary address is not RFC1918.
func fakeNATEnv(t *testing.T, privateIP, publicIP string) {
	t.Helper()
	fakeNATEnvErr(t, privateIP, publicIP, nil)
}

// fakeNATEnvErr is fakeNATEnv with control over the public-IP probe's error, so both halves of
// the inconclusive guard (empty answer, failed probe) can be tested separately. It pins the
// third seam to "no local interface holds the public address", which is the NAT shape; use
// fakeMultiHomedEnv for the dual-homed shape.
func fakeNATEnvErr(t *testing.T, privateIP, publicIP string, publicErr error) {
	t.Helper()
	fakeEndpointEnv(t, privateIP, publicIP, publicErr, "")
}

// fakeMultiHomedEnv is the dual-homed shape measured on RunOS dev 2026-08-24: the host holds its
// own public address on dev, and has a private address as well.
func fakeMultiHomedEnv(t *testing.T, privateIP, publicIP, dev string) {
	t.Helper()
	fakeEndpointEnv(t, privateIP, publicIP, nil, dev)
}

// fakeEndpointEnv stands in for the host facts the endpoint checks read. dev is the local
// interface that holds publicIP, or "" when no interface does. The private-network candidate set
// is the single obvious one, privateIP on fixturePrivateDev, whenever privateIP is RFC1918; use
// fakeEndpointEnvCandidates for the shapes where the candidate set is the thing under test.
//
// It drops the memoized facts, because the checks now read one answer per preflight run and a
// leftover answer from the previous test would decide this one.
func fakeEndpointEnv(t *testing.T, privateIP, publicIP string, publicErr error, dev string) {
	t.Helper()
	var candidates []nwIfaceAddr
	if ip := net.ParseIP(privateIP); ip != nil && ip.IsPrivate() {
		candidates = []nwIfaceAddr{{dev: fixturePrivateDev, addr: privateIP}}
	}
	fakeEndpointEnvCandidates(t, privateIP, publicIP, publicErr, dev, candidates)
}

// fakeEndpointEnvCandidates is fakeEndpointEnv with the private-network candidate set supplied, so
// the bridges-only and more-than-one-candidate shapes are reachable from a test.
func fakeEndpointEnvCandidates(t *testing.T, privateIP, publicIP string, publicErr error, dev string, candidates []nwIfaceAddr) {
	t.Helper()
	origPrivate, origPublic, origIface := nwPrimaryPrivateIPv4Fn, nwExternalIPFn, nwIfaceHoldingIPv4Fn
	origCandidates := nwPrivateIPv4CandidatesFn
	nwPrimaryPrivateIPv4Fn = func() string { return privateIP }
	nwExternalIPFn = func() (string, error) { return publicIP, publicErr }
	nwIfaceHoldingIPv4Fn = func(addr string) string {
		if dev != "" && addr == publicIP {
			return dev
		}
		return ""
	}
	nwPrivateIPv4CandidatesFn = func() []nwIfaceAddr { return candidates }
	nwResetEndpointFacts()
	t.Cleanup(func() {
		nwPrimaryPrivateIPv4Fn, nwExternalIPFn, nwIfaceHoldingIPv4Fn = origPrivate, origPublic, origIface
		nwPrivateIPv4CandidatesFn = origCandidates
		nwResetEndpointFacts()
	})
}

// FCR 148 F8. The remedy was correct but its timing was not: preflight runs BEFORE `runos
// register` (templates/install.sh runs preflight, then register), so the node has no nid yet and
// the control plane refuses the join with a 409. The operator read two commands and could only
// run the first one. The warning must name the order and say when each command becomes runnable.
func TestNatCollisionWarningStatesWhenEachCommandBecomesRunnable(t *testing.T) {
	fakeNATEnv(t, fixturePrivateIP, fixturePublicIP)

	err := checkNATEndpointCollision()
	if err == nil {
		t.Fatal("a private primary address with a different public IP must warn")
	}
	msg := err.Error()

	// The join is not runnable when this prints, and the operator must be told so.
	if !strings.Contains(msg, "has registered") {
		t.Errorf("want the warning to say the join waits until the node has registered, got:\n%s", msg)
	}
	if !strings.Contains(msg, "409") {
		t.Errorf("want the warning to name the 409 refusal an early join gets, got:\n%s", msg)
	}
	// The operator needs the nid, which only exists after registration.
	if !strings.Contains(msg, "runos nodes list --cid <cid>") {
		t.Errorf("want the warning to show how to read the nid after registration, got:\n%s", msg)
	}
	// Create is runnable now; join is not. The order must be explicit, not implied by line order.
	create := strings.Index(msg, "runos clusters networks create")
	join := strings.Index(msg, "runos clusters networks join")
	if create < 0 || join < 0 || create > join {
		t.Errorf("want create printed before join, got create=%d join=%d in:\n%s", create, join, msg)
	}
	if !strings.Contains(msg, "in this order") {
		t.Errorf("want the warning to state the ordering plainly, got:\n%s", msg)
	}
	// The node's own private address still fills in, so the join line is copy-pasteable.
	if !strings.Contains(msg, "--address "+fixturePrivateIP) {
		t.Errorf("want the node's private address filled into the join command, got:\n%s", msg)
	}
	// The ingress command is refused before registration for the same reason as the join, so it
	// carries a timing annotation too rather than sitting untagged under the numbered block.
	ingress := strings.Index(msg, "runos nodes ingress <nid> --cid <cid> --no-public-ingress")
	if ingress < 0 || ingress < join {
		t.Errorf("want the ingress command printed after the join, got ingress=%d join=%d in:\n%s", ingress, join, msg)
	}
	if !strings.Contains(msg, "AFTER registration as well") {
		t.Errorf("want the ingress command to say it is also refused before registration, got:\n%s", msg)
	}
}

// FCR 148 F8, second pass. The first fix printed an ordered remedy that was still a NO-OP when
// followed exactly: it joined only THIS node. Endpoint resolution reads a self-join over
// network_memberships (nodeward/persist/network/shared.go:53-61), so a membership held by one
// node returns zero rows for BOTH peers, rule 2 in nodeward/persist/node/endpoint.go never
// fires, and the tunnels keep colliding while every API call returns success. Confirmed on
// hardware 2026-08-23: WireGuard peered over the LAN address only after BOTH lab nodes held a
// membership in one network.
func TestNatCollisionWarningRequiresEveryNodeBehindTheNatToJoin(t *testing.T) {
	fakeNATEnv(t, fixturePrivateIP, fixturePublicIP)

	err := checkNATEndpointCollision()
	if err == nil {
		t.Fatal("a private primary address with a different public IP must warn")
	}
	msg := err.Error()

	if !strings.Contains(msg, "Join EVERY node behind this NAT to the SAME network") {
		t.Errorf("want the warning to demand a membership for every node behind the NAT, got:\n%s", msg)
	}
	if !strings.Contains(msg, "A membership on ONE node alone changes NOTHING") {
		t.Errorf("want the warning to say a single membership resolves nothing, got:\n%s", msg)
	}
	if !strings.Contains(msg, "BOTH peers hold a membership in one network") {
		t.Errorf("want the warning to name the both-peers rule endpoint resolution applies, got:\n%s", msg)
	}
	// The operator needs a way to CHECK the member set, not just an instruction to build it.
	// --json is not optional here: formatCellValue in the CLI (cli/internal/output/formatter.go)
	// collapses an array of objects to "[N entries]", so the default table hides the member nids
	// and the operator cannot tell which nodes are in the network.
	if !strings.Contains(msg, "runos clusters networks list --cid <cid> --json") {
		t.Errorf("want the warning to show how to read the member set back, got:\n%s", msg)
	}
	// The retired claim. "Declaring the network is the whole remedy" told the operator to stop
	// after one join, which is exactly the no-op. It must not come back.
	if strings.Contains(msg, "whole remedy") {
		t.Errorf("the single-join 'whole remedy' claim is a no-op instruction and must stay out, got:\n%s", msg)
	}
}

// The printed CLI commands cannot run on the machine that prints them: the `runos` on a node is
// the node agent, which registers no `clusters` or `nodes` subcommand (cmd/root.go). An operator
// standing on the node reads "the runos CLI" as the binary that just printed the warning.
func TestNatCollisionWarningSaysWhereEachCommandRuns(t *testing.T) {
	fakeNATEnv(t, fixturePrivateIP, fixturePublicIP)

	err := checkNATEndpointCollision()
	if err == nil {
		t.Fatal("a private primary address with a different public IP must warn")
	}
	msg := err.Error()

	if !strings.Contains(msg, `unknown command "clusters" for "runos"`) {
		t.Errorf("want the warning to show what this node answers to a CLI command, got:\n%s", msg)
	}
	if !strings.Contains(msg, "workstation that has the RunOS CLI installed") {
		t.Errorf("want the warning to name where the CLI commands run, got:\n%s", msg)
	}
	// The node-local way to read the nid. `runos nodes list` prints EVERY node in the cluster,
	// and this warning only fires where two or more nodes share one NAT, so a multi-row list is
	// the normal case. Picking the wrong row writes a wrong membership.
	if !strings.Contains(msg, "sudo runos status") {
		t.Errorf("want the warning to name the node-local command that prints this node's nid, got:\n%s", msg)
	}
	if !strings.Contains(msg, "prints EVERY node, not just this one") {
		t.Errorf("want the warning to say nodes list is not scoped to this node, got:\n%s", msg)
	}
	// The nid exists before register: it is minted with the registration token
	// (nodeward/persist/rtoken/add.go:11) and carried into the node row at register
	// (nodeward/persist/node/node.go:174). Register creates the ROW, not the nid.
	if !strings.Contains(msg, "the nid is reserved when you generate the join command") {
		t.Errorf("want the warning to state the nid lifecycle correctly, got:\n%s", msg)
	}
}

// The installer runs preflight, register and the Kubernetes install back to back with no pause
// (templates/install.sh). The FCR's failing case is a node that needs the overlay DURING the
// Kubernetes join, so the operator who follows the ordering advice normally arrives after the
// join has already used the colliding endpoint. The warning must give that operator a recovery
// path, and it must not promise that nothing restarts.
func TestNatCollisionWarningSaysTheInstallerDoesNotWait(t *testing.T) {
	fakeNATEnv(t, fixturePrivateIP, fixturePublicIP)

	err := checkNATEndpointCollision()
	if err == nil {
		t.Fatal("a private primary address with a different public IP must warn")
	}
	msg := err.Error()

	if !strings.Contains(msg, "THE INSTALLER DOES NOT WAIT") {
		t.Errorf("want the warning to say the install does not pause for these steps, got:\n%s", msg)
	}
	// The recovery path must be a command that RUNS in the state where it is printed. Re-running
	// the installer script does not: the registration token in it is single-use (nodeward
	// persist/rtoken/special.go:15 stamps spent_at, find_by.go:11 then skips it) and lasts about
	// 20 minutes (add.go:22), so register exits non-zero and templates/install.sh:231 stops the
	// run. `sudo runos install` is a node-agent subcommand (cmd/install/root.go) and is what the
	// agent's own failure hints already tell operators to re-run.
	if !strings.Contains(msg, "sudo runos install") {
		t.Errorf("want the warning to give a recovery command that runs on this node, got:\n%s", msg)
	}
	if strings.Contains(msg, "re-run the installer on this node") {
		t.Errorf("re-running the installer fails on a registered node (spent token) and must not be the advice, got:\n%s", msg)
	}
	if !strings.Contains(msg, "Do NOT re-run the installer script") {
		t.Errorf("want the warning to say the installer script cannot be re-run here, got:\n%s", msg)
	}
	// The trap that undoes the whole remedy: a fresh join command mints a NEW nid
	// (nodeward/persist/rtoken/add.go:11), so the step-3 membership is left on the old nid and the
	// collision returns while every command still reports success.
	if !strings.Contains(msg, "mints a NEW nid") {
		t.Errorf("want the warning to say a new join command orphans the step-3 membership, got:\n%s", msg)
	}
	// "you restart nothing" over-promised. The claim is true of the peer list only.
	if !strings.Contains(msg, "no agent restart and no manual VPN sync is needed") {
		t.Errorf("want the no-restart claim scoped to the peer push, got:\n%s", msg)
	}
	if strings.Contains(msg, "you restart nothing") {
		t.Errorf("the unscoped 'you restart nothing' claim is false for an install that already joined, got:\n%s", msg)
	}
}

// A directly routable node stays silent, because a warning that fires on a healthy machine
// trains operators to ignore every warning. This is the shape the REAL probe produces:
// nwPrimaryPrivateIPv4 returns a value only inside `if ip4.IsPrivate()`, so a host with a public
// primary address yields "" and the check returns at the ifaceIP == "" guard.
func TestNatCollisionStaysSilentWhenThereIsNoPrivatePrimaryAddress(t *testing.T) {
	fakeNATEnv(t, "", fixturePublicIP)

	if err := checkNATEndpointCollision(); err != nil {
		t.Fatalf("no RFC1918 primary address is not NAT; want no warning, got:\n%s", err)
	}
}

// The equal-IP guard, pinned separately. The real probe cannot feed a public address into
// ifaceIP today, so this covers the guard itself rather than a reachable production shape.
func TestNatCollisionStaysSilentWhenThePublicIPIsBoundToTheInterface(t *testing.T) {
	fakeNATEnv(t, fixturePublicIP, fixturePublicIP)

	if err := checkNATEndpointCollision(); err != nil {
		t.Fatalf("a public IP bound to the interface is not NAT; want no warning, got:\n%s", err)
	}
}

// A failed public-IP probe is inconclusive, and inconclusive must not warn.
func TestNatCollisionStaysSilentWhenThePublicIPProbeFails(t *testing.T) {
	fakeNATEnvErr(t, fixturePrivateIP, "", fmt.Errorf("no egress to the IP echo services"))

	if err := checkNATEndpointCollision(); err != nil {
		t.Fatalf("a failed public-IP probe is inconclusive; want no warning, got:\n%s", err)
	}
}

// The other half of the same guard: the probe succeeds and answers nothing.
func TestNatCollisionStaysSilentWhenThePublicIPIsEmpty(t *testing.T) {
	fakeNATEnv(t, fixturePrivateIP, "   ")

	if err := checkNATEndpointCollision(); err != nil {
		t.Fatalf("a blank public IP is inconclusive; want no warning, got:\n%s", err)
	}
}

// nwAnyLocalIPv4 returns an IPv4 address this machine really holds, with the interface that holds
// it. CONTRIBUTING.md forbids real addresses in the repo, so the test reads one at runtime instead
// of hardcoding it. Returns ("", "") when the machine has no non-loopback IPv4, which is the one
// case the caller must skip rather than fail.
func nwAnyLocalIPv4(t *testing.T) (dev string, addr string) {
	t.Helper()
	ifaces, err := net.Interfaces()
	if err != nil {
		return "", ""
	}
	for _, iface := range ifaces {
		if iface.Flags&net.FlagLoopback != 0 || iface.Flags&net.FlagUp == 0 {
			continue
		}
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			ipnet, ok := a.(*net.IPNet)
			if !ok {
				continue
			}
			if ip4 := ipnet.IP.To4(); ip4 != nil {
				return iface.Name, ip4.String()
			}
		}
	}
	return "", ""
}

// Measured on RunOS dev 2026-08-24. Three Hetzner Cloud servers joined one cluster. Each holds its
// OWN routable address on eth0 and a private address on enp7s0 for a Hetzner private network. Every
// one of them was greeted with "this node is behind NAT (private ... vs public ...)". BOTH claims
// in that warning are false for such a host: it is not behind NAT, it holds the public address
// itself, and three DISTINCT public addresses cannot collide as WireGuard endpoints. The operator
// reads a security-flavoured alarm about a cluster that is working.
//
// The cause is that the check compares the primary PRIVATE address to the externally observed one
// and calls any difference NAT. It never asks whether this host holds the external address on one
// of its own interfaces.
//
// This test feeds an address the machine REALLY holds, read through net.Interfaces() at runtime, as
// the externally observed address. That is the dual-homed shape, and it drives the real
// interface-enumeration path rather than a stub of it.
func TestNatCollisionStaysSilentWhenThisHostHoldsTheExternalAddressItself(t *testing.T) {
	dev, addr := nwAnyLocalIPv4(t)
	if addr == "" {
		t.Skip("this machine has no non-loopback IPv4; there is no dual-homed shape to test")
	}
	if addr == fixturePrivateIP {
		t.Skipf("this machine holds the private fixture address %s; the two roles would collide", fixturePrivateIP)
	}
	// Only the two host-fact seams are faked. nwIfaceHoldingIPv4Fn stays REAL, which is the whole
	// point of this test: it proves the shipped interface-enumeration path answers correctly.
	origPrivate, origPublic := nwPrimaryPrivateIPv4Fn, nwExternalIPFn
	nwResetEndpointFacts()
	t.Cleanup(nwResetEndpointFacts)
	nwPrimaryPrivateIPv4Fn = func() string { return fixturePrivateIP }
	nwExternalIPFn = func() (string, error) { return addr, nil }
	t.Cleanup(func() { nwPrimaryPrivateIPv4Fn, nwExternalIPFn = origPrivate, origPublic })

	if err := checkNATEndpointCollision(); err != nil {
		t.Fatalf("this host holds %s on %s, so it is multi-homed and NOT behind NAT; want no collision warning, got:\n%s",
			addr, dev, err)
	}

	// The same real path must ALSO put the node in the multi-homed branch, naming the interface it
	// really found. A silent nat-collision check with a silent advisory beside it would leave the
	// operator with nothing at all.
	err := checkMultiHomedEndpoint()
	if err == nil {
		t.Fatal("a host that holds its own public address and has a private one must get the advisory")
	}
	if !strings.Contains(err.Error(), dev) {
		t.Errorf("want the advisory to name the interface %q that really holds %s, got:\n%s", dev, addr, err)
	}
}

// The Hetzner Cloud shape from the field, driven hermetically through the seams so the exact text
// an operator reads is pinned. Measured on RunOS dev 2026-08-24: a routable address on eth0, a
// private-network address on enp7s0, and a "behind NAT" warning that was false twice over.
func TestMultiHomedHostGetsNoCollisionClaimAndAShortAdvisory(t *testing.T) {
	fakeMultiHomedEnv(t, fixturePrivateIP, fixturePublicIP, "eth0")

	if err := checkNATEndpointCollision(); err != nil {
		t.Fatalf("the host holds %s itself, so it is not behind NAT; want no collision warning, got:\n%s",
			fixturePublicIP, err)
	}

	err := checkMultiHomedEndpoint()
	if err == nil {
		t.Fatal("a multi-homed host with a private address must still get the short advisory")
	}
	msg := err.Error()

	// The three facts. The public address, the interface that holds it, and the private address.
	for _, want := range []string{fixturePublicIP, "eth0", fixturePrivateIP, "NOT behind NAT"} {
		if !strings.Contains(msg, want) {
			t.Errorf("want the advisory to state %q, got:\n%s", want, msg)
		}
	}
	// The gain, stated as a gain. This case is an optimisation, not a repair.
	if !strings.Contains(msg, "optimisation, not a repair") {
		t.Errorf("want the advisory to frame the private network as an optimisation, got:\n%s", msg)
	}
	if !strings.Contains(msg, "dial each other at their private addresses") {
		t.Errorf("want the advisory to say what declaring the network buys, got:\n%s", msg)
	}
	// The two commands, with this node's private address filled in so the join is copy-pasteable.
	if !strings.Contains(msg, "runos clusters networks create --cid <cid> --name <network-name> --json") {
		t.Errorf("want the create command, got:\n%s", msg)
	}
	if !strings.Contains(msg, "--address "+fixturePrivateIP) {
		t.Errorf("want the node's private address filled into the join command, got:\n%s", msg)
	}
	// Every false or alarming claim from the NAT text must stay OUT of this branch.
	for _, banned := range []string{"behind NAT (private", "collide", "only one stays up", "hairpin", "sudo runos install", "THE INSTALLER DOES NOT WAIT"} {
		if strings.Contains(msg, banned) {
			t.Errorf("the multi-homed advisory must not claim %q, got:\n%s", banned, msg)
		}
	}
	// Short. The NAT text is a 5-step repair runbook; this one is an advisory and must read as one.
	if lines := strings.Count(msg, "\n") + 1; lines > 10 {
		t.Errorf("the advisory must stay short, got %d lines:\n%s", lines, msg)
	}
}

// The NAT branch is unchanged, word for word. It was hardened over several rounds (FCR 148 F8,
// hardware 2026-08-18, 2026-08-19 and 2026-08-23) and the 2026-08-24 split must not have touched
// it. A genuinely NAT'd host holds no interface with the public address.
func TestGenuinelyNattedHostStillGetsTheFullCollisionWarning(t *testing.T) {
	fakeNATEnv(t, fixturePrivateIP, fixturePublicIP)

	if err := checkMultiHomedEndpoint(); err != nil {
		t.Fatalf("no interface holds the public address, so this is NAT, not multi-homing; want no advisory, got:\n%s", err)
	}

	err := checkNATEndpointCollision()
	if err == nil {
		t.Fatal("a NAT'd host must still get the endpoint-collision warning")
	}
	msg := err.Error()

	if !strings.Contains(msg, fmt.Sprintf("this node is behind NAT (private %s vs public %s)", fixturePrivateIP, fixturePublicIP)) {
		t.Errorf("want the NAT header unchanged, got:\n%s", msg)
	}
	if !strings.Contains(msg, "their tunnels collide and only one stays up, and same-NAT peers also need NAT hairpin support. A single node behind NAT is fine.") {
		t.Errorf("want the collision paragraph unchanged, got:\n%s", msg)
	}
	if !strings.Contains(msg, "This is a networking heads-up, not a RunOS limitation.") {
		t.Errorf("want the closing line unchanged, got:\n%s", msg)
	}
}

// Both checks read the same facts, so both must stay silent on every inconclusive answer. Pinning
// them together stops the split from starting to warn where preflight used to say nothing.
func TestBothEndpointChecksStaySilentOnInconclusiveFacts(t *testing.T) {
	cases := []struct {
		name  string
		setup func(t *testing.T)
	}{
		{"no RFC1918 primary address", func(t *testing.T) { fakeNATEnv(t, "", fixturePublicIP) }},
		{"public IP bound to the primary interface", func(t *testing.T) { fakeNATEnv(t, fixturePublicIP, fixturePublicIP) }},
		{"public IP probe failed", func(t *testing.T) {
			fakeNATEnvErr(t, fixturePrivateIP, "", fmt.Errorf("no egress to the IP echo services"))
		}},
		{"public IP probe answered blank", func(t *testing.T) { fakeNATEnv(t, fixturePrivateIP, "   ") }},
		// The same inconclusive answers with an interface that WOULD match: the guard must win
		// before the interface question is ever asked.
		{"no RFC1918 primary address, interface present", func(t *testing.T) {
			fakeMultiHomedEnv(t, "", fixturePublicIP, "eth0")
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tc.setup(t)
			if err := checkNATEndpointCollision(); err != nil {
				t.Errorf("inconclusive facts must not warn; nat-collision said:\n%s", err)
			}
			if err := checkMultiHomedEndpoint(); err != nil {
				t.Errorf("inconclusive facts must not warn; multi-homed-endpoint said:\n%s", err)
			}
		})
	}
}

// Both branches are registered, both are advisory, and neither blocks an install. A wrong severity
// here would fail an install over a working cluster.
func TestEndpointChecksAreRegisteredAsAdvisory(t *testing.T) {
	want := map[string]bool{"nat-collision": false, "multi-homed-endpoint": false}
	for _, c := range preflightChecks() {
		if _, ok := want[c.name]; !ok {
			continue
		}
		want[c.name] = true
		if c.sev != sevWarn {
			t.Errorf("check %q must be advisory, got severity %v", c.name, c.sev)
		}
		if c.fatal {
			t.Errorf("check %q must not be a fatal prerequisite", c.name)
		}
		if !c.net {
			t.Errorf("check %q reads the externally observed address, so it belongs to the network phase", c.name)
		}
	}
	for name, found := range want {
		if !found {
			t.Errorf("no %q check is registered", name)
		}
	}
}

// nwIfaceHoldingIPv4 itself, on addresses no machine holds. RFC 5737 TEST-NET-3 is reserved for
// documentation and is never assigned, so this answer is stable on any builder.
func TestNwIfaceHoldingIPv4RejectsAddressesThisHostDoesNotHold(t *testing.T) {
	for _, addr := range []string{fixturePublicIP, "", "not-an-ip", "::1", "0.0.0.0"} {
		if dev := nwIfaceHoldingIPv4(addr); dev != "" {
			t.Errorf("nwIfaceHoldingIPv4(%q) = %q, want \"\" (no interface holds it)", addr, dev)
		}
	}
}

// F1, adversarial review of the 2026-08-24 split. The split gave each branch its OWN call to
// nwEndpointFacts, so one preflight run probed the public IP TWICE and the two answers were never
// required to agree. Driven with two different answers, BOTH warnings printed, one screen apart,
// saying opposite things: nat-collision called the node NAT'd, multi-homed-endpoint said it was
// NOT behind NAT. Realistic triggers: dual-WAN or failover egress, a rotating CGNAT pool, or a
// transient failure of the first provider sending the second call to another provider. The first
// provider is the DUAL-STACK ipify endpoint, so an IPv6-preferring host falls through to provider
// two routinely.
//
// The run goes through the phase runner, because "once per preflight run" is the claim under test.
func TestOnePreflightRunProbesThePublicIPOnceAndTheTwoBranchesCannotDisagree(t *testing.T) {
	probes := 0
	origPrivate, origPublic, origIface := nwPrimaryPrivateIPv4Fn, nwExternalIPFn, nwIfaceHoldingIPv4Fn
	nwPrimaryPrivateIPv4Fn = func() string { return fixturePrivateIP }
	nwExternalIPFn = func() (string, error) {
		probes++
		if probes == 1 {
			return fixturePublicIP, nil // no local interface holds it -> the NAT shape
		}
		return fixtureHeldPublicIP, nil // pub0 holds it -> the multi-homed shape
	}
	nwIfaceHoldingIPv4Fn = func(addr string) string {
		if addr == fixtureHeldPublicIP {
			return "pub0"
		}
		return ""
	}
	t.Cleanup(func() {
		nwPrimaryPrivateIPv4Fn, nwExternalIPFn, nwIfaceHoldingIPv4Fn = origPrivate, origPublic, origIface
		nwResetEndpointFacts()
	})

	var natErr, multiErr error
	_ = runChecks([]check{
		{name: "nat-collision", fn: func() error { natErr = checkNATEndpointCollision(); return natErr }, sev: sevWarn, net: true},
		{name: "multi-homed-endpoint", fn: func() error { multiErr = checkMultiHomedEndpoint(); return multiErr }, sev: sevWarn, net: true},
	})

	if probes != 1 {
		t.Errorf("one preflight run must probe the public IP ONCE, got %d probes", probes)
	}
	if natErr != nil && multiErr != nil {
		t.Errorf("the two branches contradicted each other in one run.\nnat-collision:\n%s\n\nmulti-homed-endpoint:\n%s", natErr, multiErr)
	}
}

// F4, adversarial review of the 2026-08-24 split. The advisory's runbook was a SILENT NO-OP when
// followed literally: it printed only THIS node's join. RunOS hands out the private address only
// when BOTH peers hold a membership in one network, which the NAT branch says loudly, so an
// operator who ran exactly the two printed commands got no change and both commands reported
// success. The advisory also never said where <nid> or <networkId> come from, while the NAT branch
// gives each its own step.
func TestMultiHomedAdvisoryCannotBeFollowedIntoANoOp(t *testing.T) {
	fakeMultiHomedEnv(t, fixturePrivateIP, fixturePublicIP, "eth0")

	err := checkMultiHomedEndpoint()
	if err == nil {
		t.Fatal("a multi-homed host with a private address must get the advisory")
	}
	msg := err.Error()

	// One membership is not a remedy, and the text must say so in its own words.
	for _, want := range []string{"EVERY node", "SAME network", "BOTH peers hold a membership"} {
		if !strings.Contains(msg, want) {
			t.Errorf("the advisory must say that one membership alone changes nothing; missing %q, got:\n%s", want, msg)
		}
	}
	// Both placeholders the printed commands carry must have a stated source.
	if !strings.Contains(msg, "The create command prints <networkId>") {
		t.Errorf("the advisory must say where <networkId> comes from, got:\n%s", msg)
	}
	if !strings.Contains(msg, "'sudo runos status'") {
		t.Errorf("the advisory must say where each node's <nid> comes from, got:\n%s", msg)
	}
}

// F3, adversarial review of the 2026-08-24 split, proved on real Linux. nwPrimaryPrivateIPv4
// returns the FIRST RFC1918 address in net.Interfaces() order, which is not "this node's address
// on the private network". In a container holding eth0=172.17.0.2 (a docker bridge address),
// pub0=198.51.100.7, virbr0=192.168.122.1 and privnic=10.50.50.4, the advisory printed
// `--address 172.17.0.2`, the bridge.
//
// The advisory now prints a literal address only when exactly ONE candidate interface holds one.
// With more than one it names the interfaces and lets the operator supply the address, because a
// guess printed as a literal is what reached the operator.
func TestMultiHomedAdvisoryPrintsNoAddressItIsNotConfidentAbout(t *testing.T) {
	bridgeAddr, realAddr := "172.17.0.2", "10.50.50.4"
	fakeEndpointEnvCandidates(t, bridgeAddr, fixtureHeldPublicIP, nil, "pub0", []nwIfaceAddr{
		{dev: "eth0", addr: bridgeAddr},
		{dev: "privnic", addr: realAddr},
	})

	err := checkMultiHomedEndpoint()
	if err == nil {
		t.Fatal("a multi-homed host with private addresses must still get the advisory")
	}
	msg := err.Error()

	for _, banned := range []string{"--address " + bridgeAddr, "--address " + realAddr} {
		if strings.Contains(msg, banned) {
			t.Errorf("two candidates is a guess, so the advisory must print no literal address; found %q in:\n%s", banned, msg)
		}
	}
	if !strings.Contains(msg, "--address <address>") {
		t.Errorf("want the join command to carry an <address> placeholder, got:\n%s", msg)
	}
	for _, want := range []string{"eth0", "privnic", "ip -4 addr show <interface>"} {
		if !strings.Contains(msg, want) {
			t.Errorf("want the advisory to name the interfaces and how to read the address; missing %q, got:\n%s", want, msg)
		}
	}
	if lines := strings.Count(msg, "\n") + 1; lines > 10 {
		t.Errorf("the advisory must stay short in this branch too, got %d lines:\n%s", lines, msg)
	}
}

// The other half of F3. A node with a public NIC plus a docker0 or a libvirt virbr0 and NO real
// private network was given this advisory at all, telling it to declare a network for a bridge
// address. There is nothing to declare on such a host, so both branches must stay silent.
func TestMultiHomedAdvisoryStaysSilentWhenEveryPrivateAddressIsABridge(t *testing.T) {
	fakeEndpointEnvCandidates(t, "172.17.0.1", fixtureHeldPublicIP, nil, "pub0", nil)

	if err := checkMultiHomedEndpoint(); err != nil {
		t.Errorf("docker0 and virbr0 are not a private network to declare; want silence, got:\n%s", err)
	}
	if err := checkNATEndpointCollision(); err != nil {
		t.Errorf("this host holds its own public address, so it is not behind NAT; want silence, got:\n%s", err)
	}
}

// The name test behind the candidate filter. Over-excluding costs silence on an advisory,
// under-excluding costs a printed address that is wrong, so the plain "br0" case matters: it is
// commonly the operator's own bridged NIC on a KVM host, which is the private path the advisory is
// about.
func TestNwIsBridgeOrVirtualInterface(t *testing.T) {
	for _, dev := range []string{"docker0", "br-1a2b3c", "virbr0", "veth1234", "vboxnet0", "vmnet1", "cni0", "kube-ipvs0", "wg0", "cilium_host", "dummy0"} {
		if !nwIsBridgeOrVirtualInterface(dev) {
			t.Errorf("%q is a bridge or virtual link and must not be a private-network candidate", dev)
		}
	}
	for _, dev := range []string{"eth0", "enp7s0", "ens18", "br0", "bond0", "eno1", "privnic"} {
		if nwIsBridgeOrVirtualInterface(dev) {
			t.Errorf("%q can carry a real private network and must stay a candidate", dev)
		}
	}
}
