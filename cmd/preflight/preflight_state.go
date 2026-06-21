package preflight

import (
	"context"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/runos-official/nodeagent/config"
	"github.com/runos-official/nodeagent/roslog"
	"github.com/spf13/viper"
)

// stInstallLockPath is the advisory lock file guarding against concurrent or
// re-entrant installs/registrations. /run is tmpfs, so the lock is naturally
// cleared on reboot (a crashed installer leaves no permanently stale lock once
// the box reboots).
const stInstallLockPath = "/run/runos-install.lock"

// Mount flag bits from statfs(2) f_flags (ST_RDONLY / ST_NOEXEC, same values as
// MS_RDONLY / MS_NOEXEC on Linux). Matches the sibling files' local-constant
// style for these flags.
const (
	stMntReadOnly = 1 // ST_RDONLY / MS_RDONLY
	stMntNoExec   = 8 // ST_NOEXEC / MS_NOEXEC
)

// stRunCmd runs argv[0] with the rest as args under a bounded context and returns
// combined stdout+stderr plus the error. It never hangs: the context timeout
// kills a stuck child. A missing binary returns an error the caller treats as
// "cannot determine" (soft-skip), never as a finding.
func stRunCmd(timeout time.Duration, name string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, name, args...).CombinedOutput()
	return string(out), err
}

// stExists reports whether a path exists (any type). Errors other than
// not-exist are treated as "exists" so a permission quirk does not make us
// under-report leftover state.
func stExists(path string) bool {
	_, err := os.Stat(path)
	if err == nil {
		return true
	}
	return !os.IsNotExist(err)
}

// stIsFile reports whether path exists and is a regular, non-empty file.
func stIsFile(path string) bool {
	fi, err := os.Stat(path)
	return err == nil && fi.Mode().IsRegular() && fi.Size() > 0
}

// stGlobHasMatch reports whether pattern matches at least one existing entry.
func stGlobHasMatch(pattern string) bool {
	m, err := filepath.Glob(pattern)
	return err == nil && len(m) > 0
}

// stServiceActive reports whether a systemd unit is active. It returns false on
// any uncertainty (systemctl missing/errored) so we never block on an ambiguous
// service state.
func stServiceActive(unit string) bool {
	path, err := exec.LookPath("systemctl")
	if err != nil {
		return false
	}
	// `systemctl is-active <unit>` prints exactly "active" on stdout when the unit
	// is running, regardless of exit code capture nuances.
	out, _ := stRunCmd(5*time.Second, path, "is-active", unit)
	return strings.TrimSpace(out) == "active"
}

