# Architecture

This document is the in-depth companion to the [README](../README.md). It
describes how the node agent is put together: the control-plane link, the two
security channels, and the full set of instructions it handles.

## The control-plane link

The node agent maintains one long-lived, mutually-authenticated gRPC connection
to the RunOS control plane (Nodeward) and translates the instructions it receives
into real operations on the machine: WireGuard overlay networking, Kubernetes
install/join via kubeadm, a local HAProxy Kubernetes-API load balancer, etcd
membership, and virtual-IP failover.

Instructions arrive as `ToNodeAgent` messages (a type, a base64-encoded JSON
payload, and a UUID tag); the agent replies with `FromNodeAgent` messages
carrying the same tag, so each request is matched to its response. A worker pool
processes instructions with bounded concurrency.

### The two channels

- **L1Sec (port 9191)** — TLS-only. Used only for registration and certificate
  exchange, when the node first proves its enrolment token and receives its
  client certificate.
- **L2Sec (port 9192)** — mutual TLS. Used for every operational instruction
  after registration. The node authenticates with the client certificate it
  received over L1Sec.

The node dials out to both; the control plane never opens a connection to the
node.

## Instruction catalog

The agent handles the instruction types the control plane sends over the stream.
They arrive only over the authenticated mTLS channel; see
[SECURITY.md](../SECURITY.md) for the trust model and what this command surface
means for the agent's privileges.

| Instruction | Purpose |
|---|---|
| `SET_VPN_PEERS` | Configure WireGuard peers |
| `GET_NODE_STATUS` | Report node readiness |
| `GET_CLUSTER_JOIN_CMD` | Return the kubeadm join command |
| `APPLY_CR` / `DELETE_CR` | Apply / delete Kubernetes custom resources |
| `RUN_KUBECTL_COMMAND` | Execute a kubectl command |
| `RUN_REMOTE_SCRIPT` | Run a remote script |
| `INSTALL_HELM_CHART` / `UNINSTALL_HELM_CHART` | Helm operations |
| `RUN_WEB_REQUEST` | Perform an HTTP request |
| `APPLY_OPERATOR` | Apply a Kubernetes operator |
| `UNINSTALL_NODE` / `REINSTALL_NODE` | Node lifecycle |
| `REMOVE_ETCD_MEMBER` | Remove a member from the etcd cluster |
| `UPDATE_DNSMASQ` | Update DNS settings |
| `UPGRADE_NODE_K8S` | Upgrade Kubernetes on the node |
| `VIP_ASSIGN` / `VIP_RELEASE` | Assign / release the control-plane virtual IP |

## On-node responsibilities

- **Registration + mTLS.** On `register`, the agent authenticates to the control
  plane over L1Sec and receives client certificates; all later traffic uses the
  L2Sec mTLS channel.
- **WireGuard VPN.** Generates a keypair, reports its public key and overlay IP,
  receives peer configs, and keeps the overlay in sync so nodes reach each other
  across NAT.
- **Kubernetes install / join.** Installs containerd + Kubernetes and runs the
  kubeadm flow to bring the node up as a control-plane or worker node.
- **HAProxy API load balancer.** Runs a local HAProxy (listener on port 6446)
  that balances Kubernetes API traffic across control-plane nodes and is
  refreshed as the control-plane set changes.
- **etcd.** Manages etcd membership, including removing a departing member.
- **VIP failover.** Manages a virtual IP for resilient control-plane access.
- **Health + status.** Heartbeats and node-status reporting back to the control
  plane.

## Configuration

Default control-plane host: `nodeward.runos.com`. Default installer host:
`https://get.runos.com`. Both can be overridden via `/etc/runos/config.yaml` or
environment variables; see [configuration.md](configuration.md).
