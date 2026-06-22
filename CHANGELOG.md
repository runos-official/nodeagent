# Changelog

All notable changes to the RunOS node agent are documented here. The format
follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/) and the project
uses [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

The release pipeline extracts the section matching the pushed tag (`## vX.Y.Z`)
as the GitHub release notes, so every released version needs a section here.

## v1.5.4

### Fixed
- **Preflight clock-skew now self-heals instead of blocking automated provisions.**
  A freshly-provisioned cloud node often boots before its clock is NTP-synced; the
  clock-skew preflight check then BLOCKED with "sudo timedatectl set-ntp true" — fine
  for a manual install, but a dead end for automated cloud provisioning (add-server)
  where there is no operator to run it (and no console feedback). The check now enables
  NTP (`timedatectl set-ntp true`) and waits up to ~45s for sync before re-checking,
  failing only if the clock genuinely will not sync (e.g. NTP egress on UDP 123 blocked).

## v1.5.3

### Fixed
- **`runos uninstall` is now idempotent — it no longer wedges on a half-uninstalled
  node.** Two load-bearing steps failed on *every* retry once their targets were
  already gone: `kubeadm reset` exited 127 when kubeadm was absent, and `purge
  packages` exited 100 ("Unable to locate package") once the Kubernetes apt repo had
  been removed by a prior partial run, so apt could no longer resolve the names. Both
  now treat "already removed" as success: kubeadm reset is guarded by `command -v
  kubeadm`, and the purge targets only the packages dpkg still tracks. The `rm -rf`
  wipes now stop kubelet/containerd first, lazy-unmount any live pod-volume mounts
  under `/var/lib/kubelet`, and assert the target is actually gone, so a busy mount or
  immutable file can no longer turn a re-runnable cleanup into a permanent "partial
  uninstall". Net effect: `runos uninstall` succeeds (and clears `/etc/runos`, then
  reboots) regardless of how partially-uninstalled the node already was.

## v1.5.2