// checkExistingKubernetesAndLeftovers blocks when the node already hosts a live
// cluster (kubeadm/k3s/rke2/microk8s) or carries stale cluster/RunOS leftovers
// (etcd data, kube manifests, foreign CNI, wg0, a prior /etc/runos install, held
// kube packages, conflicting containerd config). Prevents the two worst install
// outcomes: silently destroying a live cluster, and a join that fails deep inside
// kubeadm/containerd with a cryptic port- or CNI-collision error. Supersedes the
// old admin.conf-only check (also covers workers, which have no admin.conf).
func checkExistingKubernetesAndLeftovers() error {
	// --- Live cluster: do NOT touch, block loudly. ---
	var live []string
	if stServiceActive("kubelet") {
		// Confirm an apiserver is actually answering before calling it "live"; a
		// stale kubelet unit alone is a leftover, handled below.
		if stApiserverAnswers() {
			live = append(live, "live kubeadm cluster (kubelet active + apiserver on :6443)")
		}
	}
	for _, u := range []string{"k3s", "k3s-agent", "rke2-server", "rke2-agent"} {
		if stServiceActive(u) {
			live = append(live, "live "+u)
		}
	}
	if stExists("/etc/rancher/k3s") || stExists("/etc/rancher/rke2") {
		live = append(live, "k3s/rke2 config under /etc/rancher")
	}
	if stExists("/var/snap/microk8s") {
		live = append(live, "microk8s (snap)")
	}

	// --- Stale leftovers (only matter when not already flagged live). ---
	var stale []string
	if stGlobHasMatch("/etc/kubernetes/manifests/*.yaml") {
		stale = append(stale, "/etc/kubernetes/manifests/*.yaml")
	}
	if stExists("/etc/kubernetes/pki/ca.crt") {
		stale = append(stale, "/etc/kubernetes/pki/ca.crt")
	}
	if stExists("/etc/kubernetes/admin.conf") || stExists("/etc/kubernetes/kubelet.conf") {
		stale = append(stale, "/etc/kubernetes/*.conf")
	}
	if stExists("/var/lib/etcd/member") {
		stale = append(stale, "/var/lib/etcd/member (etcd data)")
	}
	if stForeignCNIPresent() {
		stale = append(stale, "foreign CNI config in /etc/cni/net.d")
	}
	if stExists("/etc/wireguard/wg0.conf") || stLinkExists("wg0") {
		stale = append(stale, "wg0 / /etc/wireguard/wg0.conf")
	}
	// NOTE: do NOT key this on /etc/runos/config.yaml or the mTLS CA path — config
	// init writes config.yaml on EVERY command (including this preflight), and the
	// installer downloads the L1Sec CA *before* preflight runs, so both would
	// false-positive on a clean install. The systemd unit is the one unambiguous
	// "a prior install actually ran" marker (it does not exist yet at preflight
	// time in a normal install). Registration leftovers are owned by
	// checkAlreadyRegistered.
	if stExists("/etc/systemd/system/runos.service") {
		stale = append(stale, "a prior RunOS install (the runos.service systemd unit)")
	}
	if held := stHeldKubePackages(); len(held) > 0 {
		stale = append(stale, "held packages: "+strings.Join(held, " "))
	}
	if stContainerdConfigConflicts() {
		stale = append(stale, "conflicting /etc/containerd/config.toml (cri disabled or SystemdCgroup=false)")
	}

	if len(live) > 0 {
		return fmt.Errorf("a LIVE cluster was detected on this host (%s).\n\nRunOS will not install onto a running cluster (it would destroy it). Install on a clean node instead.\nIf this node is disposable, tear the cluster down first, then re-run:\n  sudo kubeadm reset -f            # (kubeadm hosts)\n  sudo rm -rf /etc/kubernetes /var/lib/etcd /etc/cni/net.d/*\n  sudo ip link del wg0 2>/dev/null || true",
			strings.Join(live, "; "))
	}
	if len(stale) > 0 {
		return fmt.Errorf("leftover cluster/RunOS state was detected on this host (%s).\n\nRunOS will not install over leftovers (they collide with the fresh install). Clean up, then re-run:\n  sudo runos uninstall                              # if a prior RunOS install\n  sudo kubeadm reset -f                             # if a prior kubeadm host\n  sudo rm -rf /etc/kubernetes /var/lib/etcd /etc/cni/net.d/* /etc/runos\n  sudo ip link del wg0 2>/dev/null || true\n  sudo apt-mark unhold kubelet kubeadm kubectl containerd 2>/dev/null || true",
			strings.Join(stale, "; "))
	}
	return nil
}

