# Changelog

All notable changes to the RunOS node agent are documented here. The format
follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/) and the project
uses [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

The release pipeline extracts the section matching the pushed tag (`## vX.Y.Z`)
as the GitHub release notes, so every released version needs a section here.

## Unreleased

## v1.8.0-rc.31

### Added

- **Interactive sessions, the agent half** (goal 31). The agent can now hold a live two-way session
  and stream it to a far end, which is what lets a terminal work on a machine with no inbound path
  and no public DNS record. Nothing sends a session frame yet, so this release changes no behaviour
  on any node: it is shipped alone, and early, precisely so that anything it does disturb is
  attributable to it rather than to a bundle.

  **A SESSION IS NOT AN INSTRUCTION, and that is the design.** An instruction is request-and-reply
  and returns; a session is open for as long as somebody is typing. Handing one to the worker pool
  would hold one of five workers for the life of a terminal, so five open terminals would leave a
  node unable to apply a manifest, run a script or report its status. No value of `numWorkers`
  fixes that. Session frames are therefore dispatched on the RECEIVER goroutine, beside
  `HandleInstructionAsResponse` and before the pool, and never touch the instruction channel.

  **Bounded at 16 and refused rather than queued**, with a reason naming the limit. A queued
  terminal is a terminal that appears to hang. The slot is reserved before the far end is dialled,
  so two opens arriving together cannot both pass the check, and it is returned when a dial fails,
  or a node would lose a slot per failure and end up refusing everything while holding nothing.

  **The l2sec contract is unchanged**, deliberately. Session frames ride the `ToNodeAgent` message
  that already exists: `type` names the frame, `tag` carries the session id, `jsonB64` carries the
  payload. That proto is hand-synced between nodeward and this repo with no gate over it, and proto
  drift is the coordination hazard this goal names as its worst.

  Output leaves on `SendBulkToNodeward`, so a terminal printing flat out gives way to anything the
  node owes the control plane. The send is synchronous, so a far end producing faster than the
  stream drains is made to wait: that is the backpressure, and nothing is ever dropped, because a
  console that silently loses bytes is worse than one that stutters.

- **The web-terminal far end.** A plain websocket to a Service inside the cluster, dialled from the
  node so it resolves through the cluster's own DNS. Nothing about this path leaves the cluster.

  Built by reading the terminal server's own source rather than assuming it behaves like ttyd, and
  two details would have been wrong by assumption. The client must offer TWO subprotocols: the one
  carrying the pre-shared key, which the server deliberately never selects because a selection is
  echoed in a response header, and the plain one it does select. And a message over 1 MiB is
  DROPPED by the far end with only a server-side log line, so input is chunked here rather than
  handed over whole and lost silently.

  A resize is a JSON object carrying `cols` and `rows` and nothing else, because the far end tells
  a resize from input by parsing every message as JSON. A zero dimension is never sent, since a
  refused resize falls through that parser into the PTY. The key rides a header and never a query
  string, which is the far end's own rule: a token in a URL is written into the ingress access log,
  the browser's history and any `Referer` a page leaks.

## v1.8.0-rc.30

### Fixed

- **A freshly installed node no longer finishes DEGRADED** (G28-F2). `wg-quick@wg0` was left in the
  `failed` state on every newly installed node, so `systemctl is-system-running` reported
  `degraded` until that node's first reboot. Measured 2026-08-19 on three nodes; two others that
  had rebooted were clean, which is the tell. The install brought `wg0` up by hand and only
  ENABLED the unit, so systemd never started it; `dnsmasq` and `runos-clear-link-dns` both carry
  `Wants=wg-quick@wg0.service`, their starts pulled the unit, and its `ExecStart` ran against an
  interface that already existed, which `wg-quick` refuses.

  Two halves here, the third is in nodeward. The unit's `ExecStart` is now guarded, so a pull after
  the interface exists is a no-op instead of a failure; the boot path is unchanged, because at boot
  `wg0` does not exist and `wg-quick` brings it up exactly as before. And `EnsureWg0BootOrder` now
  repairs a node ALREADY in the fleet, which nothing re-installs: `reset-failed`, `daemon-reload`,
  then `start`. The reload is unconditional, because an earlier run may have written the guarded
  unit and had its own reload fail; without it the repair would start the stale unit systemd still
  holds and fail again, leaving the node degraded through every agent start.

- **Preflight no longer blocks an install on an aligned supernet of a reserved range.**
  `idRouteOverlap` documented an "at least as specific" rule and did not implement it: it compared
  only the destination's base address, so a route to `10.96.0.0/11` was reported as overlapping the
  service range `10.96.0.0/12` and BLOCKED the install, although a /11 is less specific and loses
  longest-prefix match against RunOS's own routes. That is the one destination in all of IPv4 that
  hits the case.

### Added

- **Control traffic gets priority over session data on the node's outbound stream** (goal 31,
  starvation design decision 3). Every message a node sends takes one mutex, so a terminal
  producing output as fast as it can would compete with every status reply the node owes the
  control plane. Two classes only: control is everything the agent sends today, bulk is session
  data, and control never waits behind bulk. Bulk yields for at most 250 ms and then sends, because
  a console that never prints is as broken as one that starves the control plane. `SendToNodeward`
  keeps exactly today's synchronous behaviour and only gains a counter around the lock wait.
  Nothing calls `SendBulkToNodeward` yet; the session handling that will is the next piece.

