package commons

import (
	"reflect"
	"strings"
	"testing"
)

// validPubKey is a canonical 44-char WireGuard base64 public key (43 base64
// chars + one '=' pad). Generated for tests; not a real key.
const validPubKey = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopq="

func TestWgSetPeerArgs_ValidPeer(t *testing.T) {
	args, err := WgSetPeerArgs(validPubKey, "10.0.0.2", "203.0.113.5", nil)
	if err != nil {
		t.Fatalf("expected valid peer to pass, got error: %v", err)
	}
	want := []string{
		"set", "wg0",
		"peer", validPubKey,
		"allowed-ips", "10.0.0.2/32",
		"endpoint", "203.0.113.5:51820",
		"persistent-keepalive", "5",
	}
	if !reflect.DeepEqual(args, want) {
		t.Fatalf("WgSetPeerArgs(, nil) =\n  %v\nwant\n  %v", args, want)
	}

	// Each untrusted field must be its own argument (no shell string), so a
	// metacharacter can never be word-split or interpreted by a shell.
	for _, a := range args {
		if strings.ContainsAny(a, ";|&`$") && a != validPubKey {
			t.Fatalf("unexpected shell metacharacter in arg %q", a)
		}
	}
}

func TestWgSetPeerArgs_RejectsBadInput(t *testing.T) {
	cases := []struct {
		name       string
		pubKey     string
		allowedIP  string
		endpointIP string
	}{
		{
			name:       "pubkey with shell metacharacters",
			pubKey:     "evil; rm -rf / #",
			allowedIP:  "10.0.0.2",
			endpointIP: "203.0.113.5",
		},
		{
			name:       "pubkey wrong length",
			pubKey:     "tooshort=",
			allowedIP:  "10.0.0.2",
			endpointIP: "203.0.113.5",
		},
		{
			name:       "pubkey missing pad",
			pubKey:     "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqr",
			allowedIP:  "10.0.0.2",
			endpointIP: "203.0.113.5",
		},
		{
			name:       "allowed ip is not an IP (injection attempt)",
			pubKey:     validPubKey,
			allowedIP:  "10.0.0.2 endpoint evil",
			endpointIP: "203.0.113.5",
		},
		{
			name:       "endpoint ip is not an IP",
			pubKey:     validPubKey,
			allowedIP:  "10.0.0.2",
			endpointIP: "not-an-ip$(reboot)",
		},
		{
			name:       "empty pubkey",
			pubKey:     "",
			allowedIP:  "10.0.0.2",
			endpointIP: "203.0.113.5",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			args, err := WgSetPeerArgs(tc.pubKey, tc.allowedIP, tc.endpointIP, nil)
			if err == nil {
				t.Fatalf("expected rejection, got args: %v", args)
			}
			if args != nil {
				t.Fatalf("expected nil args on rejection, got: %v", args)
			}
		})
	}
}

func TestValidateWgPubKey(t *testing.T) {
	if err := validateWgPubKey(validPubKey); err != nil {
		t.Fatalf("valid pubkey rejected: %v", err)
	}
	if err := validateWgPubKey("not base64 !@#"); err == nil {
		t.Fatal("expected invalid pubkey to be rejected")
	}
}

// Goal 27, vpn-peer-protocol. An endpointless peer is IDENTITY ONLY: key and allowed IP, no
// endpoint. It exists because a node behind NAT with no inbound path has no endpoint anyone can
// dial, and requiring one meant such a peer was omitted from the peer set entirely, key included.
// The far side had then never heard of it and rejected its opening packet instead of
// authenticating it and learning where it came from.
func TestWgSetPeerArgs_EndpointlessPeerIsIdentityOnly(t *testing.T) {
	args, err := WgSetPeerArgs(validPubKey, "10.0.0.2", "", nil)
	if err != nil {
		t.Fatalf("an endpointless peer must be accepted, got error: %v", err)
	}
	want := []string{
		"set", "wg0",
		"peer", validPubKey,
		"allowed-ips", "10.0.0.2/32",
		"persistent-keepalive", "5",
	}
	if !reflect.DeepEqual(args, want) {
		t.Fatalf("WgSetPeerArgs(, nil) =\n  %v\nwant\n  %v", args, want)
	}

	// No empty endpoint argument may be emitted. `wg set ... endpoint ""` is an error, and an
	// endpoint argument with an empty value would also shift every later argument.
	for _, a := range args {
		if a == "endpoint" {
			t.Fatal("an endpointless peer must not emit an endpoint argument at all")
		}
		if a == "" {
			t.Fatal("no argument may be empty")
		}
	}
}

// The keepalive is what makes the endpointless design work: the NAT'd node dials out and holds
// its mapping open, so the far side can answer. Losing it would leave the peer unreachable a
// minute after it went quiet.
func TestWgSetPeerArgs_EndpointlessPeerKeepsTheKeepalive(t *testing.T) {
	args, _ := WgSetPeerArgs(validPubKey, "10.0.0.2", "", nil)
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "persistent-keepalive 5") {
		t.Fatalf("keepalive missing from %v", args)
	}
}

// Making the endpoint optional must not have made it unvalidated: a SUPPLIED endpoint is still
// checked exactly as before, so nothing untrusted reaches the command line.
func TestWgSetPeerArgs_StillRejectsASuppliedBadEndpoint(t *testing.T) {
	for _, bad := range []string{"not-an-ip$(reboot)", "203.0.113.5 extra", "; rm -rf /"} {
		if args, err := WgSetPeerArgs(validPubKey, "10.0.0.2", bad, nil); err == nil {
			t.Fatalf("endpoint %q must still be rejected, got args: %v", bad, args)
		}
	}
}

// A peer that hosts VMs carries their pool addresses as extra /32 allowed-ips beside its own
// (goal 27, vm-group-range-routing), and each extra is validated like the primary: every field
// of a peer is untrusted input to a root exec.
func TestWgSetPeerArgsCarriesExtraAllowedIPs(t *testing.T) {
	args, err := WgSetPeerArgs(validPubKey, "10.0.0.2", "", []string{"10.77.5.2", "10.77.5.3"})
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "allowed-ips 10.0.0.2/32,10.77.5.2/32,10.77.5.3/32") {
		t.Fatalf("args = %v", args)
	}
	if _, err := WgSetPeerArgs(validPubKey, "10.0.0.2", "", []string{"not-an-ip"}); err == nil {
		t.Fatal("an invalid extra allowed-ip must be rejected")
	}
	if _, err := WgSetPeerArgs(validPubKey, "10.0.0.2", "", []string{"10.0.0.3; rm -rf /"}); err == nil {
		t.Fatal("an injection-shaped extra must be rejected")
	}
}
