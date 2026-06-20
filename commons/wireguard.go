package commons

import (
	"fmt"
	"net"
	"os/exec"
	"regexp"

	"github.com/runos-official/nodeagent/roslog"
)

// wgKeepalive is the persistent-keepalive interval (seconds) applied to every
// WireGuard peer, matching the previous inline value.
const wgKeepalive = "5"

// wgListenPort is the WireGuard UDP port every peer endpoint listens on.
const wgListenPort = "51820"

// wgPubKeyRe matches a canonical WireGuard public key: standard base64 of a
// 32-byte key, which is always 43 base64 chars followed by a single '=' pad
// (44 chars total).
var wgPubKeyRe = regexp.MustCompile(`^[A-Za-z0-9+/]{43}=$`)

// validateWgPubKey returns an error if pubKey is not a canonical 44-char
// WireGuard base64 public key.
func validateWgPubKey(pubKey string) error {
	if !wgPubKeyRe.MatchString(pubKey) {
		return fmt.Errorf("invalid WireGuard public key %q", pubKey)
	}
	return nil
}

// validateIP returns an error if ip does not parse as an IP address.
func validateIP(ip string) error {
	if net.ParseIP(ip) == nil {
		return fmt.Errorf("invalid IP address %q", ip)
	}
	return nil
}

// WgSetPeerArgs validates the untrusted peer fields and returns the argument
// vector for `wg set wg0 peer ...`. Each value becomes a SEPARATE exec argument
// (no shell, no string interpolation), so peer fields can never be interpreted
// as shell metacharacters. pubKey must be a canonical WireGuard base64 key and
// both allowedIP and endpointIP must parse as IP addresses; anything else is
// rejected with an error.
func WgSetPeerArgs(pubKey, allowedIP, endpointIP string) ([]string, error) {
	if err := validateWgPubKey(pubKey); err != nil {
		return nil, err
	}
	if err := validateIP(allowedIP); err != nil {
		return nil, fmt.Errorf("invalid allowed-ips: %w", err)
	}
	if err := validateIP(endpointIP); err != nil {
		return nil, fmt.Errorf("invalid endpoint: %w", err)
	}

	return []string{
		"set", "wg0",
		"peer", pubKey,
		"allowed-ips", allowedIP + "/32",
		"endpoint", endpointIP + ":" + wgListenPort,
		"persistent-keepalive", wgKeepalive,
	}, nil
}

// SetWgPeer validates the peer fields and configures the WireGuard peer by
// invoking `wg` directly (no shell), so untrusted peer fields cannot be used
// for command injection. It returns an error if validation or the command
// fails.
func SetWgPeer(pubKey, allowedIP, endpointIP string) error {
	args, err := WgSetPeerArgs(pubKey, allowedIP, endpointIP)
	if err != nil {
		roslog.E("Rejecting invalid WireGuard peer", err, "pubKey", pubKey, "allowedIp", allowedIP, "endpointIp", endpointIP)
		return err
	}

	roslog.I("Setting WireGuard peer", "pubKey", pubKey, "allowedIp", allowedIP, "endpointIp", endpointIP)
	cmd := exec.Command("wg", args...)
	if out, err := cmd.CombinedOutput(); err != nil {
		roslog.E("wg set failed", err, "output", string(out))
		return fmt.Errorf("wg set failed: %w (%s)", err, string(out))
	}
	return nil
}