// stApiserverAnswers reports whether something is accepting TCP on
// 127.0.0.1:6443 (a live apiserver). Bounded dial; any failure => false so we
// never over-claim "live".
func stApiserverAnswers() bool {
	conn, err := net.DialTimeout("tcp", "127.0.0.1:6443", 3*time.Second)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

// stLinkExists reports whether a network interface (e.g. wg0) exists. Uses
// `ip link show`; missing ip or any error => false (soft, never blocks alone).
func stLinkExists(name string) bool {
	path, err := exec.LookPath("ip")
	if err != nil {
		return false
	}
	_, err = stRunCmd(5*time.Second, path, "link", "show", name)
	return err == nil
}

// stForeignCNIPresent reports whether /etc/cni/net.d contains a CNI config that
// is NOT Cilium (Cilium is what RunOS installs). A Cilium-only dir is not a
// conflict. Ambiguity (unreadable dir) => false.
func stForeignCNIPresent() bool {
	entries, err := os.ReadDir("/etc/cni/net.d")
	if err != nil {
		return false
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := strings.ToLower(e.Name())
		if !strings.HasSuffix(name, ".conf") && !strings.HasSuffix(name, ".conflist") && !strings.HasSuffix(name, ".json") {
			continue
		}
		if strings.Contains(name, "cilium") {
			continue
		}
		b, rerr := os.ReadFile(filepath.Join("/etc/cni/net.d", e.Name()))
		if rerr != nil {
			// A present-but-unreadable foreign-looking file is suspicious; treat
			// as foreign so we do not silently install over it.
			return true
		}
		if strings.Contains(strings.ToLower(string(b)), "cilium") {
			continue
		}
		return true
	}
	return false
}

// stHeldKubePackages returns the apt holds that match kube*/containerd. A held
// package pins a version that will fight the installer's apt steps. Missing
// apt-mark => empty (we cannot tell, so we do not block on it).
func stHeldKubePackages() []string {
	path, err := exec.LookPath("apt-mark")
	if err != nil {
		return nil
	}
	out, err := stRunCmd(5*time.Second, path, "showhold")
	if err != nil {
		return nil
	}
	var held []string
	for _, line := range strings.Split(out, "\n") {
		p := strings.TrimSpace(line)
		if p == "" {
			continue
		}
		if strings.HasPrefix(p, "kube") || p == "containerd" || strings.HasPrefix(p, "containerd") {
			held = append(held, p)
		}
	}
	return held
}

// stContainerdConfigConflicts reports whether an existing containerd config would
// break the kubelet (CRI plugin disabled, or cgroupfs instead of systemd). Only
// flags a config that is present AND clearly misconfigured; absent or fine => no
// conflict.
func stContainerdConfigConflicts() bool {
	b, err := os.ReadFile("/etc/containerd/config.toml")
	if err != nil {
		return false
	}
	text := string(b)
	low := strings.ToLower(text)
	// CRI explicitly disabled: disabled_plugins = ["cri"] (or with extra members).
	if strings.Contains(low, "disabled_plugins") {
		// crude but safe: only flag when "cri" appears on a disabled_plugins line.
		for _, line := range strings.Split(low, "\n") {
			if strings.Contains(line, "disabled_plugins") && strings.Contains(line, "cri") {
				return true
			}
		}
	}
	// SystemdCgroup = false present (case-insensitive, whitespace-tolerant).
	for _, line := range strings.Split(text, "\n") {
		l := strings.ToLower(strings.ReplaceAll(line, " ", ""))
		if strings.Contains(l, "systemdcgroup=false") {
			return true
		}
	}
	return false
}

// checkAlreadyRegistered blocks when this node already looks registered: a real
// node ID in config.yaml plus a parseable, non-expired mTLS client cert and a
// private key on disk. Prevents a silent re-registration that burns a fresh
// enrollment token and creates a duplicate node in the console. (Local only.)
func checkAlreadyRegistered() error {
	nid := strings.TrimSpace(config.GetNID())
	if nid == "" || nid == "xxxxx" {
		// No node ID yet -> not registered. Nothing to assert.
		return nil
	}

	crtPath := stRawPath("mtls.crt", "/etc/runos/mtls.crt")
	keyPath := stRawPath("mtls.key", "/etc/runos/mtls.key")

	// Both cert and key must be present and the cert must parse; otherwise this is
	// a half-state, not a confident "already registered" -> do not block.
	if !stIsFile(keyPath) {
		return nil
	}
	crtBytes, err := os.ReadFile(crtPath)
	if err != nil || len(crtBytes) == 0 {
		return nil
	}
	cert := stParseFirstCert(crtBytes)
	if cert == nil {
		// Cert file present but unparseable -> ambiguous; let the dedicated CA/cert
		// checks speak. Do not block here.
		return nil
	}
	now := time.Now()
	if now.Before(cert.NotBefore) || now.After(cert.NotAfter) {
		// Expired/not-yet-valid cert is not a confident "registered & healthy"
		// state; a re-provision is the right path and other checks cover it.
		return nil
	}

	aid := strings.TrimSpace(config.GetAID())
	aidNote := ""
	if aid != "" && aid != "xxxxx" {
		aidNote = fmt.Sprintf(" (account %s)", aid)
	}
	return fmt.Errorf("this node already appears registered with RunOS%s: node ID %q in /etc/runos/config.yaml plus a valid mTLS client certificate in /etc/runos.\n\nRe-registering consumes a new token and may create a DUPLICATE node in the console. To deliberately re-provision a clean node:\n  sudo runos uninstall        # or: sudo rm -rf /etc/runos\nthen re-run the install command. If the existing config is for a different account than the one you are installing under, that confirms a stale install to wipe first.",
		aidNote, nid)
}

// stRawPath reads a viper path key without triggering config's
// create-empty-file side effects, falling back to a default.
func stRawPath(key, def string) string {
	v := strings.TrimSpace(viper.GetString(key))
	if v == "" {
		return def
	}
	return v
}

// stParseFirstCert PEM-decodes caBytes and returns the first CERTIFICATE block
// parsed as an x509 cert, or nil if none is found / it does not parse.
func stParseFirstCert(b []byte) *x509.Certificate {
	rest := b
	for {
		var block *pem.Block
		block, rest = pem.Decode(rest)
		if block == nil {
			return nil
		}
		if block.Type != "CERTIFICATE" {
			continue
		}
		cert, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			return nil
		}
		return cert
	}
}