### Changed

- **A mutex parameter that guarded nothing is gone** (no behaviour change). `stream.go` created a
  local `streamMutex`, passed a pointer to every worker, and no worker ever used it: every response
  goes out through `SendToNodeward`, which takes the package-level mutex in `outbound.go`. The
  parameter read as the lock guarding the send path while the real one sat in another file, which
  is an expensive false lead when the send path's serialisation is the fact you are grounding.

- **The `nat-collision` warning names the remedy that actually works.** It offered only "a distinct
  routable IP, or a distinct inbound UDP 51820 port-forward per node", and never mentioned the
  declared-network model, which is what two RunOS nodes behind one public address actually use and
  what goal 28 proved twice on hardware. It now prints the commands, with the node's own private
  address filled in. Every printed command was run against the real CLI before shipping.
  `--no-public-ingress` is gated on the node having no inbound path at all, and the text says
  plainly that it also removes the node from the cluster's public DNS record.

## v1.8.0-rc.29

### Fixed

- **The uninstall removes the firewall dispatch's SHADOW jump too** (goal 30 unit-8 review). A
  rename that failed mid-swap leaves `-j RUNOS-VMFW-N` in FORWARD, and the chain's `-X` then
  refuses because it is still referenced, so an uninstalled node kept a live customer-firewall
  chain. Both jumps are removed now.

## v1.8.0-rc.28

### Fixed

- **The uninstall removes the per-VM firewall chains too** (goal 30, customer-firewall-rules).
  Conductor's 076-vm-group-bridge applier now also builds `RUNOS-VMFW` in filter FORWARD and one
  `RUNOS-VMFW-<vmid>-IN` / `-OUT` chain per machine that has rules; the uninstall removes the
  FORWARD jump, every one of those chains and their `-N` shadows, so a cluster reset still leaves
  the box bare.

## v1.8.0-rc.27

### Fixed

- **The uninstall releases held assigned addresses BEFORE it removes the DNAT chain** (goal 30
  unit-7 review). The other order left a window in which the host answered ARP for a VM's public
  address while nothing forwarded it, so the internet's packets for the guest landed on the host's
  own sshd. Test pins the order.

## v1.8.0-rc.26

### Fixed

