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
	args, err := WgSetPeerArgs(validPubKey, "10.0.0.2", "203.0.113.5")
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
		t.Fatalf("WgSetPeerArgs() =\n  %v\nwant\n  %v", args, want)
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
			args, err := WgSetPeerArgs(tc.pubKey, tc.allowedIP, tc.endpointIP)
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
