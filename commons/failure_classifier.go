package commons

import "strings"

// Stable machine error codes returned by classifyCommandFailure. They are wire
// values (sent in the structured nodelog `code` field) and consumed by the
// console / nodeward, so keep them stable: add new ones, never rename.
const (
	codeAptLock    = "NA_APT_LOCK"
	codeDiskFull   = "NA_DISK_FULL"
	codeNetUnreach = "NA_NET_UNREACH"
	codePkgNotFnd  = "NA_PKG_NOTFOUND"
	codeHeldPkgs   = "NA_HELD_PKGS"
	codePermission = "NA_PERMISSION"
	codeKubeadm    = "NA_KUBEADM"
	codeContainerd = "NA_CONTAINERD"
	codeRepoGPG    = "NA_REPO_GPG"
	codeGeneric    = "NA_GENERIC"
)

// classifyCommandFailure maps a failed install command's output to a stable
// machine error code, a plain-language cause and a short "Try:" remedy, so the
// console node page shows the customer something actionable instead of a raw
// stderr dump (and tooling can branch on the code).
//
// step is a human label for what was being attempted (here, the command that
// failed); command is the failing command line (already secret-redacted by the
// caller); output is the command's combined stdout+stderr.
//
// Matching is case-insensitive substring matching against output, in priority
// order (most specific / most common operator pain first). It never panics and
// never returns an empty code or cause: when nothing matches, or output is
// empty, it falls back to NA_GENERIC with a step-named message that points at
// the full log.
func classifyCommandFailure(step, command, output string) (code string, cause string, remedy string) {
	lower := strings.ToLower(output)

	// contains reports whether any of the given signatures appears in output
	// (case-insensitive). Signatures are passed pre-lowercased.
	contains := func(sigs ...string) bool {
		for _, s := range sigs {
			if s != "" && strings.Contains(lower, s) {
				return true
			}
		}
		return false
	}

	switch {
	// --- apt/dpkg lock held ---------------------------------------------
	case contains(
		"could not get lock",
		"dpkg frontend lock",
		"unable to acquire the dpkg",
		"unable to lock the administration directory",
		"unable to lock directory",
	):
		return codeAptLock,
			"Another package manager (apt, dpkg, or unattended-upgrades) is holding the system package lock, so the install could not proceed.",
			"wait a minute for the other process to finish, or run `sudo lsof /var/lib/dpkg/lock-frontend` to find it, then retry the install. Disabling unattended-upgrades during install also helps."

	// --- no space left on device ----------------------------------------
	case contains("no space left on device", "write error: no space"):
		return codeDiskFull,
			"The node ran out of disk space during installation.",
			"free up disk on the node (`df -h` to see what is full, clear `/var/cache/apt` or old logs), then retry the install."

	// --- DNS / network unreachable --------------------------------------
	case contains(
		"temporary failure resolving",
		"could not resolve host",
		"name or service not known",
		"connection timed out",
		"failed to connect",
		"network is unreachable",
		"no route to host",
	):
		return codeNetUnreach,
			"The node could not reach the network to download packages or contact RunOS (DNS resolution or an outbound connection failed).",
			"check the node's DNS and outbound internet access (`ping 1.1.1.1`, `nslookup get.runos.com`), confirm any firewall/proxy allows the install endpoints, then retry."

	// --- package not found ----------------------------------------------
	case contains(
		"unable to locate package",
		"has no installation candidate",
		"was not found", // covers: Version '...' for '...' was not found
		"no candidate version",
	):
		return codePkgNotFnd,
			"A required package or package version is not available from the node's configured apt repositories.",
			"run `sudo apt-get update` to refresh the package lists, confirm the right repositories (and OS version) are enabled, then retry the install."

	// --- held / conflicting / broken packages ---------------------------
	case contains(
		"held packages",
		"held broken packages",
		"broken packages",
		"unmet dependencies",
		"following packages have unmet dependencies",
	):
		return codeHeldPkgs,
			"The install was blocked by held, broken, or conflicting packages already on the node.",
			"run `sudo apt-get -f install` to repair dependencies and `sudo apt-mark showhold` to find held packages, resolve them, then retry the install."

	// --- permission denied ----------------------------------------------
	case contains("permission denied", "operation not permitted", "must be run as root", "are you root"):
		return codePermission,
			"The install command did not have the privileges it needed (permission denied / not running as root).",
			"ensure the node agent runs as root (or with sudo), check file ownership/permissions and any AppArmor/SELinux policy, then retry."

	// --- kubeadm preflight / init / join --------------------------------
	case contains(
		"[preflight] some fatal errors occurred",
		"error execution phase",
		"couldn't validate the identity of the api server",
		"unhealthy", // covers: etcd ... unhealthy
		"context deadline exceeded",
		"could not initialize a kubernetes cluster",
		"failed to bootstrap",
	):
		return codeKubeadm,
			"Kubernetes setup (kubeadm) failed during a preflight check or an init/join phase, so the node did not join the cluster.",
			"review the kubeadm error above; common causes are swap still enabled, a port already in use, a clock skew, or the control plane being unreachable. Fix the flagged item and retry the install."

	// --- containerd / CRI ------------------------------------------------
	case contains(
		"failed to pull image",
		"failed to resolve reference",
		"image pull",
		"cri socket",
		"/run/containerd/containerd.sock",
		"connect: connection refused", // CRI socket not up
		"failed to get sandbox image",
	):
		return codeContainerd,
			"The container runtime (containerd/CRI) could not pull an image or was not reachable, so workloads could not start.",
			"confirm containerd is running (`systemctl status containerd`) and the node can reach the image registry, then retry the install."

	// --- GPG / repo signing / Release file ------------------------------
	case contains(
		"no_pubkey",
		"is not signed",
		"does not have a release file",
		"the following signatures couldn't be verified",
		"gpg error",
	):
		return codeRepoGPG,
			"An apt repository is misconfigured or its signing key is missing, so package metadata could not be trusted.",
			"import the missing repository GPG key (the NO_PUBKEY id above) or fix the offending source in `/etc/apt/sources.list*`, run `sudo apt-get update`, then retry."

	// --- generic fallback -----------------------------------------------
	default:
		return codeGeneric,
			genericCause(step),
			"review the full command output in /var/log/runos.log on the node to see why it failed, fix the underlying issue, then retry the install."
	}
}

// genericCause builds the no-match cause line, naming the failing step so the
// customer at least knows which command broke. Never returns an empty string.
func genericCause(step string) string {
	step = strings.TrimSpace(step)
	if step == "" {
		return "An installation command failed. See the full log on the node for details."
	}
	return "An installation command failed: " + step + ". See the full log on the node for details."
}