- **The uninstall removes the assigned-address machinery too** (goal 30, associate-and-disassociate).
  Conductor's 076-vm-group-bridge applier now also writes a `RUNOS-VMGRP-DNAT` chain in nat
  PREROUTING (assigned address -> the VM's pool address) and, for on-link addresses, holds the
  address on the WAN interface, recording it in `/etc/runos/vm-group-firewall/.held-addresses`.
  The uninstall releases those addresses and removes the chain and its shadow with the other
  RUNOS-VMGRP chains, so a cluster reset still leaves the box bare.

## v1.8.0-rc.25

### Fixed

- **A transient error fetching the install command list is retried, not fatal** (goal 30, G30-F3
  review). Since rc.24 a fetch that fails reports `INSTALL_ERROR`, so a nodeward restart
  (`Unavailable`) or a slow call (`DeadlineExceeded`) must not reach that path when a retry would
  have carried it through. Both are now retried with the same backoff as the "not connected" case.

## v1.8.0-rc.24

### Fixed

- **An install that dies before it fetches its command list now reports `INSTALL_ERROR`** (goal 30,
  G30-F3). The command runner already reports it when a command fails, but a fetch that fails after
  its retries, or a wait for a control plane that gives up, happens before any command runs, so the
  node stayed at `not_installed` with a dead installer: the provisioning job upstream could not
  fast-fail on it and waited out its whole readiness clock, and a first-node claim held by such a
  node could not expire on failure. Best-effort; the install still exits non-zero with the same
  message either way.

## v1.8.0-rc.23

### Fixed

- **The uninstall removes the `-N` shadow firewall chains too** (goal 30 unit-2 review). The
  076-vm-group-bridge applier now rebuilds its chains under a `-N` shadow name and renames them into
  place atomically, so a reconcile never leaves a window with no metadata/anti-spoof/cross-pool
  drops. On a clean node the shadow is already renamed away, but a build that failed mid-swap leaves
  a `RUNOS-VMGRP-*-N` chain behind; the uninstall now flushes and removes those so a reset still
  leaves the box bare.

## v1.8.0-rc.22

### Fixed

- **The uninstall removes the pool-egress NAT chain it now installs** (goal 30,
  guest-egress-without-the-pod-nic). Conductor's 076-vm-group-bridge gained a `nat POSTROUTING`
  chain (`RUNOS-VMGRP-NAT`) that masquerades guest pool egress; the uninstall now tears it down with
  the other RUNOS-VMGRP chains, so a cluster reset leaves the box bare. Same rule, same shape as the
  mangle/filter chains: whatever RunOS installs on a node, its removal is written in the same change.

### Fixed

- **The first `GetInstallCommands` after a wait gets 90 seconds, not 30** (goal 30, G30-F1 review).
  When a second control plane waits for the first and the first turns ready, the very next call is the
  expensive one: nodeward fetches the join command from the just-ready control plane. A 30-second
  deadline there could fail the install on attempt 1 with a message claiming five attempts, on exactly
  the happy path the wait exists to serve.

## v1.8.0-rc.20

### Fixed

- **Two control planes may now be started together on an empty cluster** (goal 30, G30-F1, with
  nodeward 1.6.0-rc.35). Until now the first node was elected by counting READY control planes, so
  two machines that registered before either had finished installing were BOTH told they were first,
  both ran `kubeadm init`, and RunOS reported one healthy two-node cluster that was two clusters with
  two CAs (measured on ede 2026-08-17). Nodeward now hands the role to exactly one machine. The agent's
  half: a node whose install finds no ready control plane to join WAITS for one, asking again every
  15 seconds for up to 30 minutes, when nodeward says so with a `FailedPrecondition` whose message
  starts `WAIT_FOR_CONTROL_PLANE:`; every other error keeps its old meaning. And reporting
  `INITIALIZING_NEW_CLUSTER`, seconds before `kubeadm init`, is now a question: if nodeward answers
  `FIRST_NODE_CLAIM_RELEASED` (the role went to another machine while this install looked dead) the
  install stops without initialising, and if nodeward cannot be reached for two minutes it also stops,
  because starting a cluster on a guess is the defect being removed. Re-running `sudo runos install`
  then joins the cluster the other machine built.

## v1.8.0-rc.19

### Fixed

- **The uninstall removes the VM group segment firewall it was leaving behind.** Conductor's
  076-vm-group-bridge installs a conf per group, an applier, a boot unit and iptables chains jumped
  to from mangle PREROUTING and filter INPUT on both address families. Measured 2026-08-17,
  immediately after that fence was written: a full cluster reset left the unit ENABLED, the applier
  in place and both chains installed on every host, on boxes the reset had otherwise returned to
  bare. Same shape as the rvg bridge gap (R4) two rounds earlier, found the same way. The chains go
  before the files, so a boot racing the uninstall cannot re-apply from a conf that is about to
  disappear, and every step names the RUNOS-VMGRP chains explicitly so a builtin can never be
  flushed. The FORWARD chain an earlier version of the fence used is torn down too, for nodes
  provisioned before the hook moved.

## v1.8.0-rc.18

### Fixed

- **The endpointless declaration converges BOTH ways again** (adversarial review of rc.17). The
  liveness rule in rc.17 could tell a working address from a dead one, which is what R8 needed, but
  it cannot tell an address WE configured from one the kernel learned by roaming. So a peer we had
  configured with a public endpoint, re-declared as having no inbound path while that tunnel was up,
  read live for ever and its endpoint was never cleared: the declaration converged one way and not
  back, which is the failure the whole file was written about. The agent now remembers what it last
  APPLIED per peer. A peer whose applied endpoint was non-empty and is now empty has had its
  declaration changed and clears at once; a peer that was already endpointless can only be holding a
  roamed address, and there liveness rules.
- **A stepped clock no longer wipes every roamed endpoint at once.** `wg` reports the handshake as
  wall clock and so does the agent, so an NTP step, a resumed machine or a box with a dead RTC moves
  one without the other. A negative age is now read as unmeasurable rather than as stale, and
  clearing a roamed endpoint takes TWO consecutive dead readings, so one bad sample cannot cost the
  30 to 70 second outage R8 was filed about.
- **An unparseable `wg show wg0 dump` is an error, not "this node has no peers".** An empty map plans
  no removals, so a format change would have silently retired the rule that a removed node's key
  stops working, with nothing to say so.
- The liveness window is now pinned by a test that states a literal 125 s age (just past WireGuard's
  120 s REKEY_AFTER_TIME). The rc.17 tests derived their input from the constant, so the suite stayed
  green with the window set to ten seconds. Its comment no longer claims our own
  persistent-keepalive refreshes the handshake: for an endpointless peer the far side is necessarily
  the initiator, so the refresh comes from there, and 180 s is justified by REJECT_AFTER_TIME.

## v1.8.0-rc.17

### Fixed

- **A peer declared to have no inbound path no longer loses its live address on every peer sync**
  (goal 19 prod-readiness round, finding R8). WireGuard reports one endpoint field and never says
  whether the kernel got it from us or learned it from an inbound packet. The convergence plan
  read that one field, saw an address on a peer that is supposed to have none, and removed and
  re-added the peer to clear it. For a NAT'd peer that address is the whole point: the peer dials
  out and the far side learns where from, so the rule fired on a WORKING session every single
  pass.
  - Measured on cluster ede on 2026-08-17, two home nodes declared `noPublicIngress` and a third
    node in Hetzner: over 17.7 minutes the third node held no endpoint for its two peers in 23 of
    62 samples (37 % of the time), across 6 and 7 separate wipe episodes. Each episode destroyed
    the session and took 30 to 70 seconds to re-learn the address from the far side's keepalive,
    and for that whole window the node could not START a conversation with either peer. Both peers
    were etcd members of the same control plane.
  - `PlanPeerConvergence` now also reads each peer's LAST HANDSHAKE, and clears an endpoint only
    when its session is not live. A peer that has never handshaken, or whose handshake is older
    than 180 s, is still cleared, so a node that genuinely moved behind NAT still converges; a
    keepalive-driven session (every peer carries `persistent-keepalive 5`) never approaches that
    age, so a roamed address is left alone.
  - `CurrentWgPeers` becomes `CurrentWgPeerStates` and reads `wg show wg0 dump` rather than
    `wg show wg0 endpoints`, because the endpoints view does not carry the handshake time.

## v1.8.0-rc.16

### Fixed

- **A failed Kubernetes read no longer demotes the node in RunOS's records** (goal 19 review,
  finding R6). The heartbeat built `isCp`, `isWorker` and `status` from `kubectl get node
  <hostname>` through the agent's own kube proxy. Every one of them returned `false` / `false` /
  `not_ready` when that read failed. Nodeward wrote those values, so its control-plane list
  emptied, every agent's proxy lost its backends, and the remaining nodes reported `false` in
  turn. Measured on cluster ede on 2026-08-16: one of three control planes was hard powered off
  and within two minutes RunOS marked both SURVIVORS `not_ready` with `isCp=false`, refused VM
  deletes, and broke kubectl through the proxy on every node, while Kubernetes itself stayed
  healthy with etcd quorum.
  - `IsCP()` now reads the LOCAL fact first: the node is a control plane if the kubeadm static pod
    manifest `/etc/kubernetes/manifests/kube-apiserver.yaml` exists. The label read is the
    fallback for a worker.
  - A control plane reads its OWN API server (`https://127.0.0.1:6443` with `admin.conf`) before
    the proxied path, so it still reports `ready` when the proxy has no targets.
  - When every read fails the agent carries the last known role and status instead of inventing
    a demotion, and reports `unknown` only when it has never read the node object.
  - The heartbeat carries a new `rolesKnown` flag. `false` means the roles are carried, and
    Nodeward refuses to write them. An older agent omits the field and Nodeward reads its absence
    as `true`, which is the behaviour those agents already had.

