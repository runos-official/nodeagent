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
// new nodes (uc/prep/10_wg.go, WgQuickUnit). Nothing re-runs an install on a node that is
// already in the fleet, so the agent restores the SAME bytes on every start: a node installed
// before the unit existed is repaired the next time its agent runs, and a hand-edited file goes
// back to what RunOS declared. Byte-identical content on both sides is what keeps this from
// becoming two writers with two opinions.
//
// The file is the stock template with nss-lookup.target removed, as an INSTANCE unit that
// shadows the template. wg0 never needed name resolution: every endpoint RunOS distributes is an
// address.

// WgQuickUnitPath is the INSTANCE unit for wg0. A real file here shadows the stock template
// `wg-quick@.service` for this one instance, which is the only way to take a dependency OUT of
// it: systemd lets a drop-in add to After=/Wants= but never reset them ("dependencies can only be
// added in drop-ins", systemd.unit(5)). Measured 2026-08-15: a drop-in with `After=` then
// `After=network-online.target` still loaded with nss-lookup.target in the ordering.
const WgQuickUnitPath = "/etc/systemd/system/wg-quick@wg0.service"

// wgQuickDropInDir is the drop-in an install before the instance unit wrote. It only appended
// network-online.target, which the unit declares, so it is removed rather than left to confuse.
const wgQuickDropInDir = "/etc/systemd/system/wg-quick@wg0.service.d"

// WgQuickUnit must stay byte-identical to nodeward's uc/prep WgQuickUnit (nodeward's test suite
// checks the two when the repos are checked out side by side).
const WgQuickUnit = `# RunOS managed. Do not edit: the node agent restores this file.
# This instance unit shadows the stock wg-quick@.service template for wg0. It is the stock unit
# with nss-lookup.target REMOVED from the ordering: dnsmasq provides that target and RunOS orders
# dnsmasq after wg0, so keeping it is a cycle that systemd breaks by not starting wg0 at boot.
[Unit]
Description=WireGuard via wg-quick(8) for wg0 (RunOS)
After=network-online.target
Wants=network-online.target
PartOf=wg-quick.target
Documentation=man:wg-quick(8)
Documentation=man:wg(8)

[Service]
Type=oneshot
RemainAfterExit=yes
ExecStart=/usr/bin/wg-quick up wg0
ExecStop=/usr/bin/wg-quick down wg0
ExecReload=/bin/bash -c 'exec /usr/bin/wg syncconf wg0 <(exec /usr/bin/wg-quick strip wg0)'
Environment=WG_ENDPOINT_RESOLUTION_RETRIES=infinity

[Install]
WantedBy=multi-user.target
`

// EnsureWg0BootOrder writes the instance unit when the file on disk differs, drops the old
// drop-in, then reloads systemd.
//
// It touches nothing when the file already matches, so a healthy node pays one read per agent
// start. A node with no wg-quick template installed has no wg0 yet (mid-install), and the
// install writes the file itself; nothing is created here. Failures are logged and never fatal:
// this repairs the NEXT boot, and the agent has work to do on this one.
func EnsureWg0BootOrder() {
	if _, err := os.Stat("/usr/lib/systemd/system/wg-quick@.service"); err != nil {
		if _, err2 := os.Stat("/lib/systemd/system/wg-quick@.service"); err2 != nil {
			// wireguard-tools is not installed yet; the install writes the unit.
			return
		}
	}
	changed, err := ensureFileContent(WgQuickUnitPath, WgQuickUnit)
	if err != nil {
		roslog.W("Could not check the wg0 unit", err, "path", WgQuickUnitPath)
		return
	}
	dropInRemoved := false
	if _, err := os.Stat(wgQuickDropInDir); err == nil {
		if err := os.RemoveAll(wgQuickDropInDir); err != nil {
			roslog.W("Could not remove the old wg0 drop-in", err, "path", wgQuickDropInDir)
		} else {
			dropInRemoved = true
		}
	}
	if !changed && !dropInRemoved {
		return
	}
	roslog.I("Restored the wg0 unit; wg0 will start at the next boot without waiting on dnsmasq", "path", WgQuickUnitPath, "dropInRemoved", dropInRemoved)
	if out, err := exec.Command("systemctl", "daemon-reload").CombinedOutput(); err != nil {
		roslog.W("systemctl daemon-reload failed after restoring the wg0 unit; it applies at the next reload", err, "output", string(out))
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