// checkInstallLock blocks when another install/registration already holds the
// /run/runos-install.lock advisory lock, i.e. a concurrent or re-entrant run.
// Prevents two installers racing on /etc/runos, apt, and kubeadm at once (which
// produces corrupt config and half-applied package state). A stale lock left by a
// crashed installer clears on reboot (/run is tmpfs) or can be removed by hand.
func checkInstallLock() error {
	// O_CREATE so the file exists; we only TEST the lock here (non-blocking) and
	// release it immediately, so this probe itself never holds the install out.
	f, err := os.OpenFile(stInstallLockPath, os.O_CREATE|os.O_RDWR, 0644)
	if err != nil {
		// Cannot create the lock file (e.g. /run not writable in an odd sandbox).
		// That is not a confident "someone else is installing" -> do not block.
		roslog.W("could not open install lock file; skipping concurrent-install check", err, "path", stInstallLockPath)
		return nil
	}
	defer f.Close()

	if lerr := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); lerr != nil {
		// Lock is held by another process -> a concurrent install/register.
		return fmt.Errorf("another RunOS install/registration is already running on this node (the lock %s is held).\n\nWait for it to finish (watch the node in the RunOS console). If that process died and left a stale lock, remove it and re-run:\n  sudo rm -f %s\nDo not run the install command twice in parallel.",
			stInstallLockPath, stInstallLockPath)
	}
	// We got the lock; release it (we were only probing) and continue.
	_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	return nil
}