## v1.8.0-rc.15

### Fixed

- **A script that finished right at its budget is no longer reported as a timeout** (goal 19
  second review, finding 25). `timedOut` was taken from the context alone, and the context always
  expires when a script runs to the end of its budget, including when the script finished at that
  moment and exited 0. The kill of the process group also returned its raw `ESRCH` when the group
  had already gone, and `exec` replaces the process's own result with whatever `Cancel` returns,
  so a clean run reported `exitCode -1, timedOut true` with the good verdict sitting in `response`.
  Conductor reads `timedOut` and throws the verdict away, so a successful step read as a hung node.
  The kill now returns `os.ErrProcessDone` for an already-dead group, and `timedOut` needs a failed
  run as well as an expired deadline.
- **A script's captured output is capped at 2 MiB per stream**, with a marker naming how much was
  dropped. Both buffers grew without limit, so a script that dumped a log held all of it in the
  agent's memory and then built a reply past nodeward's 16 MB gRPC limit, which loses the whole
  reply, verdict included, rather than the excess.
- **The uninstall's VM group bridge loop runs under `timeout 30`** like every other step that talks
  to the kernel. An `ip` wedged on a stuck netlink socket would otherwise hold the whole uninstall
  open with no way out.

## v1.8.0-rc.14

### Fixed

