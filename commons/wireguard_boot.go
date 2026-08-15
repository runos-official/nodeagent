package commons

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/runos-official/nodeagent/roslog"
)

// The wg0 boot ordering repair.
//
// WHAT WAS WRONG. The stock wg-quick@.service is `After=nss-lookup.target`. dnsmasq PROVIDES
// nss-lookup.target, and RunOS orders dnsmasq After=wg-quick@wg0 because dnsmasq binds wg0's
// address. That is a cycle, and systemd breaks a cycle by deleting one job. Measured on a Hetzner
// Ubuntu 24.04 node 2026-08-15: "dnsmasq.service: Job wg-quick@wg0.service/start deleted to break
// ordering cycle", so wg0 did not exist after boot. dnsmasq's start-pre loop timed out at 90s,
// its restart re-pulled Wants=wg-quick@wg0, and the tunnel came up ~105s after boot. Every reboot.
//
// THE FIX IS A FILE, and it lives in two places on purpose. Nodeward writes it at install for
// new nodes (uc/prep/10_wg.go, WgQuickOverride). Nothing re-runs an install on a node that is
// already in the fleet, so the agent restores the SAME bytes on every start: a node installed
// before the reset is repaired the next time its agent runs, and a hand-edited file goes back to
// what RunOS declared. Byte-identical content on both sides is what keeps this from becoming two
// writers with two opinions.
//
// The reset (`After=` then `After=network-online.target`) is what removes nss-lookup.target from
// the ordering. wg0 never needed name resolution: every endpoint RunOS distributes is an address.

// WgQuickOverridePath is the drop-in nodeward writes at install.
const WgQuickOverridePath = "/etc/systemd/system/wg-quick@wg0.service.d/override.conf"

// WgQuickOverride must stay byte-identical to nodeward's uc/prep WgQuickOverride.
const WgQuickOverride = `[Unit]
# RunOS managed. Do not edit: the node agent restores this file.
# The ordering is RESET here, not appended. The stock unit is After=nss-lookup.target, which
# dnsmasq provides, and RunOS orders dnsmasq after wg0; keeping the stock ordering is a cycle
# that systemd breaks by not starting wg0 at boot.
After=
After=network-online.target
Wants=
Wants=network-online.target
`

// EnsureWg0BootOrder writes the override when the file on disk differs, then reloads systemd.
//
// It touches nothing when the file already matches, so a healthy node pays one read per agent
// start. A missing directory means the node has no wg0 unit yet (mid-install), and the install
// will write the file itself; nothing is created here. Failures are logged and never fatal:
// this repairs the NEXT boot, and the agent has work to do on this one.
func EnsureWg0BootOrder() {
	changed, err := ensureFileContent(WgQuickOverridePath, WgQuickOverride)
	if err != nil {
		roslog.W("Could not check the wg0 boot ordering override", err, "path", WgQuickOverridePath)
		return
	}
	if !changed {
		return
	}
	roslog.I("Restored the wg0 boot ordering override; wg0 will start at the next boot without waiting on dnsmasq", "path", WgQuickOverridePath)
	if out, err := exec.Command("systemctl", "daemon-reload").CombinedOutput(); err != nil {
		roslog.W("systemctl daemon-reload failed after restoring the wg0 override; it applies at the next reload", err, "output", string(out))
	}
}

// ensureFileContent writes want to path when the file is absent or differs. Returns whether it
// wrote. It refuses to create the parent directory: an absent directory means the unit is not
// installed here yet, and that is the installer's job.
func ensureFileContent(path, want string) (bool, error) {
	if _, err := os.Stat(filepath.Dir(path)); err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	have, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return false, err
	}
	if err == nil && bytes.Equal(have, []byte(want)) {
		return false, nil
	}
	tmp := path + ".runos-tmp"
	if err := os.WriteFile(tmp, []byte(want), 0o644); err != nil {
		return false, fmt.Errorf("write %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return false, fmt.Errorf("rename %s: %w", path, err)
	}
	return true, nil
}