// checkImmutableTargetPaths blocks when a path RunOS must write is immutable
// (chattr +i) or append-only (+a), or when /usr/local/bin (where the binary
// lands) is not writable+executable. Prevents the baffling failure where even
// root cannot write config/certs/sysctl drop-ins/the binary on a hardened host,
// surfacing later as an opaque EPERM with no mention of the immutable attribute.
func checkImmutableTargetPaths() error {
	var bad []string

	// chattr/lsattr-based immutable/append-only detection on key dirs+files.
	targets := []string{
		"/etc",
		"/etc/runos",
		"/etc/fstab",
		"/etc/sysctl.d",
		"/etc/modules-load.d",
		"/etc/resolv.conf",
	}
	for _, t := range targets {
		if !stExists(t) {
			continue // absent target -> installer creates it; nothing to assert.
		}
		if flag := stImmutableFlag(t); flag != "" {
			bad = append(bad, fmt.Sprintf("%s (%s)", t, flag))
		}
	}

	// /usr/local/bin must be writable + executable for the agent binary.
	if msg := stBinDirNotUsable("/usr/local/bin"); msg != "" {
		bad = append(bad, msg)
	}

	if len(bad) > 0 {
		return fmt.Errorf("a target path RunOS must write is immutable/append-only or not writable+exec: %s.\n\nEven root cannot write the config, certificates, sysctl/module drop-ins, or the binary there. Clear the attribute or remount, then re-run:\n  sudo lsattr -d <path>           # inspect\n  sudo chattr -i <path>           # clear immutable\n  sudo chattr -a <path>           # clear append-only\n  # or remount /usr writable+exec if it is read-only/noexec\nImmutable /etc and read-only /usr are hardening measures that block installation.",
			strings.Join(bad, ", "))
	}
	return nil
}

// stImmutableFlag returns "immutable" or "append-only" if lsattr reports the i/a
// flag on path, else "". Missing lsattr or unparseable output => "" (we do not
// block on an uncertain result; a probe-write would be the fallback but lsattr is
// the reliable signal here).
func stImmutableFlag(path string) string {
	bin, err := exec.LookPath("lsattr")
	if err != nil {
		return ""
	}
	// -d so a directory is reported as itself, not its contents.
	out, err := stRunCmd(5*time.Second, bin, "-d", path)
	if err != nil {
		return ""
	}
	line := strings.TrimSpace(out)
	if line == "" {
		return ""
	}
	// lsattr output: "<flags>  <path>"; flags is the first field.
	fields := strings.Fields(line)
	if len(fields) == 0 {
		return ""
	}
	flags := fields[0]
	switch {
	case strings.Contains(flags, "i"):
		return "immutable"
	case strings.Contains(flags, "a"):
		return "append-only"
	}
	return ""
}

// stBinDirNotUsable returns a description if dir is not writable+executable (a
// probe create+remove fails, or the mount is read-only/noexec), else "". The dir
// is created if absent (the installer would create it too); if creation fails we
// report that. Bounded, local.
func stBinDirNotUsable(dir string) string {
	if !stExists(dir) {
		if err := os.MkdirAll(dir, 0755); err != nil {
			return fmt.Sprintf("%s is missing and cannot be created (%v)", dir, err)
		}
	}
	// statfs for read-only / noexec mount flags.
	var st syscall.Statfs_t
	if err := syscall.Statfs(dir, &st); err == nil {
		if st.Flags&stMntReadOnly != 0 {
			return fmt.Sprintf("%s is on a read-only mount", dir)
		}
		if st.Flags&stMntNoExec != 0 {
			return fmt.Sprintf("%s is on a noexec mount", dir)
		}
	}
	// Probe create+remove to catch immutable/EPERM the stat flags miss.
	probe := filepath.Join(dir, ".runos-preflight-probe")
	f, err := os.OpenFile(probe, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0755)
	if err != nil {
		if os.IsPermission(err) {
			return fmt.Sprintf("%s is not writable (root got EPERM; immutable or read-only)", dir)
		}
		// Other errors are ambiguous; do not over-report.
		return ""
	}
	_ = f.Close()
	_ = os.Remove(probe)
	return ""
}