- **`RUN_REMOTE_SCRIPT` reports stdout, stderr and the exit code separately** (goal 19 review,
  findings 2, 3 and 6). `CombinedOutput` merged the two streams, so any script that wrote a
  diagnostic to stderr corrupted its own JSON verdict, and the exit code was thrown away, so a
  `set -e` script that aborted before printing anything read as success with an empty body. The
  response keeps `response` as stdout alone for compatibility and adds `stderr`, `exitCode` and
  `timedOut`.
- **A script run is bounded and killed by process group.** The request carries `timeoutSeconds`
  (default 15 minutes, capped at 60); the run uses `exec.CommandContext` with `Setpgid` and a
  `kill(-pid)` cancel, so a hung script no longer holds one of the five instruction workers
  forever and a grandchild cannot keep the output pipes open.
- **The uninstall removes the VM group pool bridges.** Conductor's `076-vm-group-bridge` persists
  `/etc/systemd/network/90-rvg<gid>.netdev` and `.network`; nothing removed them, so a wiped node
  still carried the units and the live `rvg*` links with their gateway addresses (measured on ftb1
  after two full resets). Scoped to the `90-rvg` prefix and to `rvg*` links, so `wg0` and the
  cilium interfaces are never touched.

## v1.8.0-rc.13

### Fixed

