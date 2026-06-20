# Contributing to the RunOS Node Agent

Thanks for contributing. This is a **public** repository for a Linux daemon that
runs on every RunOS node, so the bar on correctness, clarity, and not leaking
internal details is high.

## Build and test

```bash
make build        # build ./runos (version injected from the latest git tag)
make test         # go test -race ./...
make vet          # go vet ./...
```

Before opening a PR, make sure all of these pass:

```bash
go build ./...
go vet ./...
go test -race ./...
gofmt -l .        # must print nothing
```

The agent is built **CGO-free** and must cross-compile for both target
architectures:

```bash
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build .
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build .
```

Do not introduce a dependency that requires CGO or breaks the linux/amd64 +
linux/arm64 cross-compile.

## Go conventions

- Run `gofmt` (CI and the release gate reject unformatted code). `goimports`
  ordering is preferred.
- Every package has a `// Package <name> ...` doc comment. Keep it accurate when
  you change a package's responsibility.
- Keep `version.Version` a `var` defaulting to `"dev"`; it is overridden at build
  time via `-ldflags`. Do not hardcode a version or convert it back to a `const`.
- Prefer the existing patterns: structured logging via `roslog`, command
  execution via `commons`, control-plane calls via `backend`. Instruction
  handlers live in `agentstream/in_*.go`.
- No emdashes in code, comments, or docs.

## No secrets, no real identifiers (public repo)

Never commit:

- Credentials, tokens, API keys, or private keys (PEM, mTLS keys, etc.).
- Real account / cluster / node IDs, OSIDs, or other opaque identifiers.
- Org or customer names, internal hostnames or IPs, or private URLs.

Use placeholders in examples and fixtures. The public default hosts
`nodeward.runos.com` and `get.runos.com` and the release-workflow OIDC identity
URL are the only "real" public values that belong here. The release script runs
a deterministic secret-pattern scan over the deploy payload and fails closed;
do not rely on it as your only check, keep the source clean.

## Changelog

User-facing changes need a `## vX.Y.Z` section in `CHANGELOG.md`. The release
pipeline extracts that section verbatim as the GitHub release notes, and the
release script refuses a version with no matching section.

## Releasing

Releases are deterministic and scripted, not hand-run. The node agent ships as
raw attested binaries on GitHub Releases; publishing the artifact is the release
(rolling the live fleet is gated downstream). Use `make release` (which runs
[`scripts/release.sh`](scripts/release.sh)): it runs preflight checks, a
fail-closed sensitivity scan, and the build/vet/test gates before tagging and
pushing. Do not push release tags or publish releases by hand.

## Security

For anything security-sensitive, see [`SECURITY.md`](SECURITY.md). Report
vulnerabilities to security@runos.com rather than opening a public issue.