// checkCloudInitComplete blocks when cloud-init is present but first-boot setup
// has not cleanly finished. Installing during cloud-init races a half-configured
// node (broken apt sources, no DNS, an un-grown disk, the wrong hostname), which
// then fails far downstream with errors that never mention cloud-init. Skipped
// entirely when cloud-init is absent (most non-cloud images).
func checkCloudInitComplete() error {
	bin, err := exec.LookPath("cloud-init")
	if err != nil {
		return nil // not a cloud image -> nothing to wait on.
	}
	// Require the runtime result file too; cloud-init can be installed but unused.
	if !stExists("/run/cloud-init/result.json") {
		return nil
	}

	// `cloud-init status --wait` blocks until done; bound it to ~180s so a stuck
	// first boot cannot hang preflight forever.
	out, runErr := stRunCmd(185*time.Second, bin, "status", "--wait")
	status := stParseCloudInitStatus(out)

	switch status {
	case "done":
		return nil
	case "error", "degraded":
		return fmt.Errorf("cloud-init first-boot setup FAILED (status: %s).\n\nInstalling now races a half-configured node (broken apt sources, no DNS, un-grown disk, wrong hostname). Inspect the failure:\n  cloud-init status --long\n  sudo less /var/log/cloud-init.log\nThe reliable fix is to delete this server and add a fresh one from a clean supported Ubuntu image, then re-run.", status)
	default:
		// Empty/unknown status after the bounded wait. If the wait itself errored
		// (timeout/kill), treat it as "still running past timeout" and block; the
		// install must not proceed onto an unfinished first boot. If we simply
		// could not parse a healthy status, be conservative and block with the
		// running-past-timeout guidance (a transient retry clears it).
		if runErr != nil || status == "running" || status == "" {
			return fmt.Errorf("cloud-init has not finished first-boot setup (it is still running past the wait timeout).\n\nInstalling now races a half-configured node. Wait 1-2 minutes and retry:\n  cloud-init status --long\nIf it stays stuck or shows an error, delete this server and add a fresh one from a clean supported Ubuntu image, then re-run.")
		}
		return nil
	}
}

// stParseCloudInitStatus extracts the status token from `cloud-init status`
// output (e.g. "status: done"). Returns lowercase status or "" if not found.
func stParseCloudInitStatus(out string) string {
	for _, line := range strings.Split(out, "\n") {
		l := strings.ToLower(strings.TrimSpace(line))
		if strings.HasPrefix(l, "status:") {
			return strings.TrimSpace(strings.TrimPrefix(l, "status:"))
		}
	}
	// Some versions print just the bare word.
	t := strings.ToLower(strings.TrimSpace(out))
	switch t {
	case "done", "error", "degraded", "running":
		return t
	}
	return ""
}