- **The control-plane-driven uninstall (`nodes delete`) really reboots the node now.** rc.12 ran
  the uninstall inline and rebooted from a goroutine, but the uninstall's own `systemctl stop runos`
  killed the agent process first, so the reply was never sent and no reboot happened (goal 23
  review, 2026-08-16; measured on the reset of cluster 8go: every machine wiped, none rebooted).
  The handler now answers at once and runs `runos uninstall --yes` followed by `systemctl reboot`
  in a transient systemd unit that outlives the agent.
- **`runos uninstall --yes` exits non-zero on a partial wipe** even though it reboots, so a script
  cannot read a half-wiped node as clean.
- The orphaned-shim kill after `kubeadm reset` matches only Kubernetes shims
  (`-namespace k8s.io`), not a Docker daemon sharing the box, and no longer kills its own shell.

## v1.8.0-rc.12

### Fixed

- **Preflight egress verdicts are measured in the right order and say when they might be wrong**
  (goal 23 review of F4 and F28). DNS resolution is measured for every target BEFORE any probe,
  so a resolver that was slow once and is now cached no longer reads as "resolved in 1ms then
  timed out connecting (firewall)". The nine targets are probed concurrently (a fully blocked host
  reports in seconds, not minutes). When the summary blames the allowlist it now also names the
  `curl` cross-check and `--skip-check egress-endpoints` (new flag, also `RUNOS_PREFLIGHT_SKIP`)
  for the case where the same URL answers from the box and preflight is the thing that is wrong.
  The HTTP/2 guard is a hermetic negotiation test, not a struct-field assertion. The resolver
  list also reads `resolved.conf.d/*.conf` and the runtime `resolv.conf`.
- **Uninstall no longer leaves a live API server behind on the failure path** (goal 23 F9
  follow-up). After `kubeadm reset`, orphaned pods and shims are stopped before containerd is;
  every timeout kills after TERM (`timeout -k`); the partial-uninstall path reboots with `--yes`;
  the control-plane-driven uninstall (nodes delete) logs its error and reboots instead of
  returning silently. The `wg-quick@wg0` instance unit and its drop-in are removed.

## v1.8.0-rc.11

### Added

- **Peers can carry the VMs they host** (goal 27, vm-group-range-routing). A peer entry may now
  include `extraAllowedIps`: the pool addresses of the virtual machines that node currently hosts.
  The agent folds each into the peer's WireGuard `allowed-ips`, adds a kernel route per address
  (protocol 201, converged exactly like the peer routes), and allows the VM group's /24 through
  ufw beside the peered cluster ranges. Every extra address is validated like the primary: peer
  fields are untrusted input to a root exec. An agent older than this release ignores the field;
  its own mesh is intact and only the VMs behind the peer are out of its reach.

## v1.8.0-rc.10

### Fixed

- **`runos sync vpn` converges the peer ufw rules too.** rc.9 added the peered-cluster ufw rules on
  the streamed peer set only, so the manual repair tool fixed the routes but not the firewall.
  Measured on a Hetzner node with ufw active, peered to another cluster: hand-deleting the rule cut
  the far cluster's DNS ("no servers could be reached"); `runos sync vpn` re-added the rule and the
  query answered again; revoking the peering removed the peer and the rule inside the push.

## v1.8.0-rc.9

### Changed

