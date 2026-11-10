# Notes for nodeward (goal 19 fix pass, 2026-08-16)

**No nodeward change is needed for the new `RUN_REMOTE_SCRIPT` response fields.** Verified by
reading, not assumed:

- `agentstream/node.go:handleNodeAgentMessage` hands the whole `*pb.FromNodeAgent` to the waiting
  channel and never decodes `JsonB64`.
- `grpc/l3sec/grpc_rpc_communicate.go:CommunicateWithNode` returns `res.JsonB64` and `res.Type`
  verbatim to conductor.

So `stderr`, `exitCode` and `timedOut` survive the relay untouched, and an older nodeward carries
them just as well as a new one. The only nodeward-side caller of `RUN_REMOTE_SCRIPT` is
`communicate/update_hosts_file.go:28`, which ignores the response body.

One thing worth knowing rather than changing: nodeward's own `communicateWithNode` timeout
defaults to 30s and is overridden by the request's `timeoutSeconds`. The agent now enforces the
same budget itself, so a long script fails with the agent's verdict (`timedOut: true`) rather than
a bare transport timeout, PROVIDED conductor keeps sending `timeoutSeconds`. It does.

## R6: the heartbeat now carries `rolesKnown` (2026-08-17)

The node agent heartbeat payload gains one field:

```json
{"externalIpAddress": "...", "isCp": true, "isWorker": false,
 "status": "ready", "version": "1.8.0-rc.16", "rolesKnown": true}
```

`rolesKnown: false` means the agent could NOT read its node object and is carrying its last known
role, or deriving `isCp` from the local kubeadm static pod manifest. Nodeward must not write
`isCp` / `isWorker` from such a heartbeat, because writing `false` over a live control plane is
what emptied the control-plane list on a test cluster on 2026-08-16.

Decode it as `*bool` and read `nil` as `true`: an agent older than v1.8.0-rc.16 omits the field,
and `true` is exactly the behaviour those agents already had.

The agent can also now report `status: "unknown"`, which it does only when it has never read its
node object. Nodeward must NOT store `unknown` as the node status: `GetControlPlaneNodes` filters
on `status = 'ready'`, so storing it would empty the control-plane list by another route.

This nodeward change is implemented in nodeward 1.6.0-rc.34, not left as a note.

## `GetInstallCommandsRequest` now carries `nodeAgentVersion` (2026-11-08)

**Nodeward must read the node agent version from this REQUEST, not from the stored node record.**

The proto message gains one field. It is additive and back-compatible: the field number is new and
every other field keeps its number.

| Message | Field | Number | Type | Wire |
| --- | --- | --- | --- | --- |
| `GetInstallCommandsRequest` | `nodeAgentVersion` | 10 | `string` | length-delimited |

```proto
message GetInstallCommandsRequest {
  string jsonB64FileList  = 5;
  string nodeAgentVersion = 10;  // added 2026-11-08
}
```

The agent fills it from its own build version, which the release pipeline injects at link time.

**Why the request and not the node record.** The runtime package holds are gated on the node agent
version: nodeward may send the install-hold commands only to a node whose agent is new enough to
release and set those holds again in a controlled run. The stored node record's version comes from
the HEARTBEAT, and a node that is being installed has not necessarily sent one yet. A gate reading
the record would therefore see no version on a fresh node and skip the holds on exactly the nodes
the install is placing them on.

**Fail closed on all three of these values.** Each one means "this agent cannot be trusted with the
holds", and none of them may be treated as new enough:

1. **Absent.** Any node agent released before this change sends no field at all, and protobuf
   decodes an absent string as `""`. Most nodes in the fleet are in this state.
2. **Empty.** A build that lost its link-time version sends `""`.
3. **A non-release string.** A local build sends `dev`. Any value that does not parse as a release
   version is in this class, and refusing to parse is the right answer, not guessing.

A fourth case is not a refusal: a release-candidate version such as `1.8.7-rc.4` is a real build and
the gate should compare it as the version it names.

**This field is only half the gate.** The agent sends the version; nodeward decides. The node agent
does not refuse the install-hold commands on its own version, so a nodeward that skips the check
sends holds to an agent that cannot release them, which is what the gate exists to prevent.

**The VPN install-command RPC cannot carry this gate.** A node install calls
`GetInstallVpnCommands` before `GetInstallCommands`. The standalone WireGuard install command also
calls only `GetInstallVpnCommands`. Its `GetInstallVpnCommandsRequest` message has no fields, so it
carries no node agent version. Nodeward must emit no package-hold command from the VPN command
list. Every install-hold command, including a hold for `wireguard-tools`, must come from the
`GetInstallCommands` response after Nodeward accepts `nodeAgentVersion`. This boundary keeps the
gate closed when only the VPN command list runs.

**The proto is hand-synced and has no gate across the two repositories.** Use field number 10 for
this field and this type. A different number on the nodeward side means both sides marshal happily
and the gate silently reads nothing. This repository holds only the generated file, and two guards
cover its own half: `go test ./l2sec/` holds the Go struct tag and the serialized descriptor to each
other for every field, and `make proto-check` refuses a generated file that protoc-gen-go would not
produce from its own descriptor. Neither guard can see nodeward's copy.

## Node patch facts outbound message

Nodeward must register `NodePatchFactsToServer` in its instruction map. A missing map entry silently drops every report.

The message uses the existing agent-request lane. Its `JsonB64` field contains base64-encoded JSON with these 17 fields:

```text
collectedAt
runningKernel
installedKernels
newestInstalledKernel
rebootRequired
rebootRequiredSince
rebootTriggerPackages
securityUpdateCount
securityUpdatePackages
securityUpdatesStale
packageMetadataAgeSeconds
packageMetadataSource
lastPackageRunAt
bootTime
automaticUpdateUnitState
heldPackages
fieldErrors
```

Nodeward must reply on the same tag with this base64-encoded JSON shape:

```json
{"accepted": true, "retryable": false, "message": ""}
```

`accepted: true` means that Nodeward accepted the record for forwarding. The reply does not mean that Conductor stored the record.

Use `accepted: false, retryable: true` for a temporary refusal, such as a full queue. The agent retries with jitter capped at 30 seconds.

Use `accepted: false, retryable: false` for a permanent validation refusal. Put the invalid field or reason in `message`. The agent logs the reason, drops that record, and waits for a fresh sample.

The agent treats a send error, timeout, undecodable reply, or retryable refusal as a retry. One pending record slot keeps only the newest sample.
