# Changelog

All notable changes to the RunOS node agent are documented here. The format
follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/) and the project
uses [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

The release pipeline extracts the section matching the pushed tag (`## vX.Y.Z`)
as the GitHub release notes, so every released version needs a section here.

## v1.8.0-rc.5

### Changed

- **The join-time clash check moved to the control plane, and the range is now chosen to avoid the
  machine.** `runos preflight --cluster-cidr`, added one release ago in v1.8.0-rc.4, is REMOVED. It
  was the wrong shape: it carried a fact from the authority through the install command, and a
  machine checking itself can only ever refuse.

  The node now reports the networks it already has (`uc/hostnet`: every interface address and route
  destination, excluding RunOS's own wg0, Cilium and CNI links) as part of registering. The control
  plane does the rest. For a cluster's FIRST node those networks decide which overlay range is
  handed out, so the first machine in a cluster can no longer collide with its own cluster at all.
  For a later node they are compared against the range the cluster already holds, and a collision
  refuses the registration before anything is installed. See ADR-0003.

  RunOS's own interfaces are excluded deliberately. wg0 holds an address inside the cluster's
  overlay range, so reporting it would make every REINSTALL of an existing node look like a
  collision with its own cluster.

  Preflight keeps the two ranges that really are identical on every cluster, the Kubernetes pod
  range `172.25.0.0/16` and service range `10.96.0.0/12`, because checking those needs no facts
  from anywhere. It no longer guards `172.24.0.0/16`, which no cluster created since the address
  pool landed has used.

  An older agent sends no networks, and the control plane then chooses a range blind, exactly as
  before. Nothing that joins today stops joining.

## v1.8.0-rc.4

### Fixed

- **The join-time clash check guards the range the cluster actually holds.** Preflight refuses a
  node whose own LAN or routes collide with a range RunOS allocates from, because that collision
  surfaces only AFTER the install, as duplicate routes that break the node mesh, the CNI or
  service routing.

  The guarded list named `172.24.0.0/16`, which was right while every cluster overlay came out of
  that block. Ranges are drawn at random from the whole of RFC1918 since the address pool landed,
  so the constant was wrong in both directions at once. It refused a host on `172.24.x`, which the
  pool excludes and only legacy clusters use. And it could not refuse a host whose LAN is the
  range the cluster actually got, which is the collision the check exists to prevent.

  `runos preflight --cluster-cidr <cidr>` now takes the cluster's own range from the control
  plane and guards that instead. It is strictly narrower: a legacy cluster passes its
  `172.24.<octet>.0/24` and the rest of the /16, which belongs to other clusters, stops being
  this node's problem.

  Omitting the flag keeps the exact behaviour of every previous release, so a node that joins
  today cannot start failing. The installer probes for the flag before passing it, so a cluster
  pinned to an older agent is unaffected.

## v1.8.0-rc.3

### Fixed

- **Multus is recognised as RunOS's own CNI plugin, rather than by luck.** The `existing-k8s`
  preflight blocks an install when it finds a CNI config it does not recognise, and that block is
  deliberately EARLIER than the wipe that would have cleared it. So a config RunOS put there
  itself has to be recognised, or a node becomes uninstallable by its own leftovers.

  Recognition depended on the word `cilium` appearing somewhere in the file. For Multus that was
  luck: Multus in auto mode writes `00-multus.conf` by embedding the existing default CNI config
  as its delegate, and that embedded copy normally mentions Cilium. A change in Multus's generated
  format would have made every node that had VM networking uninstallable, and the failure would
  present as a refusal to start rather than as anything resembling a bug.

  Multus is now named outright, so recognition no longer depends on what a third-party binary
  happens to write. A planted foreign CNI config is still foreign, which is the case the block
  exists for.

## v1.8.0-rc.2

### Added

- **A peer may now have no endpoint.** Peer configuration required a valid endpoint IP and always
  emitted `endpoint <ip>:51820`. A node behind NAT with no inbound path has no endpoint anyone can
  dial, so such a peer was left out of the peer set ENTIRELY, its public key included. The far
  side had therefore never heard of it, and rejected its opening packet instead of authenticating
  it.

  An empty endpoint now configures identity only: the key and allowed IP are applied and no
  endpoint argument is emitted at all. The NAT'd node dials out, the far side recognises the key,
  and WireGuard learns the endpoint from the authenticated traffic. The existing 5 second
  keepalive is what then holds the NAT mapping open.

  This is not a relaxation of validation. A supplied endpoint is still checked exactly as before,
  so nothing untrusted reaches the command line either way.

### Fixed

- **The peer set now converges to exactly what was sent.** The update was purely additive: it
  configured the peers it was given and removed nothing. Two consequences, both permanent and
  both silent. A retired node stayed a peer, so a machine removed from the cluster kept a working
  key. And a peer once given the wrong endpoint kept it forever, because there was no way to say
  "no endpoint". A declaration you can toggle one way but not back is not a declaration.

  Clearing an endpoint is done by removing the peer and adding it back, because WireGuard has no
  command that unsets one: `wg set` can overwrite an endpoint, never remove it. A peer that is
  already endpointless is left alone rather than churned, so a live session is not dropped for
  nothing.

  Failing to read the current peers plans NO removals. Treating "could not read" as "no peers are
  configured" would tear down every working tunnel on the node, so the unknown case fails toward
  leaving things alone.

## v1.8.0-rc.1

### Removed

- **The cluster VIP is gone.** Nothing ever connected to it. The address was assigned to `wg0` on
  an elected node and failed over carefully between nodes, and a search of every RunOS repo found
  no consumer: conductor had no reference to it at all, and the place a cluster VIP would normally
  be used, the Kubernetes API endpoint, is reached through DNS and an haproxy on every node
  instead.

  Keeping it was not free, which is what decided it. It was the only thing in the agent that
  assumed a node's address begins `172.24`, and therefore the only reason changing a cluster's
  address range required a fleet update first. It also carried a live defect: the VIP pinned host
  `.254` while nodes are allocated `1..254` inclusive, so the 254th node in a cluster would have
  taken its address. Removing the feature makes that defect cease to exist, and all 254 host
  addresses are now genuinely usable.

  Gone with it: the VIP package, the assign and release instruction handlers, the holder query on
  connect, and the self-drop after repeated heartbeat failures. Nodeward answers "not the holder"
  to any agent that still asks, so a node running an older agent releases the address by itself
  rather than being left holding a stale one.

### Fixed

- **Preflight stopped blocking installs on a network that was fine.** The egress check built its
  own HTTP client, and setting `DialContext` on a Go transport switches OFF the automatic HTTP/2
  upgrade, so preflight spoke HTTP/1.1 while every other tool on the box spoke HTTP/2. GitHub
  drops HTTP/1.1 from some hosts and returns an empty reply, which Go surfaces as `EOF`, so the
  check reported

      BLOCKED [egress-endpoints]: Cannot reach required HTTPS endpoint(s) on 443:
        - github.com (the node binary release): Get "https://github.com/": EOF

  while `curl https://github.com/` on the same machine returned 200 five times out of five, and
  the remediation text told the operator to open a firewall that was already open. The client now
  sets `ForceAttemptHTTP2`, so it matches real egress behaviour, which is the only reason this
  client exists.

  A single dropped connection also used to block an install outright. These hosts demonstrably
  drop connections intermittently, so a verdict that severe now takes three attempts before it is
  believed. (Goal 23, F28.)

### Changed

- **A node whose LAN collides with the pod or service range is now refused at join.** The
  join-time conflict check guarded only the WireGuard overlay ranges, so a node sitting in
  `172.25.0.0/16` (pod) or `10.96.0.0/12` (service) passed preflight and then collided AFTER
  install, when Cilium or kube-proxy claimed the same addresses. Both ranges are hardcoded on
  every cluster and neither was guarded. Finding out after install costs a dirty machine and a
  manual clean; finding out at join costs a sentence. The check is renamed `reserved-subnets`
  because it no longer covers WireGuard alone.

  The message now names the range AND what uses it, built from the guarded list rather than
  written out in prose, so adding a range can never leave the message describing a set it no
  longer guards. The most specific range wins, so an address in the wg1 `/21` is reported as
  the `/21` rather than the `/16` that contains it, which used to send an operator to the
  wrong remedy.

  Two things are deliberately NOT reported. A LAN that merely CONTAINS a reserved range (a
  provider `/8`, which is routine) is left alone, because RunOS installs more specific routes
  and longest-prefix match sends the traffic the right way; flagging it would refuse nodes
  that work. And RunOS's own CNI interfaces are skipped alongside the WireGuard ones, because
  `cilium_host` holds a pod-range address that survives `kubeadm reset` until the node
  reboots, so counting it would block a re-install on state RunOS itself created.

  Verified on bare metal against a live cluster: a pod-range and a service-range interface are
  each blocked with the interface named, a `10.0.0.5/8` interface is not, and a running node
  whose `cilium_host` holds `172.25.0.203/32` reports no conflict.

- **The check says so when it could not read the host's addresses or routes.** Both readers
  failed open and silently, so a missing or too-old `ip` command produced a pass that read as
  "no conflict" and proved nothing.

## v1.7.2

### Fixed

- **Two control-plane nodes can join at the same time.** `kubeadm init phase upload-certs
  --upload-certs` generates a FRESH key on every call and re-encrypts the shared
  `kubeadm-certs` Secret with it, so a second control-plane join starting while the first
  was still downloading invalidated the key the first had been handed:

      error execution phase control-plane-prepare/download-certs: error downloading certs:
        error decoding secret data with provided key: cipher: message authentication failed

  Reproduced twice on bare metal, forty seconds apart on the second occasion. The cost was
  worse than one failed install: the failed attempt left the box dirty AND created a
  duplicate node record, so the retry was then blocked by preflight until the machine was
  cleaned by hand. The upload now passes an explicit `--certificate-key`, cached for an hour
  (well inside kubeadm's own two-hour expiry on the Secret), so two joins are handed the same
  key and the second upload re-encrypts with the key the first is already using. That removes
  the race rather than narrowing its window, which is all a check can do.

## v1.7.1

### Fixed

- **Preflight no longer calls a dead DNS resolver a firewall block.** On a node that had
  belonged to a RunOS cluster, the egress check reported eight endpoints as
  `timed out (likely firewall/proxy block)` and told the operator their allowlist was
  missing entries. Nothing was blocked: `curl https://github.com --resolve
  github.com:443:140.82.121.4` returned 200 in 0.070s and the same URL with DNS returned
  200 in 5.078s, because `/etc/systemd/resolved.conf` still carried the WireGuard address
  of a dnsmasq belonging to a deleted cluster. Every lookup waited out the dead server
  before falling back, and eight endpoints times five seconds blew the check's budget. The
  check now resolves each host first and MEASURES it, so it can tell a name that did not
  resolve from a resolver that is merely slow from a connection that really was blocked,
  and when DNS is the cause on every failing host it says so and points at the file on the
  machine rather than at a network team. RunOS writes that line itself and nothing removes
  it, so the operator most likely to meet this is the bare-metal customer reusing their own
  hardware.

- **Uninstall releases the CSI block mounts before `kubeadm reset`.** `kubeadm reset` cannot
  unmount the CSI volumeDevice bind mounts a virtual-machine disk leaves behind, so an
  uninstall failed on ANY node that had ever hosted a VM. Deterministic, measured on four
  hosts. The uninstall then correctly refused to reboot, and the node was left with
  kube-apiserver still LISTENING on 6443 and cilium-agent still running, because stopping
  containerd does not stop its reparented shim children. The kubelet is now stopped, DRBD
  devices are dropped and everything under `/var/lib/kubelet` is lazy-unmounted first.

- **Both remedies for a stuck port now say to reboot.** The `ports-free` preflight told the
  operator to run `sudo kubeadm reset -f`, which is the step that had already failed and
  which cannot kill an orphaned process or remove an interface a live cilium-agent
  recreates. The partial-uninstall remedy said to re-run the uninstall, which cannot clear
  it either. Only a reboot does, and neither message suggested one.

## v1.7.0

### Added

- Remote `RESTART_SERVICE` instruction. Nodeward (driven by the console/CLI) can
  restart the runos agent on a node. The handler acknowledges first, then
  schedules the restart as a detached transient systemd unit (`systemd-run`) so
  the ack flushes over the instruction stream before the process exits, and the
  unit comes back under `Restart=always`. Recovers a node whose VPN/network is
  stuck (e.g. after a wake-from-hibernate) without SSH.

### Fixed

- Wake-from-hibernate VPN recovery. The WireGuard peer sync now runs on every
  Nodeward (re)connect plus a slow periodic self-heal ticker, instead of a
  one-shot at startup. Previously a node that booted before its network was
  routable ran the startup sync too early, failed silently, and left its peer
  table empty/stale until a manual `systemctl restart runos`.
- systemd ordering: the unit now waits for `network-online.target` (was
  `network.target`), which fires before the NIC has a routable address. The
  updater migrates already-installed units.
- systemd start-rate limiting disabled (`StartLimitIntervalSec=0`) so an explicit
  or self-restart cannot trip `DefaultStartLimitBurst` and leave the unit failed
  ("start request repeated too quickly").

## v1.6.1

### Fixed
- **The apt-sources preflight check no longer hard-blocks an install on a
  transient mirror-probe timeout.** The mirror-root HEAD probe was a single
  6-second shot that also followed redirects, so on a fresh node whose
  networking was still converging (or whose mirror root 301s to a host apt
  never contacts, like security.ubuntu.com to www.ubuntu.com) it could time
  out and abort the install even though apt itself was healthy. The probe now
  treats any HTTP response (including a redirect, without following it) as
  proof of reachability and no longer blocks on its own: the real
  `apt-get update` (now run under its own generous 75s budget instead of the
  generic 8s exec cap) is the decider. Genuinely broken apt egress still
  blocks, now with accurate attribution listing the unreachable mirrors, and
  a probe blip that apt survives is logged as transient and the install
  proceeds.

## v1.6.0

`runos update` with no `--version` now updates to the version advertised by the
control plane. It resolves the exact tag conductor advertises for the node's
account (`GET {conductor}/{aid}/node-agent-version`) and installs that, verified
against the release `checksums.txt` exactly like an explicit `--version`. It is
never a floating "latest": if the advertised version cannot be resolved (no
conductor URL, network/HTTP error, empty or unparseable body) the update fails
closed rather than guessing.

Previously a bare `runos update` errored with "no target version was provided"
while the command's own help promised advertised-resolution it did not implement;
both are now fixed. Passing an explicit `runos update --version vX.Y.Z` is
unchanged. The conductor URL is taken from `client.server.conductor`, or derived
`get.` -> `api.` from the installer URL, so this works on existing node configs
that have no explicit conductor entry.

## v1.5.5

### Fixed
- **Deleting the control-plane node that holds etcd leadership no longer orphans its
  etcd member.** etcd refuses to cleanly remove the member that is the current raft
  leader, so the node-delete abandon phase's `member remove` silently no-op'd and left
  a permanent orphan voting member (quorum stayed inflated, so the cluster tolerated
  fewer real failures, and an operator had to remove the member by hand). Reproduced
  live 2/2 when deleting the leader, 0/2 on non-leaders. `RemoveEtcdMemberDirect` now
  detects when the target is the current leader and transfers raft leadership to a
  healthy, started, non-learner survivor first (`etcdctl move-leader`, directed at the
  leader's own client endpoint, since move-leader must hit the current leader), waits
  for leadership to settle off the target (~10s), then removes it against the local
  endpoint as before. If no eligible transferee exists or leadership does not move, it
  errors WITHOUT removing rather than orphaning, so the caller retries/aborts.
  Non-leader removals (live or already-dead) are unchanged.

### Removed
- **Removed the `REINSTALL_NODE` whole-node reinstall handler.** The whole-node
  reinstall feature was killed; its instruction handler and stream wiring are gone.

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
