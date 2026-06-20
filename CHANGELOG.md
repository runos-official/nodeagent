# Changelog

All notable changes to the RunOS node agent are documented here. The format
follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/) and the project
uses [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

The release pipeline extracts the section matching the pushed tag (`## vX.Y.Z`)
as the GitHub release notes, so every released version needs a section here.

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