### Fixed
- **`runos uninstall` now actually removes `/etc/runos` (the node's identity).**
  The full uninstall wiped Kubernetes, WireGuard, packages and network but LEFT
  `/etc/runos/config.yaml` (the NID) and the mTLS client cert + CA behind, even
  though the function's contract said a full uninstall "clears RunOS
  configuration and certificates" (it was never implemented). So an uninstalled
  node still looked registered, and the NEXT install was correctly BLOCKED by the
  `already-registered` preflight check. Uninstall now removes `/etc/runos`, so
  uninstall -> reinstall works without a manual `sudo rm -rf /etc/runos`.

## v1.5.1

### Fixed
- **`runos update` no longer pipes a remote script into root bash.** The
  self-update previously did `curl <installer>/update | sudo bash`, trusting an
  unverified remote script as root (a root-RCE vector if the installer host were
  compromised). It now, entirely in Go: downloads the exact release binary
  (`nodeagent-linux-<arch>`) from GitHub Releases, verifies its sha256 against
  the release's `checksums.txt`, and only on match atomically swaps
  `/usr/local/bin/runos` (temp file + rename) and restarts the service. Fails
  closed with a clear message + non-zero exit on any download/checksum/restart
  error.

## v1.5.0

Structured node logs. Install-command failures now send a stable machine error
code + plain-language cause + remedy as structured fields (not just free text)
to Nodeward, so the console node page renders an actionable Cause / Try / code
block and support can match on a stable code.

### Added
- `commons.classifyCommandFailure` returns a stable `code` (NA_APT_LOCK,
  NA_DISK_FULL, NA_NET_UNREACH, NA_PKG_NOTFOUND, NA_HELD_PKGS, NA_PERMISSION,
  NA_KUBEADM, NA_CONTAINERD, NA_REPO_GPG, NA_GENERIC) alongside the cause+remedy.
- `backend.AddNodelogStructured` sends `code`/`cause`/`remedy`/`docs_url` on the
  L2SEC `AddNodelogRequest` (new proto fields 20/25/30/35); the plain
  `AddNodelog` wrapper is unchanged for existing callers. Back-compatible.

## v1.4.0

CLI quality pass: every `runos` subcommand audited (132 findings) and brought to
an enterprise bar — correct exit codes, no panics, single clean failure block,
consistent stdout/stderr, optional `--json`, and TTY-aware color. Validated live
on registered + unregistered nodes.

### Added
- `--json` output for `status`, `version`, `logs` (raw JSONL), `etcd list`,
  `kubeproxy list`; `runos --version`/`-v`.
- TTY/`NO_COLOR`-aware coloring: piped/redirected/CI/systemd output is now plain
  text automatically (no raw ANSI escapes); interactive terminals keep color.
- `Args: cobra.NoArgs` on commands that take no positionals, so a stray argument
  errors with a non-zero exit instead of being silently ignored.

### Fixed
- **No more panics on recoverable conditions.** `backend.NodewardL2Sec()` returns
  cert/key/CA load errors instead of `panic()`, so `agent`/`test`/`status`/
  `certificate renew` on an unregistered or half-installed node give a clear
  message and a non-zero exit rather than a Go stack trace.
- **Correct exit codes.** `status`, `sync vpn`, `etcd list/remove`, `kubeproxy
  list/refresh`, `update`, `uninstall`, `certificate renew`, `test`, `agent` were
  `Run` (always exit 0); converted to `RunE` so a real failure exits non-zero
  (CI/`&&` gating now works).
- **One failure block, not three.** Root sets `SilenceUsage`/`SilenceErrors` and
  failures route through a single `roslog.Fail` block (no duplicate generic line,
  no cobra usage dump).
- **Real bugs:** `register --control-plane` was parsed but ignored; `status`
  wrote a remote nodelog on every read; `uninstall` reported success even when
  destructive steps failed; `setconfig` couldn't create config on a fresh node;
  `etcd remove` had an else/Help() control-flow bug; `update`/`kubeproxy` reported
  success on no-op. All fixed. Diagnostics now go to stderr, data to stdout.

## v1.3.0

Install-flow robustness, round 2: catch almost everything in preflight before
anything is installed, and make every message plain-language and actionable so a
failed install reads as an unmet prerequisite, not a broken product. Validated
live on Ubuntu 24.04 (healthy node passes clean; induced failures each block with
a clear message).

### Added
- **Preflight expanded to 44 checks** with a collect-all runner: it reports
  EVERY blocking issue in one pass (instead of fail-fast, one-per-rerun), while
  fatal prerequisites still stop early. New coverage for the realistic ways an
  install fails before installing: HTTP-proxy vs direct-gRPC egress, high-port
  (9191/9192) vs 443 egress, the full HTTPS endpoint set (Go net/http, not curl),
  DNS answer sanity; systemd-as-init, /proc+/sys mounted, cgroup v2 + controllers,
  REAL kernel-module load (not dry-run), writable sysctls, Linux capabilities,
  container/WSL detection; /var space+inodes, mount exec/ro flags, swap
  persistence in fstab, RAM pressure, entropy, etcd fsync, ulimits; existing-k8s
  leftovers, already-registered, install lock, immutable target paths, cloud-init
  completion, L1Sec CA validity, base tooling; hostname validity+persistence,
  machine-id, resolv.conf/nsswitch, wireguard subnet overlap; apt usability,
  firewall posture, rp_filter.
- **Support line on every failure** (`roslog.SupportLine`): preflight, register
  and install failures now end with how to reach support@runos.com if it looks
  like a RunOS bug rather than an environment issue.

### Fixed
- **`register` no longer panics** on a missing/corrupt/unpinned L1Sec CA or a
  dial failure — it prints a clear `FAILED:` block and exits non-zero.
- **Customer-facing install-failure messages are plain language.** A failed
  install command used to push a raw `Command failed:/Error:/Output:` blob to the
  node's console page; it now shows a classified cause + `Try:` remedy (apt/dpkg
  lock, no space, DNS/network, package-not-found, held packages, permission,
  kubeadm preflight, containerd/CRI, GPG/repo), with the full raw detail kept in
  /var/log/runos.log.

## v1.2.0

Manual-install robustness: defensive preflight, honest failure reporting, clear
actionable errors, and Ubuntu 26.04 support. Validated by real installs on
Ubuntu 24.04 and 26.04.

