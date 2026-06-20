# RunOS Node Agent

The node agent (`runos`) is the small daemon you install on each machine you want
to add to your [RunOS](https://runos.com)-managed Kubernetes cluster. It runs on
your hardware, joins the machine to your cluster, and keeps it connected and
healthy.

It only ever dials **out** to the RunOS control plane over an encrypted,
mutually-authenticated link, so there are no inbound ports to open. A machine
behind NAT or a home/office firewall works without any port forwarding.

## What it does for you

- **Joins the machine to your cluster** and installs the Kubernetes bits it
  needs.
- **Connects your nodes with an encrypted overlay network** (WireGuard), so they
  reach each other even across NAT and different networks.
- **Keeps the Kubernetes API reachable** through a local load balancer, so the
  cluster stays usable as nodes come and go.
- **Reports health** back to the control plane and **updates itself** to the
  version the control plane has selected.

## How it works

The node agent holds one long-lived, mutually-authenticated connection to the
RunOS control plane. The control plane sends it instructions ("join the cluster",
"set up the network", "update yourself") and the agent carries them out on the
machine, then reports back. Nothing reaches the node except over that one
authenticated, outbound link.

```
        mutually-authenticated link (agent dials out, no inbound port)
  RunOS control plane  <───────────────────────────►  node agent
                                                       (on your machine)
```

The full transport detail and the complete instruction set live in
[docs/architecture.md](docs/architecture.md).

## Requirements

- Linux (Ubuntu 24.04 recommended), `amd64` or `arm64`
- Root access (it installs system packages and manages networking)
- Outbound network access to the RunOS control plane and installer

## Installing

When you add a node in the RunOS console or CLI, you get a one-line install
command to run on the machine. That command downloads the agent, registers the
node, and starts it as the `runos.service` systemd unit. See
[docs/getting-started.md](docs/getting-started.md) for the full walkthrough.

The agent ships as attested Linux binaries on
[GitHub Releases](https://github.com/runos-official/nodeagent/releases); the
installer downloads the exact release the control plane has selected and verifies
its checksum before installing it to `/usr/local/bin/runos`.

## Updating

```bash
runos update                     # update to the version the control plane advertises
runos update --version v0.24.0   # pin to an exact release
```

Which version a node should run is decided by the RunOS control plane, so a plain
`runos update` moves you to the version selected for your cluster.

## Security

The agent talks to the control plane over mutual TLS only and holds no inbound
listener for control traffic. Released binaries carry a keyless Sigstore
build-provenance attestation, so you can verify any binary came from this
repository's pipeline:

```sh
gh attestation verify nodeagent-linux-amd64 --repo runos-official/nodeagent
```

Because the agent manages the machine on the control plane's behalf, it runs with
root and broad node access. See [SECURITY.md](SECURITY.md) for the trust model,
what that means for you, and how to report a vulnerability.

## Documentation

- [docs/getting-started.md](docs/getting-started.md) — install and first run
- [docs/architecture.md](docs/architecture.md) — how it works, in depth
- [docs/configuration.md](docs/configuration.md) — configuration and hosts
- [docs/commands.md](docs/commands.md) — command reference
- [docs/troubleshooting.md](docs/troubleshooting.md) — diagnosing problems
- [SECURITY.md](SECURITY.md) — security model and reporting
- [CONTRIBUTING.md](CONTRIBUTING.md) — building, testing, and releasing
- [CHANGELOG.md](CHANGELOG.md) — release history

## License

The RunOS node agent is **source-available** under the
[Elastic License 2.0](LICENSE): the source is published for transparency and
security review, not as open source. Use is subject to the license terms. See
[LICENSE](LICENSE) and [NOTICE](NOTICE). Copyright 2026 RunOS.