// checkL1SecCAFileValid validates the L1Sec registration CA at
// config.GetPublicCACertPath(). When the file is PRESENT it must be a real PEM
// certificate that is currently valid, and (if a pin is configured) must match
// the expected sha256 — a mismatch means a possible MITM of the CA download and
// is fatal. When the file is ABSENT it is treated as "installer has not placed it
// yet" and only warns (the full install command, curl | bash, fetches it), so a
// stand-alone preflight does not falsely block. Prevents a registration that
// either trusts a captive-portal/error-page "CA" or, worse, a substituted CA.
func checkL1SecCAFileValid() error {
	path := config.GetPublicCACertPath()
	fi, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			roslog.W("L1Sec registration CA not present yet; the full install command (curl ... | sudo bash) downloads it. Skipping CA validation.", nil, "path", path)
			return nil
		}
		// Present but unreadable: that IS a problem the installer will hit.
		return fmt.Errorf("the RunOS registration CA at %s exists but cannot be read (%v).\n\nFix its permissions/ownership (it must be root-readable), or re-run the full install command to re-fetch it:\n  curl -sSL <install-url> | sudo bash", path, err)
	}
	if fi.IsDir() {
		return fmt.Errorf("the RunOS registration CA path %s is a directory, not a certificate file.\n\nRemove it and re-run the full install command to re-fetch the CA:\n  sudo rm -rf %s\n  curl -sSL <install-url> | sudo bash", path, path)
	}
	if fi.Size() == 0 {
		return fmt.Errorf("the RunOS registration CA at %s is empty (a truncated or failed download).\n\nRe-run the full install command to re-fetch it; do not run 'runos register' on its own:\n  curl -sSL <install-url> | sudo bash", path)
	}

	caBytes, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("the RunOS registration CA at %s could not be read (%v).\n\nRe-run the full install command to re-fetch it:\n  curl -sSL <install-url> | sudo bash", path, err)
	}

	// Must be a usable PEM cert pool AND a parseable CERTIFICATE block.
	pool := x509.NewCertPool()
	cert := stParseFirstCert(caBytes)
	if !pool.AppendCertsFromPEM(caBytes) || cert == nil {
		return fmt.Errorf("the RunOS registration CA at %s is not a valid certificate (it looks like a proxy/captive-portal error page, an HTML body, or a truncated download).\n\nCheck egress to the CDN on 443 and any HTTP(S)_PROXY / TLS interception, then re-run the full install command to re-fetch it:\n  curl -sSL <install-url> | sudo bash", path)
	}

	now := time.Now()
	if now.Before(cert.NotBefore) {
		return fmt.Errorf("the RunOS registration CA at %s is not yet valid (NotBefore %s).\n\nThis is usually a wrong system clock. Fix the clock and re-run:\n  sudo timedatectl set-ntp true", path, cert.NotBefore.UTC().Format(time.RFC3339))
	}
	if now.After(cert.NotAfter) {
		return fmt.Errorf("the RunOS registration CA at %s has expired (NotAfter %s).\n\nRe-run the full install command to re-fetch a current CA; if it persists, your system clock may be wrong (sudo timedatectl set-ntp true):\n  curl -sSL <install-url> | sudo bash", path, cert.NotAfter.UTC().Format(time.RFC3339))
	}

	// Pin check (verifyL1SecCAPin moved earlier): a configured pin that does not
	// match is a possible MITM of the CA fetch -> fatal. No pin -> warn (unpinned
	// build), matching backend's fail policy.
	pin := stConfiguredCAPin()
	if pin != "" {
		sum := sha256.Sum256(caBytes)
		got := hex.EncodeToString(sum[:])
		if got != pin {
			return fmt.Errorf("the RunOS registration CA at %s does NOT match the expected fingerprint (possible MITM of the CA download).\n\nexpected sha256: %s\nactual   sha256: %s\nDo NOT proceed. Check for an HTTP(S)_PROXY / TLS-intercepting proxy on egress to the CDN, then re-run the full install command to re-fetch the genuine CA:\n  curl -sSL <install-url> | sudo bash", path, pin, got)
		}
	} else {
		roslog.W("L1Sec CA is not pinned (no mtls.public-ca-sha256 / build pin); validated as a PEM but not integrity-verified", nil, "path", path)
	}
	return nil
}

// stConfiguredCAPin mirrors backend.configuredL1SecCAPin for the config-supplied
// pin: lowercase, trimmed, "sha256:" prefix stripped. The build-time ldflags pin
// lives in the backend package (unexported) and is not visible here; preflight
// can only see the config-key pin, which is the operator-facing override.
func stConfiguredCAPin() string {
	pin := strings.TrimSpace(viper.GetString("mtls.public-ca-sha256"))
	return strings.TrimPrefix(strings.ToLower(pin), "sha256:")
}

