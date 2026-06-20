# Security Policy

## Reporting a Vulnerability

If you discover a security vulnerability in any RunOS product or service, including the node agent, control plane, web console, or infrastructure, please report it responsibly.

**Email:** security@runos.com

Please include:
- A description of the vulnerability
- Steps to reproduce
- Potential impact

We will acknowledge reports within 8 hours and aim to provide a fix or mitigation plan within 3 days for critical issues.

## Security Practices

- **No hardcoded secrets.** No credentials, tokens, or private keys are committed to this repository. Runtime material (mTLS client cert/key, CA, account and node identifiers) is loaded from the node's config and certificate files, never baked into the binary.
- **mTLS for all operational traffic.** The agent talks to the Nodeward control plane over two channels: **L1Sec (port 9191)** is TLS-only and used solely for initial registration and certificate exchange; **L2Sec (port 9192)** is mutual TLS and carries every operational instruction. The agent presents its client certificate on L2Sec and will not run operational instructions on an unauthenticated channel.
- **Certificate handling.** The client certificate, key, and CA live under `/etc/runos/` (`mtls.crt`, `mtls.key`, `ca.crt`, plus the public L1Sec CA). The private key is node-local and is never transmitted or logged. Restrict these files to root.
- **Least privilege on the wire.** Instructions arrive as base64-encoded JSON with a UUID tag over the authenticated stream; responses are correlated by tag. The agent does not accept inbound operational commands from any source other than the mTLS stream.
- **Transparent protocol.** The agent's behavior is observable from this source: the instruction catalog, the control-plane hosts, and the install/update flow are all in the open. There are no hidden command channels.

### Privilege scope (root on the node, by design)

The node agent runs as **root** and manages the machine directly: it installs
system packages, configures WireGuard and networking, runs `kubeadm`/`kubectl`,
manages etcd membership, and executes control-plane-issued scripts and commands.
This is a deliberate trade-off, not an oversight:

- The agent's job is to turn open-ended control-plane instructions into real node
  operations, which inherently requires privileged, root-level access. The set of
  operations is not known ahead of time, so it cannot be reduced to a fixed,
  unprivileged capability set without breaking legitimate node management.
- The security boundary that matters is therefore the **mutually authenticated
  (mTLS) control-plane link** that decides *what* the agent is told to do, plus
  the node-local protection of the agent's certificate and key, rather than a
  reduced on-node privilege.
- **Operator implications.** Anyone who can issue instructions over the trusted
  control-plane stream, or who obtains a node's mTLS client certificate, can
  effectively run as root on that node. Protect the control-plane credentials and
  the node's `/etc/runos/` key material accordingly; treat them as the primary
  thing to monitor and rotate.

## Release Integrity & Trust Model

The `runos` binary installed on a node is cryptographically tied to the artifact this repository's release pipeline produced.

The node agent ships as **raw linux binaries** on GitHub Releases:
`nodeagent-linux-amd64`, `nodeagent-linux-arm64`, and `checksums.txt`
(`sha256sum` of the two binaries). The on-node installer downloads the release
asset directly to `/usr/local/bin/runos`.

1. **Keyless build-provenance attestation (this repo).** The release workflow
   (`.github/workflows/release.yml`) runs `actions/attest-build-provenance`,
   which issues a keyless Sigstore attestation for **each** released binary. Each
   attestation binds the binary's sha256 to this workflow's OIDC identity,
   `https://github.com/runos-official/nodeagent/.github/workflows/release.yml@<ref>`.
   No long-lived signing secret is stored, which is safe for a public repo.
   Verify a released binary manually:

   ```bash
   gh attestation verify nodeagent-linux-amd64 --repo runos-official/nodeagent
   ```

2. **Published checksums.** `checksums.txt` carries the sha256 of both binaries,
   generated in the same workflow run, so a downloaded binary can be checked
   against the published digest before it is installed.

3. **Exact-version pinning.** A node can pin an exact release tag
   (`runos update --version v0.24.0`), downloading that specific release asset
   rather than tracking a moving pointer. Which version the fleet runs is gated
   downstream by what the control plane advertises (`NODE_AGENT_VERSION`) and a
   per-cluster conductor pin; publishing a release does not roll the fleet.

Together these defend against **post-build tampering of the distributed
artifact** (replace-asset and swap-checksum attacks): a swapped release binary
no longer matches its attestation or its published sha256 and is detectable.

### What attestation does NOT guarantee (accepted limitation)

Attestation proves a binary *came from this workflow*. It does **not** prove the
binary is benign. A compromised **build**, malicious code merged into the release
branch, or a subverted workflow/runner would produce an evil-but-validly-attested
binary that passes verification. The attestation is blind to this class of attack.

The **only** defense against a subverted build is repo-side access control:
keeping malicious code and workflow edits out in the first place. The two layers
below are that defense.

### Codified in-repo: pinned action SHAs

Every `uses:` in `release.yml` and `ci.yml` is pinned to a full 40-character
commit SHA with a trailing version comment (for example
`actions/attest-build-provenance@a2bbfa25375fe432b6a289bc6b6cd05ecd0c4c32 # v4.1.0`).
A moved tag on a third-party action therefore cannot silently swap in new code.
**Re-confirm this on every workflow edit**, and pin any newly added action the
same way before merging.

### Admin-only hardening checklist (human/admin action required)

These controls cannot be enforced by files in this repo. A GitHub org/repo admin
must apply them in repository settings; they are the actual mitigation for the
build-compromise limitation above:

- [ ] **Branch protection** on the release branch: require pull requests, block
  direct pushes and force-pushes.
- [ ] **Required PR review**: at least one approving review before merge.
- [ ] **Restrict workflow edits**: limit who can modify files under
  `.github/workflows/` and tighten the repo's Actions permissions.
- [ ] **Restrict tag push and release publishing**: the release workflow triggers
  on `v*` tag pushes, so control over who can push tags and publish releases is
  control over what gets attested and shipped.
- [ ] **Enable Immutable Releases**: so assets cannot be replaced after a release
  is published, closing the swap-asset-after-publish window. The release workflow
  creates the release as a draft and flips it to published precisely so asset
  upload still works under Immutable Releases.
