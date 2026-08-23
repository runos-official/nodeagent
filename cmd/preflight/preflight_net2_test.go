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
)

// fakeNATEnv stands in for the two host facts the NAT-collision check reads, so the printed
// remedy is reachable without a NAT. Pass privateIP "" for a directly routable host, which is
// what the real probe returns when the primary address is not RFC1918.
func fakeNATEnv(t *testing.T, privateIP, publicIP string) {
	t.Helper()
	fakeNATEnvErr(t, privateIP, publicIP, nil)
}

// fakeNATEnvErr is fakeNATEnv with control over the public-IP probe's error, so both halves of
// the inconclusive guard (empty answer, failed probe) can be tested separately.
func fakeNATEnvErr(t *testing.T, privateIP, publicIP string, publicErr error) {
	t.Helper()
	origPrivate, origPublic := nwPrimaryPrivateIPv4Fn, nwExternalIPFn
	nwPrimaryPrivateIPv4Fn = func() string { return privateIP }
	nwExternalIPFn = func() (string, error) { return publicIP, publicErr }
	t.Cleanup(func() { nwPrimaryPrivateIPv4Fn, nwExternalIPFn = origPrivate, origPublic })
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