- **The stale wg1 user VPN range is no longer guarded at join** (goal 27, wg1-user-vpn-range-preflight).
  Preflight refused a host whose LAN overlapped the hardcoded `172.24.200.0/21`, which is stale now
  that user VPN addressing is account-scoped and pooled in conductor. The check moved to the control
  plane the same way the wg0 `172.24.0.0/16` check did: conductor refuses a VPN-server install onto a
  node whose reported networks overlap the account's real user VPN range. A machine on
  `172.24.200.0/24` now registers without a false refusal. The two Kubernetes ranges stay guarded.

### Fixed

- **A node with ufw active no longer drops a peered cluster's traffic** (goal 27, peering-loose-ends).
  The install-time ufw rules allow this node's own range and the legacy `172.24.0.0/16` only, so a
  peered cluster's globally-unique range was dropped even with the kernel route in place. On every
  `SET_VPN_PEERS` the agent now converges `ufw allow` rules for the /24 of each out-of-prefix peer,
  tagged with a marker comment and added and removed with the peering. A no-op when ufw is absent or
  inactive, and it never touches an operator's own rules.

### Removed

- **The stale `node_agent_installer.sh` and `node_agent_updater.sh` copies.** Nothing read them; the
  scripts a node actually runs are `templates/install.sh` and `templates/update.sh` in the templates
  repo, and the in-repo copies had drifted from them in both directions.

## v1.8.0-rc.8

### Fixed

- **wg0 comes up at boot again.** The stock `wg-quick@.service` is `After=nss-lookup.target`,
  dnsmasq provides that target, and RunOS orders dnsmasq after wg0 (it binds wg0's address). That
  is an ordering cycle, and systemd breaks it by deleting wg0's start job: measured on a Hetzner
  Ubuntu 24.04 node, "dnsmasq.service: Job wg-quick@wg0.service/start deleted to break ordering
  cycle", so a rebooted node ran about 105 seconds with no overlay until dnsmasq's start-pre timed
  out and its restart re-pulled the tunnel. The agent now restores nodeward's INSTANCE unit
  `/etc/systemd/system/wg-quick@wg0.service` on every start, byte-identical to what a fresh
  install writes: the stock template with nss-lookup.target removed, shadowing the template for
  wg0. A drop-in cannot do this, because systemd lets a drop-in add dependencies but never remove
  them (rc.7 tried that and it did not take, which is why rc.7 was never advertised beyond the
  minutes it took to measure). The old drop-in is removed with it. One read on a healthy node; a
  write plus `daemon-reload` on a node installed before the unit. wg0 never needed name
  resolution: every endpoint RunOS distributes is an address.

### Changed

- The "could not read wg0's own prefixes" message during install is now informational. The peer
  set can arrive before wg0 exists once per install; the next peer set converges the routes.

## v1.8.0-rc.7

Superseded by rc.8 within the hour: its drop-in reset of `After=` did not take effect, because
systemd does not let a drop-in remove a dependency. Do not advertise.

## v1.8.0-rc.6

### Added

- **Kernel routes for peers outside this node's own overlay range, so cluster peering can carry
  traffic.** wg0 comes up with the cluster's own /24 as its only route, and `wg set` adds no
  routes, so a peer from a PEERED cluster was accepted by WireGuard's allowed-ips and then
  unreachable: the kernel sent its traffic out the default gateway while the tunnel looked up.
  Every peer whose address is outside wg0's own prefixes now gets a /32 route on wg0, tagged with
  its own routing-protocol number (201) so it can be listed and removed without touching anything
  else, and the routes converge to exactly the peer set: a withdrawn peer loses its route. On a
  cluster with no peerings this is a no-op. The control plane withholds cross-cluster peers from
  any pair where either agent is below this version, because a route on one side alone is a
  tunnel that sends and cannot receive.

### Changed

- **`runos sync vpn` now converges the peer set exactly, like the agent stream does.** It was
  additive, so a node that fell back to manual sync kept retired peers and stale endpoints the
  stream path had already cleared. Both paths now plan the same removals and the same routes.

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