### Added
- **Ubuntu 26.04 support.** Preflight now admits 22.04/24.04/**26.04** (was a
  hard block on anything but 22.04/24.04). A unified `/etc/os-release` parser
  (ID + VERSION_ID) replaces the two divergent parsers; a genuinely-unsupported
  OS fails with a message naming the detected OS + the supported set. Validated:
  a full install on Ubuntu 26.04 reaches a Ready control-plane node (k8s 1.35.4,
  containerd 2.2.2).
- **Preflight checks** with clear remedies: not-root, CPU arch (amd64/arm64),
  swap enabled, required ports in use (6443/10250/2379/2380/6446 + udp
  51820/8472), clock/NTP skew, and Nodeward host:port reachability (classifies
  DNS-fail vs refused vs firewall). Cheap/local checks run before network ones.

### Fixed
- **Honest failure reporting (the "install said success but the node never came
  up" bug).** The on-node installer now checks the exit code of every step
  (register + install were previously unchecked) and only prints the success
  banner if all passed; otherwise it prints a `FAILED: <step>` block and exits
  non-zero. The `install`/`register` cobra commands now exit non-zero on failure
  (were exit 0), `log.Fatalf`/`panic` on recoverable errors are replaced with a
  structured `FAILED: <step> — Cause — Try` block, and gRPC registration errors
  map to actionable messages (bad/expired token, bad --aid, Nodeward
  unreachable).
- **Register flag validation:** empty/missing `--token`/`--aid`/`--server` are
  rejected up front (an empty `--server` no longer silently persists an empty
  Nodeward host).

## v1.1.1

### Fixed
- `uninstall` no longer stalls for minutes. All package removals are now a single
  non-interactive `apt-get` (was five separate, lock-contending invocations), and
  every potentially-blocking step (kubeadm reset, systemctl, netplan, apt) is
  bounded by `timeout` so a wedged step can't hang the whole uninstall. Also
  removes the previously-missed `wireguard-tools`. Measured ~12s end to end on a
  control-plane node (was minutes).

### Security
- `runos uninstall` now requires `--yes` (or an interactive "yes" confirmation)
  before it wipes Kubernetes/etcd and reboots, so a bare invocation can't destroy
  a node by accident. The nodeward `UNINSTALL_NODE` instruction path is
  unaffected.

## v1.1.0

Security hardening pass (file permissions, secret logging, instruction-handler
input validation, transport trust, and connection resilience). No on-wire
protocol change.

### Security
- The mTLS private key (`/etc/runos/mtls.key`) and the agent log
  (`/var/log/runos.log`) are now created `0600` (were world-readable `0644`).
  The key is also re-tightened to `0600` on every agent startup, so already
  deployed nodes are remediated on the next restart.
- Removed cleartext logging of certificate/key PEM material; command and script
  logging now redacts secret-bearing values (`PASSWORD=` / `TOKEN=` / ...).
- `RUN_REMOTE_SCRIPT` no longer builds `curl … | bash`: the script id/path is
  validated and the fetched script runs argv-style (no shell string).
- `RUN_WEB_REQUEST` blocks loopback / link-local / cloud-metadata targets
  (dialing the resolved IP to defeat DNS-rebinding) and ignores caller-supplied
  TLS-skip.
- `REINSTALL_NODE` writes its command to a root-only `0600` script rather than
  interpolating it into a systemd unit.
- `UPDATE_DNSMASQ` (directive allow/deny-list) and `INSTALL_HELM_CHART`
  (https-only, internal-IP block, name validation) now validate their inputs.
- The L1Sec public CA is verified against a pinned SHA256 (set at release;
  warn-only until set). TLS minimum version raised to 1.2.

### Changed
- The agent now reconnects in-process with capped exponential backoff instead of
  exiting on a transient stream/connection error and relying on a systemd
  restart, so network blips no longer cause full process restarts (and a re-run
  of VPN sync). Dial is bounded by a timeout.

## v1.0.0

First public release of the RunOS node agent.

- Source-available under the Elastic License 2.0.
- Published as attested `linux/amd64` + `linux/arm64` binaries on GitHub
  Releases, built by GitHub Actions on a `v*` tag with a keyless Sigstore
  build-provenance attestation and a `checksums.txt`. The installer downloads the
  exact release the control plane selects and verifies its checksum before
  installing.
- Pre-release tags (`-rc.N`) publish a hidden release candidate: pushed and
  pinnable by exact version, and excluded from the "Latest release" pointer.
- Verify a released binary with:
  `gh attestation verify nodeagent-linux-amd64 --repo runos-official/nodeagent`.

## v0.23.17

Baseline of the public release pipeline.

- Distribution moves to GitHub Releases: each `v*` tag publishes raw linux
  binaries `nodeagent-linux-amd64` and `nodeagent-linux-arm64` (built
  `CGO_ENABLED=0`, `-trimpath`, stripped) plus a `checksums.txt`. The on-node
  installer downloads the release asset directly, so a release is the deploy of
  the artifact.
- Build and version are now driven by the git tag: `version.Version` is injected
  at build time via `-ldflags` (it defaults to `dev` for local builds) instead
  of being hardcoded.
- Releases carry a keyless Sigstore build-provenance attestation bound to each
  binary's sha256, verifiable with
  `gh attestation verify nodeagent-linux-amd64 --repo runos-official/nodeagent`.
- Pre-release tags (`-rc.N`) publish a hidden release candidate: the binaries are
  pushed and pinnable by exact version, but the release is flagged a GitHub
  prerelease and excluded from the "Latest release" pointer, so the fleet keeps
  tracking the latest stable while testers opt in by pinning the exact version.
- `runos update` gains a `--version` flag to pin an exact release tag; without it,
  the agent updates to the advertised version as before.
- Rolling the live fleet stays gated downstream (foreman advertises
  `NODE_AGENT_VERSION`; a per-cluster pin is applied via conductor); publishing a
  release does not roll the fleet.
