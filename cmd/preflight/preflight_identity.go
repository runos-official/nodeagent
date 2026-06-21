package preflight

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/runos-official/nodeagent/roslog"
)

// idLabelRe matches one RFC1123 DNS label: lowercase letters/digits/hyphen, no
// leading/trailing hyphen, 1..63 chars. kubeadm uses the node hostname verbatim
// as the Kubernetes node name, which must be a valid lowercase DNS subdomain.
var idLabelRe = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?$`)

// idAllNumericRe matches an all-numeric label (e.g. "12345"), which is IP-shaped
// and rejected as a hostname label by RFC1123/Kubernetes.
var idAllNumericRe = regexp.MustCompile(`^[0-9]+$`)

// idGenericDefaults are stock image hostnames that collide across freshly cloned
// VMs (every node ends up named the same), which kubeadm then rejects as a
// duplicate node registration.
var idGenericDefaults = map[string]bool{
	"localhost":   true,
	"localhost6":  true,
	"ubuntu":      true,
	"debian":      true,
	"vagrant":     true,
	"raspberrypi": true,
	"linux":       true,
	"server":      true,
	"node":        true,
	"localdomain": true,
	"":            true,
}

// idRun runs a command with a hard timeout and returns trimmed combined stdout.
// It never blocks indefinitely: a missing/hanging tool yields ("", err) so the
// caller can degrade to "cannot determine -> don't block".
func idRun(name string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, name, args...).Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// idReadTrim reads a file and returns its trimmed contents, or ("", err).
func idReadTrim(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(b)), nil
}

// idValidHostnameLabels validates a (possibly dotted) hostname against RFC1123:
// total <=253, each label matches idLabelRe, no all-numeric label. It returns a
// reason string when invalid, or "" when the name is acceptable.
func idValidHostnameLabels(name string) string {
	if name == "" {
		return "hostname is empty"
	}
	if len(name) > 253 {
		return fmt.Sprintf("hostname is %d characters (>253 max)", len(name))
	}
	for _, label := range strings.Split(name, ".") {
		if label == "" {
			return "hostname has an empty dot-separated label"
		}
		if len(label) > 63 {
			return fmt.Sprintf("label %q exceeds 63 characters", label)
		}
		if strings.ContainsAny(label, "_") {
			return fmt.Sprintf("label %q contains '_' (not allowed in a Kubernetes node name)", label)
		}
		if label != strings.ToLower(label) {
			return fmt.Sprintf("label %q is not lowercase", label)
		}
		if idAllNumericRe.MatchString(label) {
			return fmt.Sprintf("label %q is all-numeric (IP-shaped)", label)
		}
		if !idLabelRe.MatchString(label) {
			return fmt.Sprintf("label %q is not a valid RFC1123 DNS label", label)
		}
	}
	return ""
}

// checkHostnameValidAndPersistent verifies the node hostname is a valid,
// lowercase, persistent RFC1123 name suitable as a Kubernetes node name. It
// prevents the cryptic kubeadm-join failure / duplicate-node registration that
// happens when the name is invalid (underscore/uppercase/too long/IP-shaped),
// maps only to 127.0.0.1, or silently renames on reboot (running hostname
// differs from /etc/hostname, or only a transient name is set so DHCP/cloud-init
// will change it). Generic stock names that clash across clones are warned.
func checkHostnameValidAndPersistent() error {
	running, err := os.Hostname()
	if err != nil || strings.TrimSpace(running) == "" {
		// Cannot determine the running hostname reliably -> don't block.
		roslog.W("cannot read running hostname; skipping hostname check", err)
		return nil
	}
	running = strings.TrimSpace(running)

	// 1. RFC1123 validity of the running name (this is what kubeadm will use).
	if reason := idValidHostnameLabels(running); reason != "" {
		return fmt.Errorf("hostname %q is not a valid Kubernetes node name: %s.\n\nkubeadm uses the hostname verbatim as the node name; an invalid name fails the join.\nSet a unique, persistent, lowercase RFC1123 name and re-run:\n  sudo hostnamectl set-hostname my-node-01\n  # update /etc/hosts to map it to a non-loopback address\n  sudo reboot   # confirm it sticks, then re-run preflight", running, reason)
	}

	// 2. Generic stock defaults collide across cloned images. Warn (not block):
	// the name is technically valid, but two such nodes register as duplicates.
	if idGenericDefaults[strings.ToLower(running)] {
		roslog.W(fmt.Sprintf("hostname %q is a generic default that collides across cloned VMs; give each node a unique name (sudo hostnamectl set-hostname my-node-01)", running), nil)
	}

	// 3. Persistence: the running name must match /etc/hostname, else it renames
	// on reboot and the node re-registers under a new (duplicate) name. Only
	// assert when /etc/hostname is readable and non-empty.
	if etcName, rerr := idReadTrim("/etc/hostname"); rerr == nil && etcName != "" {
		// /etc/hostname may legitimately carry only the short name while the
		// running name is FQDN; compare on the first label to avoid a false
		// positive when only the domain suffix differs.
		runShort := strings.SplitN(running, ".", 2)[0]
		etcShort := strings.SplitN(etcName, ".", 2)[0]
		if runShort != etcShort {
			return fmt.Errorf("running hostname %q does not match /etc/hostname (%q); it will rename on reboot and register a duplicate node.\n\nMake the name persistent and re-run:\n  sudo hostnamectl set-hostname %s\n  cat /etc/hostname   # confirm it now reads %s\n  sudo reboot", running, etcName, runShort, runShort)
		}
	} else {
		// No persistent /etc/hostname. If hostnamectl reports a transient-only
		// name, DHCP/cloud-init will change it. Only block when we can confirm
		// the static name is unset via hostnamectl; otherwise stay quiet.
		if static := idStaticHostname(); static == "" && idHostnamectlPresent() {
			return fmt.Errorf("hostname %q is transient only (no static hostname set); DHCP/cloud-init will change it after reboot and the node will re-register.\n\nPin a persistent name and re-run:\n  sudo hostnamectl set-hostname %s\n  sudo reboot", running, running)
		}
		roslog.W("could not confirm hostname persistence (/etc/hostname unreadable); proceeding", nil)
	}

	// 4. /etc/hosts must not map the hostname only to 127.0.0.1 (kubelet then
	// advertises loopback). 127.0.1.1 is the Debian/Ubuntu convention and fine.
	// Only block when we positively see a 127.0.0.1 mapping and NO non-loopback
	// mapping for the name; ambiguity (file missing, name absent) does not block.
	if reason := idHostsLoopbackOnly(running); reason != "" {
		return fmt.Errorf("hostname %q resolves only to loopback in /etc/hosts: %s.\n\nkubelet would advertise 127.0.0.1 and the node would be unreachable in-cluster.\nMap the hostname to this node's real address (or use the 127.0.1.1 convention):\n  # in /etc/hosts, e.g.:  192.0.2.10  %s\nThen re-run.", running, reason, running)
	}

	return nil
}

// idStaticHostname returns the static hostname from hostnamectl, or "" if unset
// or hostnamectl is unavailable.
func idStaticHostname() string {
	out, err := idRun("hostnamectl", "--static", "status")
	if err != nil {
		// Fallback: hostnamectl without subcommand prints "Static hostname: ..".
		out, err = idRun("hostnamectl", "status")
		if err != nil {
			return ""
		}
		for _, line := range strings.Split(out, "\n") {
			line = strings.TrimSpace(line)
			if strings.HasPrefix(line, "Static hostname:") {
				v := strings.TrimSpace(strings.TrimPrefix(line, "Static hostname:"))
				if v == "n/a" {
					return ""
				}
				return v
			}
		}
		return ""
	}
	out = strings.TrimSpace(out)
	if out == "n/a" {
		return ""
	}
	return out
}

// idHostnamectlPresent reports whether hostnamectl is on PATH (so its absence
// does not get misread as "no static hostname").
func idHostnamectlPresent() bool {
	_, err := exec.LookPath("hostnamectl")
	return err == nil
}

// idHostsLoopbackOnly returns a reason when /etc/hosts maps the hostname to
// 127.0.0.1 with no non-loopback mapping, else "". Missing file or absent name
// -> "" (don't block on ambiguity).
func idHostsLoopbackOnly(hostname string) string {
	data, err := os.ReadFile("/etc/hosts")
	if err != nil {
		return ""
	}
	short := strings.SplitN(hostname, ".", 2)[0]
	mapsTo127001 := false
	mapsToNonLoopback := false
	seen := false
	for _, line := range strings.Split(string(data), "\n") {
		if i := strings.IndexByte(line, '#'); i >= 0 {
			line = line[:i]
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		ip := net.ParseIP(fields[0])
		if ip == nil {
			continue
		}
		nameMatch := false
		for _, n := range fields[1:] {
			if n == hostname || strings.SplitN(n, ".", 2)[0] == short {
				nameMatch = true
				break
			}
		}
		if !nameMatch {
			continue
		}
		seen = true
		if ip.Equal(net.IPv4(127, 0, 0, 1)) {
			mapsTo127001 = true
		} else if !ip.IsLoopback() {
			mapsToNonLoopback = true
		}
		// 127.0.1.1 (and other 127.x) are treated as the acceptable
		// Debian/Ubuntu convention: neither flag is set, so we don't block.
	}
	if seen && mapsTo127001 && !mapsToNonLoopback {
		return "mapped to 127.0.0.1 with no non-loopback entry"
	}
	return ""
}

// idHexRe matches a 32-character lowercase-hex machine-id (the systemd format).
var idHexRe = regexp.MustCompile(`^[0-9a-f]{32}$`)

// checkMachineId verifies /etc/machine-id is present, well-formed, persistent,
// and not a known cloned/blank/placeholder value. RunOS keys node identity on
// the machine-id; a missing, tmpfs-backed (resets on reboot), or duplicated
// (golden-image clone) id makes the node re-register as new or collide with
// another node, which breaks kubelet/CNI identity in confusing ways.
func checkMachineId() error {
	const path = "/etc/machine-id"

	id, err := idReadTrim(path)
	if err != nil {
		return fmt.Errorf("cannot read %s: %v.\n\nRunOS keys node identity on the machine-id. Regenerate it and re-run:\n  sudo systemd-machine-id-setup\n  sudo reboot", path, err)
	}
	if id == "" {
		return fmt.Errorf("%s is empty.\n\nRunOS keys node identity on a stable machine-id; an empty id makes the node fail to register.\nRegenerate it and re-run:\n  sudo rm -f /etc/machine-id /var/lib/dbus/machine-id\n  sudo systemd-machine-id-setup\n  sudo dbus-uuidgen --ensure\n  sudo reboot", path)
	}

	// systemd writes the literal "uninitialized\n" on first boot of an image
	// that was prepared with an empty machine-id; it must be regenerated.
	if id == "uninitialized" {
		return fmt.Errorf("%s holds the systemd 'uninitialized' placeholder (first-boot token from a golden image).\n\nRegenerate a real, persistent id and re-run:\n  sudo rm -f /etc/machine-id /var/lib/dbus/machine-id\n  sudo systemd-machine-id-setup\n  sudo dbus-uuidgen --ensure\n  sudo reboot", path)
	}
	if !idHexRe.MatchString(id) {
		return fmt.Errorf("%s value %q is not a valid 32-char lowercase-hex machine-id.\n\nRegenerate it and re-run:\n  sudo rm -f /etc/machine-id /var/lib/dbus/machine-id\n  sudo systemd-machine-id-setup\n  sudo dbus-uuidgen --ensure\n  sudo reboot", path, id)
	}
	if id == strings.Repeat("0", 32) {
		return fmt.Errorf("%s is all-zero, which is not a usable machine-id.\n\nRegenerate it and re-run:\n  sudo rm -f /etc/machine-id /var/lib/dbus/machine-id\n  sudo systemd-machine-id-setup\n  sudo dbus-uuidgen --ensure\n  sudo reboot", path)
	}

	// Persistence: if /etc/machine-id is a symlink into a tmpfs (/run, /tmp,
	// /var/run), the id is regenerated every boot. Only block when we can
	// resolve the link target and it is clearly volatile.
	if target := idSymlinkTarget(path); target != "" {
		if idIsVolatilePath(target) {
			return fmt.Errorf("%s is a symlink to a non-persistent path (%s); the machine-id resets on every reboot and the node re-registers each time.\n\nMake it persistent and re-run:\n  sudo rm -f /etc/machine-id\n  sudo systemd-machine-id-setup   # writes a real file at /etc/machine-id\n  sudo reboot", path, target)
		}
	}

	// Cross-check the dbus machine-id: when present it must match /etc/machine-id,
	// otherwise D-Bus-dependent tooling sees a split identity. A missing dbus id
	// is fine (not all hosts run dbus). Mismatch is a real, fixable problem.
	const dbusPath = "/var/lib/dbus/machine-id"
	if dbusID, derr := idReadTrim(dbusPath); derr == nil && dbusID != "" {
		// dbus file may itself be a symlink to /etc/machine-id (the common case)
		// in which case the contents already match.
		if idHexRe.MatchString(dbusID) && dbusID != id {
			return fmt.Errorf("machine-id mismatch: %s=%s but %s=%s; the two must agree.\n\nReconcile them and re-run:\n  sudo rm -f /etc/machine-id /var/lib/dbus/machine-id\n  sudo systemd-machine-id-setup\n  sudo dbus-uuidgen --ensure\n  sudo reboot", path, id, dbusPath, dbusID)
		}
	}

	return nil
}

// idSymlinkTarget returns the resolved target of path if it is a symlink, else
// "". Errors collapse to "" (treat as "not a problematic symlink").
func idSymlinkTarget(path string) string {
	fi, err := os.Lstat(path)
	if err != nil || fi.Mode()&os.ModeSymlink == 0 {
		return ""
	}
	target, err := filepath.EvalSymlinks(path)
	if err != nil {
		// Fall back to the raw link text if the chain can't be fully resolved.
		if raw, rerr := os.Readlink(path); rerr == nil {
			return raw
		}
		return ""
	}
	return target
}

// idIsVolatilePath reports whether a path lives under a conventionally tmpfs /
// non-persistent location.
func idIsVolatilePath(p string) bool {
	p = filepath.Clean(p)
	for _, prefix := range []string{"/run/", "/var/run/", "/tmp/", "/dev/shm/"} {
		if strings.HasPrefix(p, prefix) {
			return true
		}
	}
	return p == "/run" || p == "/tmp"
}

// checkResolvConfAndNsswitch verifies the system resolver layer (not just Go's
// resolver) can actually resolve names. It catches the failure where a Go-only
// DNS probe passes but apt/containerd/kubelet cannot resolve anything: no
// nameserver in /etc/resolv.conf, a systemd-resolved stub (127.0.0.53) with the
// service down, or an nsswitch 'hosts:' line missing 'dns'. The signature is a
// getent/system-path lookup failing while Go's resolver succeeds.
func checkResolvConfAndNsswitch() error {
	// 1. /etc/resolv.conf must list at least one nameserver. Follow symlinks via
	// os.ReadFile (it reads through links). If unreadable, don't block.
	nameservers := idResolvNameservers()
	if len(nameservers) == 0 {
		// Distinguish "file unreadable" (ambiguous, skip) from "readable but no
		// nameserver" (real problem).
		if _, err := os.Stat("/etc/resolv.conf"); err != nil {
			roslog.W("cannot read /etc/resolv.conf; skipping resolver check", err)
			return nil
		}
		return fmt.Errorf("/etc/resolv.conf has no 'nameserver' entry; system tools (apt, containerd, kubelet) cannot resolve names.\n\nAdd a working resolver (or enable systemd-resolved) and re-run:\n  echo 'nameserver 1.1.1.1' | sudo tee /etc/resolv.conf\n  getent hosts pkgs.k8s.io   # must return an address")
	}

	// 2. If the only nameserver is the systemd-resolved stub, the resolved
	// service must be active and answering on 127.0.0.53:53.
	if idOnlyStubNameservers(nameservers) {
		if !idResolvedActive() {
			return fmt.Errorf("/etc/resolv.conf points only at the systemd-resolved stub (127.0.0.53) but systemd-resolved is not active; no name resolution will work.\n\nStart it (or replace resolv.conf with a real nameserver) and re-run:\n  sudo systemctl enable --now systemd-resolved\n  getent hosts pkgs.k8s.io   # must return an address")
		}
		if !idTCPProbe("127.0.0.53:53") && !idUDPStubAnswers() {
			return fmt.Errorf("systemd-resolved stub 127.0.0.53:53 is not answering; system DNS is broken.\n\nRestart the resolver and re-run:\n  sudo systemctl restart systemd-resolved\n  getent hosts pkgs.k8s.io   # must return an address")
		}
	}

	// 3. nsswitch 'hosts:' line must include 'dns'. Only block when the file is
	// readable and the line is present but lacks 'dns'. A missing nsswitch.conf
	// means glibc defaults (which include dns) -> don't block.
	if hostsLine, ok := idNsswitchHostsLine(); ok {
		if !idFieldsContain(hostsLine, "dns") {
			return fmt.Errorf("/etc/nsswitch.conf 'hosts:' line (%q) does not include 'dns'; glibc-based tools (apt, containerd) will not perform DNS lookups even though the agent's Go resolver can.\n\nFix the line and re-run:\n  # ensure /etc/nsswitch.conf has:  hosts: files dns\n  getent hosts pkgs.k8s.io   # must return an address", hostsLine)
		}
		// Guard against referencing an NSS module whose shared lib is absent
		// (e.g. 'mymachines'/'resolve' on a host that lacks them), which makes
		// getent fail hard. Only flag a module we can prove is missing.
		if missing := idMissingNssModule(hostsLine); missing != "" {
			return fmt.Errorf("/etc/nsswitch.conf 'hosts:' references NSS module %q but libnss_%s.so is not installed; getent will fail.\n\nInstall the module or remove it from the 'hosts:' line, then re-run:\n  getent hosts pkgs.k8s.io   # must return an address", missing, missing)
		}
	}

	// 4. Corroborate: system path (getent) vs Go resolver. A divergence where
	// getent FAILS but Go SUCCEEDS is the cgo/nsswitch breakage we want to catch.
	// Both failing is a generic DNS problem owned by checkDNSResolution; both
	// succeeding is healthy. Only block on the specific getent-fails/Go-works
	// split, and only if getent exists.
	if idHasGetent() {
		const probe = "pkgs.k8s.io"
		getentOK := idGetentResolves(probe)
		goOK := idGoResolves(probe)
		if !getentOK && goOK {
			return fmt.Errorf("system resolver split: 'getent hosts %s' fails while the agent's own resolver succeeds; apt/containerd/kubelet will fail to resolve names even though preflight's Go DNS check passes.\n\nThis is an nsswitch/resolver-layer problem, not the agent. Fix /etc/nsswitch.conf ('hosts: files dns') and /etc/resolv.conf, confirm 'getent hosts %s' returns an address, then re-run. (Note: 'dig' working does NOT prove this is fixed.)", probe, probe)
		}
	}

	// 5. Advisory: glibc only consults the first 3 nameservers. More than 3 is a
	// warn (not block) as long as one of the first three still answers.
	if len(nameservers) > 3 {
		roslog.W(fmt.Sprintf("/etc/resolv.conf lists %d nameservers; glibc only uses the first 3, so entries beyond that are ignored", len(nameservers)), nil)
	}

	return nil
}

// idResolvNameservers returns the nameserver IPs declared in /etc/resolv.conf
// (reading through any symlink), or nil if the file is unreadable.
func idResolvNameservers() []string {
	data, err := os.ReadFile("/etc/resolv.conf")
	if err != nil {
		return nil
	}
	var out []string
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) >= 2 && fields[0] == "nameserver" {
			out = append(out, fields[1])
		}
	}
	return out
}

// idOnlyStubNameservers reports whether every nameserver is a systemd-resolved
// loopback stub (127.0.0.53 / 127.0.0.54).
func idOnlyStubNameservers(ns []string) bool {
	if len(ns) == 0 {
		return false
	}
	for _, n := range ns {
		if n != "127.0.0.53" && n != "127.0.0.54" {
			return false
		}
	}
	return true
}

// idResolvedActive reports whether systemd-resolved is active per systemctl. A
// missing systemctl yields false (we then rely on the stub probe).
func idResolvedActive() bool {
	out, err := idRun("systemctl", "is-active", "systemd-resolved")
	if err != nil {
		return false
	}
	return strings.TrimSpace(out) == "active"
}

// idTCPProbe attempts a short TCP connect to addr (host:port).
func idTCPProbe(addr string) bool {
	conn, err := net.DialTimeout("tcp", addr, 3*time.Second)
	if err != nil {
		return false
	}
	conn.Close()
	return true
}

// idUDPStubAnswers does a best-effort DNS query against the resolved stub over
// UDP using Go's resolver bound to 127.0.0.53. Returns true on any answer.
func idUDPStubAnswers() bool {
	r := &net.Resolver{
		PreferGo: true,
		Dial: func(ctx context.Context, network, address string) (net.Conn, error) {
			d := net.Dialer{Timeout: 3 * time.Second}
			return d.DialContext(ctx, "udp", "127.0.0.53:53")
		},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	addrs, err := r.LookupHost(ctx, "pkgs.k8s.io")
	return err == nil && len(addrs) > 0
}

// idNsswitchHostsLine returns the 'hosts:' configuration (everything after the
// key) from /etc/nsswitch.conf and whether such a line was found.
func idNsswitchHostsLine() (string, bool) {
	data, err := os.ReadFile("/etc/nsswitch.conf")
	if err != nil {
		return "", false
	}
	for _, line := range strings.Split(string(data), "\n") {
		if i := strings.IndexByte(line, '#'); i >= 0 {
			line = line[:i]
		}
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "hosts:") {
			return strings.TrimSpace(strings.TrimPrefix(line, "hosts:")), true
		}
	}
	return "", false
}

// idFieldsContain reports whether token appears as a whitespace-separated field
// in s (ignoring nsswitch action syntax like 'dns [NOTFOUND=return]').
func idFieldsContain(s, token string) bool {
	for _, f := range strings.Fields(s) {
		if strings.HasPrefix(f, "[") {
			continue
		}
		if f == token {
			return true
		}
	}
	return false
}

// idKnownNssModules maps nsswitch 'hosts:' source tokens whose absence we can
// safely flag. We only check modules that ship as a separate libnss_<x>.so and
// are commonly referenced; built-ins ('files', 'dns', 'myhostname' on systemd
// hosts) and action blocks are skipped.
var idKnownNssModules = map[string]bool{
	"resolve":       true,
	"mymachines":    true,
	"mdns":          true,
	"mdns4":         true,
	"mdns6":         true,
	"mdns4_minimal": true,
	"mdns6_minimal": true,
	"mdns_minimal":  true,
}

// idMissingNssModule returns the first NSS source in the hosts line whose
// libnss_<mod>.so cannot be found under any /lib*/ or /usr/lib*/ multiarch dir,
// or "" if none are provably missing. Conservative: unknown tokens are ignored.
func idMissingNssModule(hostsLine string) string {
	for _, f := range strings.Fields(hostsLine) {
		if strings.HasPrefix(f, "[") {
			continue
		}
		mod := f
		if !idKnownNssModules[mod] {
			continue
		}
		if !idNssModuleLibExists(mod) {
			return mod
		}
	}
	return ""
}

// idNssModuleLibExists reports whether libnss_<mod>.so* exists in any common lib
// directory. On any glob error it returns true (assume present -> don't block).
func idNssModuleLibExists(mod string) bool {
	patterns := []string{
		fmt.Sprintf("/lib/*/libnss_%s.so*", mod),
		fmt.Sprintf("/usr/lib/*/libnss_%s.so*", mod),
		fmt.Sprintf("/lib/libnss_%s.so*", mod),
		fmt.Sprintf("/usr/lib/libnss_%s.so*", mod),
		fmt.Sprintf("/lib64/libnss_%s.so*", mod),
		fmt.Sprintf("/usr/lib64/libnss_%s.so*", mod),
	}
	for _, p := range patterns {
		matches, err := filepath.Glob(p)
		if err != nil {
			return true
		}
		if len(matches) > 0 {
			return true
		}
	}
	return false
}

// idHasGetent reports whether the getent binary is available.
func idHasGetent() bool {
	_, err := exec.LookPath("getent")
	return err == nil
}

// idGetentResolves reports whether 'getent hosts <name>' returns at least one
// address (the system/NSS path). A non-zero exit (no address) -> false.
func idGetentResolves(name string) bool {
	out, err := idRun("getent", "hosts", name)
	if err != nil {
		return false
	}
	return strings.TrimSpace(out) != ""
}

// idGoResolves reports whether Go's resolver can resolve name, with a bound.
func idGoResolves(name string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	addrs, err := net.DefaultResolver.LookupHost(ctx, name)
	return err == nil && len(addrs) > 0
}

// idRunosWGCIDRs are the WireGuard overlay ranges RunOS assigns node mesh IPs
// from. Any pre-existing local interface/route overlapping these causes
// duplicate/asymmetric routing that breaks the mesh.
var idRunosWGCIDRs = []string{
	"172.24.0.0/16",   // wg0 node mesh
	"172.24.200.0/21", // wg1 (subset of the /16, kept explicit for messaging)
}

// idAddrEntry models the subset of `ip -j addr` we read.
type idAddrEntry struct {
	IfName   string `json:"ifname"`
	AddrInfo []struct {
		Local     string `json:"local"`
		PrefixLen int    `json:"prefixlen"`
	} `json:"addr_info"`
}

// idRouteEntry models the subset of `ip -j route` we read.
type idRouteEntry struct {
	Dst     string `json:"dst"`
	Dev     string `json:"dev"`
	PrefSrc string `json:"prefsrc"`
}

// checkWireguardSubnetOverlap blocks when an existing (non-WireGuard) interface
// address or route on this host overlaps RunOS's WireGuard range 172.24.0.0/16.
// An overlap (a Docker bridge, VPN, or LAN re-using 172.24/16) produces
// duplicate routes and asymmetric routing that silently breaks the node mesh or
// the node's own connectivity once wg0/wg1 come up. Purely local, no network.
func checkWireguardSubnetOverlap() error {
	wgNets := make([]*net.IPNet, 0, len(idRunosWGCIDRs))
	for _, c := range idRunosWGCIDRs {
		_, n, err := net.ParseCIDR(c)
		if err == nil {
			wgNets = append(wgNets, n)
		}
	}
	if len(wgNets) == 0 {
		return nil
	}

	var conflicts []string

	// Interfaces: any non-wg iface holding an address inside the wg range.
	if addrs, ok := idParseAddrs(); ok {
		for _, a := range addrs {
			if idIsWGInterface(a.IfName) {
				continue
			}
			for _, ai := range a.AddrInfo {
				ip := net.ParseIP(ai.Local)
				if ip == nil || ip.To4() == nil {
					continue
				}
				if n := idMatchWG(ip, wgNets); n != "" {
					conflicts = append(conflicts, fmt.Sprintf("interface %s holds %s/%d (overlaps %s)", a.IfName, ai.Local, ai.PrefixLen, n))
				}
			}
		}
	}

	// Routes: any route (not on wg0/wg1) whose destination overlaps, or whose
	// prefsrc is inside the wg range.
	if routes, ok := idParseRoutes(); ok {
		for _, r := range routes {
			if idIsWGInterface(r.Dev) {
				continue
			}
			if r.Dst != "" && r.Dst != "default" {
				if rn := idRouteOverlap(r.Dst, wgNets); rn != "" {
					conflicts = append(conflicts, fmt.Sprintf("route %s dev %s overlaps %s", r.Dst, r.Dev, rn))
				}
			}
			if r.PrefSrc != "" {
				if ip := net.ParseIP(r.PrefSrc); ip != nil {
					if n := idMatchWG(ip, wgNets); n != "" {
						conflicts = append(conflicts, fmt.Sprintf("route source %s (dev %s) is inside %s", r.PrefSrc, r.Dev, n))
					}
				}
			}
		}
	}

	if len(conflicts) > 0 {
		return fmt.Errorf("an existing network on this host overlaps RunOS's WireGuard range 172.24.0.0/16:\n  %s\n\nRunOS assigns node mesh IPs from 172.24.0.0/16 (wg0) and 172.24.200.0/21 (wg1); an overlap causes duplicate routes and asymmetric routing that breaks the mesh or the node's own connectivity.\nMove the conflicting interface/Docker bridge/VPN off 172.24.0.0/16 (re-IP the Docker bridge in /etc/docker/daemon.json, or re-IP the LAN/VPN), then re-run. This is an addressing conflict in your environment, not a RunOS error.", strings.Join(idDedup(conflicts), "\n  "))
	}
	return nil
}

// idIsWGInterface reports whether dev is one of the WireGuard interfaces RunOS
// itself manages (so our own future addresses/routes don't count as conflicts).
func idIsWGInterface(dev string) bool {
	return dev == "wg0" || dev == "wg1" || strings.HasPrefix(dev, "wg")
}

// idMatchWG returns the wg CIDR string that contains ip, or "".
func idMatchWG(ip net.IP, wgNets []*net.IPNet) string {
	for _, n := range wgNets {
		if n.Contains(ip) {
			return n.String()
		}
	}
	return ""
}

// idRouteOverlap returns a wg CIDR string that overlaps the route destination
// CIDR dst (either direction of containment), or "".
func idRouteOverlap(dst string, wgNets []*net.IPNet) string {
	// dst may be a bare IP (host route) or a CIDR.
	var dn *net.IPNet
	if ip := net.ParseIP(dst); ip != nil {
		if ip.To4() == nil {
			return ""
		}
		dn = &net.IPNet{IP: ip, Mask: net.CIDRMask(32, 32)}
	} else {
		_, parsed, err := net.ParseCIDR(dst)
		if err != nil {
			return ""
		}
		dn = parsed
	}
	for _, wn := range wgNets {
		if idNetsOverlap(dn, wn) {
			return wn.String()
		}
	}
	return ""
}

// idNetsOverlap reports whether two IPv4 networks intersect (either contains the
// other's base address).
func idNetsOverlap(a, b *net.IPNet) bool {
	return a.Contains(b.IP) || b.Contains(a.IP)
}

// idParseAddrs runs `ip -j addr` and parses it. ok=false on any failure (tool
// missing / non-JSON / parse error) so the caller degrades to "can't tell".
func idParseAddrs() ([]idAddrEntry, bool) {
	out, err := idRun("ip", "-j", "addr")
	if err != nil || out == "" {
		return nil, false
	}
	var entries []idAddrEntry
	if err := json.Unmarshal([]byte(out), &entries); err != nil {
		return nil, false
	}
	return entries, true
}

// idParseRoutes runs `ip -j route` and parses it. ok=false on any failure.
func idParseRoutes() ([]idRouteEntry, bool) {
	out, err := idRun("ip", "-j", "route")
	if err != nil || out == "" {
		return nil, false
	}
	var entries []idRouteEntry
	if err := json.Unmarshal([]byte(out), &entries); err != nil {
		return nil, false
	}
	return entries, true
}

// idDedup returns s with duplicate strings removed, preserving order.
func idDedup(s []string) []string {
	seen := make(map[string]bool, len(s))
	var out []string
	for _, v := range s {
		if !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	}
	return out
}