// checkCurlAndBaseTooling blocks when any base tool the agent/installer shells
// out to is missing, reporting the FULL missing set at once. Runs before any
// network probe so a missing curl cannot masquerade as a network failure.
// Prevents the late, confusing "command not found" deep in the install script on
// minimal Ubuntu images that omit iproute2/kmod/gnupg/etc.
func checkCurlAndBaseTooling() error {
	// Binaries on PATH. modprobe lives in kmod; accept either name.
	type tool struct {
		bin string
		alt string // optional alternate binary that satisfies the same need
		pkg string
	}
	tools := []tool{
		{bin: "curl", pkg: "curl"},
		{bin: "ip", pkg: "iproute2"},
		{bin: "modprobe", alt: "kmod", pkg: "kmod"},
		{bin: "iptables", pkg: "iptables"},
		{bin: "ss", pkg: "iproute2"},
		{bin: "sed", pkg: "sed"},
		{bin: "swapoff", pkg: "util-linux"},
		{bin: "systemctl", pkg: "systemd"},
		{bin: "sha256sum", pkg: "coreutils"},
		{bin: "gpg", pkg: "gnupg"},
	}

	var missingBins []string
	pkgSet := map[string]bool{}
	for _, t := range tools {
		if stHaveBin(t.bin) {
			continue
		}
		if t.alt != "" && stHaveBin(t.alt) {
			continue
		}
		name := t.bin
		if t.alt != "" {
			name = t.bin + "/" + t.alt
		}
		missingBins = append(missingBins, name)
		if t.pkg != "" {
			pkgSet[t.pkg] = true
		}
	}

	// dpkg-managed packages that ship data (not a binary on PATH): ca-certificates,
	// gnupg. Only assert when dpkg is available; otherwise we cannot tell.
	var missingPkgs []string
	if stHaveBin("dpkg") {
		for _, p := range []string{"ca-certificates", "gnupg"} {
			if !stDpkgInstalled(p) {
				missingPkgs = append(missingPkgs, p)
				pkgSet[p] = true
			}
		}
	}

	if len(missingBins) == 0 && len(missingPkgs) == 0 {
		return nil
	}

	var parts []string
	if len(missingBins) > 0 {
		parts = append(parts, "commands: "+strings.Join(missingBins, ", "))
	}
	if len(missingPkgs) > 0 {
		parts = append(parts, "packages: "+strings.Join(missingPkgs, ", "))
	}
	// Build a single apt install line covering everything missing.
	var pkgs []string
	for p := range pkgSet {
		pkgs = append(pkgs, p)
	}
	stSortStrings(pkgs)
	installLine := "sudo apt-get update && sudo apt-get install -y " + strings.Join(pkgs, " ")

	return fmt.Errorf("required base tools are missing on this node (%s).\n\nMinimal Ubuntu images often omit these. Install them, then re-run:\n  %s\nThis is an environment prerequisite, not a RunOS error.",
		strings.Join(parts, "; "), installLine)
}

// stHaveBin reports whether name resolves on PATH.
func stHaveBin(name string) bool {
	_, err := exec.LookPath(name)
	return err == nil
}

// stDpkgInstalled reports whether `dpkg -s pkg` shows an installed status.
// Bounded; any error => treated as not-installed only when dpkg ran but the
// package is absent. A dpkg execution error returns false-positive-safe: we say
// "installed" so we never block on a dpkg quirk.
func stDpkgInstalled(pkg string) bool {
	out, err := stRunCmd(5*time.Second, "dpkg", "-s", pkg)
	if err != nil {
		// `dpkg -s` exits non-zero when the package is not installed. Distinguish
		// "not installed" (output mentions it) from a dpkg failure (treat as
		// installed to avoid a false block).
		low := strings.ToLower(out)
		if strings.Contains(low, "is not installed") || strings.Contains(low, "no packages found") {
			return false
		}
		return true
	}
	return strings.Contains(out, "Status:") && strings.Contains(out, "installed") && !strings.Contains(out, "not-installed")
}

// stSortStrings sorts s in place (small slices; avoids importing sort for one
// call but kept simple/correct).
func stSortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j-1] > s[j]; j-- {
			s[j-1], s[j] = s[j], s[j-1]
		}
	}
}
