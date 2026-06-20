# Changelog

All notable changes to the RunOS node agent are documented here. The format
follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/) and the project
uses [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

The release pipeline extracts the section matching the pushed tag (`## vX.Y.Z`)
as the GitHub release notes, so every released version needs a section here.

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
