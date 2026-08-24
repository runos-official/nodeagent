# Contributing to the RunOS Node Agent

Thanks for contributing. This is a **public** repository for a Linux daemon that
runs on every RunOS node, so the bar on correctness, clarity, and not leaking
internal details is high.

## Build and test

```bash
make hooks        # install the tracked git hooks (run once per clone)
make build        # build ./runos (version injected from the latest git tag)
make test         # go test -race ./...
make vet          # go vet ./...
make leakcheck    # public-repo leak gate over every tracked file
```

Before opening a PR, make sure all of these pass:

```bash
go build ./...
go vet ./...
go test -race ./...
gofmt -l .        # must print nothing
make leakcheck    # must print "leakcheck: clean"
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

## First, install the git hooks

Run this once per clone, right after you clone:

```bash
make hooks        # sets core.hooksPath to the tracked .githooks/ directory
```

`.git/hooks` is not tracked, so a hook that is not installed is a hook nobody
has. The `pre-commit` hook runs `leakcheck` over your staged diff and blocks a
commit that would publish a credential or a new internal identifier.

## No secrets, no real identifiers (public repo)

Never commit:

- Credentials, tokens, API keys, or private keys (PEM, mTLS keys, etc.).
- Real account / cluster / node IDs, OSIDs, or other opaque identifiers.
- Org or customer names, internal hostnames or IPs, or private URLs.
- Named lab or rented test machines, and pasted terminal output that carries a
  real address.

Use placeholders in examples and fixtures. For addresses, use the ranges that
exist for exactly that purpose: `192.0.2.0/24`, `198.51.100.0/24` and
`203.0.113.0/24` (RFC 5737), and `2001:db8::/32` (RFC 3849). The public default
hosts `nodeward.runos.com` and `get.runos.com` and the release-workflow OIDC
identity URL are the only "real" public values that belong here.

### The leak gate

`scripts/leakcheck.py` enforces the rule. It runs in three places: the
`pre-commit` hook (staged diff only, fast), `make leakcheck` (on demand), and
`scripts/release.sh` (whole tree, and it cannot be skipped).

```bash
make leakcheck          # scan every tracked file
make leakcheck-staged   # scan only what is staged
make leakcheck-test     # test the checker itself
make leakcheck-update   # ratchet the baseline down after removing an identifier
```

It has two severities.

- **Credentials** hard fail, always. They can never be baselined.
- **Internal identifiers** are ratcheted, like a knip dead-code baseline.
  `scripts/leakcheck.baseline` records what this repo has already published, so
  existing work is not blocked. A NEW identifier fails the gate.

What counts as an internal identifier: the machine names and account ids listed
in `scripts/leakcheck.config`, and any IP address literal outside the
documentation, loopback, link-local, unspecified, broadcast and well-known
multicast ranges. Addresses are allow-listed rather than deny-listed because you
cannot tell a real address from an invented one by reading it. A project
constant such as a service CIDR is absorbed into the baseline once and never
asked about again.

**Do not hand-add a line to `scripts/leakcheck.baseline` to get a commit
through.** A line in that file is a record of a leak that already shipped, not a
licence to add another. Remove the identifier from the source instead, then run
`make leakcheck-update` so the baseline shrinks.

The pre-commit hook can be skipped in a genuine emergency with
`git commit --no-verify`, and it says so when it fires. That does not get the
change released: the release gate runs the same checker over the whole tree.

## Changelog

User-facing changes need a `## vX.Y.Z` section in `CHANGELOG.md`. The release
pipeline extracts that section verbatim as the GitHub release notes, and the
release script refuses a version with no matching section.

## Releasing

Releases are deterministic and scripted, not hand-run. The node agent ships as
raw attested binaries on GitHub Releases; publishing the artifact is the release
(rolling the live fleet is gated downstream). Use `make release` (which runs
[`scripts/release.sh`](scripts/release.sh)): it runs preflight checks, a
fail-closed sensitivity scan, the leak gate, and the build/vet/test gates before
tagging and pushing. Do not push release tags or publish releases by hand.

## Security

For anything security-sensitive, see [`SECURITY.md`](SECURITY.md). Report
vulnerabilities to security@runos.com rather than opening a public issue.
