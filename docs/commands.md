# Commands Reference

The RunOS Node Agent provides several commands for managing your node's lifecycle and operations.

## Command Overview

```bash
runos [command] [flags]
```

**Core Commands:**
- `agent` - Start the agent daemon
- `register` - Register node with RunOS
- `install` - Install Kubernetes and VPN components
- `uninstall` - Remove components from node
- `status` - Check agent status
- `version` - Display version information

**Operational Commands:**
- `preflight` - Check system readiness before installation
- `logs` - View agent logs with color formatting
- `sync vpn` - Synchronize VPN peer configuration
- `certificate renew` - Renew mTLS certificate
- `update` - Update the agent to the advertised (or a pinned) version
- `kubeproxy` - Manage the local HAProxy Kubernetes API proxy
- `etcd` - Manage etcd cluster membership
- `set-config <key> <value>` - Set and persist a configuration value

## Available Commands

### `agent`

Start the Node Agent daemon.

```bash
runos agent
```

**Description:**
Starts the agent in daemon mode, connecting to the RunOS control plane and processing management commands.

**What it does:**
- Establishes secure connection to RunOS control plane
- Begins processing instructions from control plane
- Sends regular heartbeat messages
- Reports node health and status
- Manages VPN connectivity
- Proxies Kubernetes API requests

**Usage:**
```bash
# Start in foreground
sudo runos agent

# Start as background service (recommended)
sudo systemctl start runos.service
```

**Logs:** Written to `/var/log/runos.log` in daemon mode

---

### `register`

Register the node with RunOS and obtain security certificates.

```bash
runos register --token <TOKEN> --aid <ACCOUNT_ID> --control-plane <0|1>
```

**Flags:**
- `--token` (required) - Registration token from RunOS console
- `--aid` (required) - Your RunOS account ID
- `--control-plane` (optional) - Set to `1` for control plane, `0` for worker. Defaults to `0`.

**Description:**
Registers this node with the RunOS control plane and downloads security certificates for authenticated communication.

**Example:**
```bash
# Register as worker node
sudo runos register --token abc123 --aid acct_5678 --control-plane 0

# Register as control plane node
sudo runos register --token abc123 --aid acct_5678 --control-plane 1
```

**What it creates:**
- `/etc/runos/config.yaml` - Configuration file
- `/etc/runos/mtls.crt` - Client certificate
- `/etc/runos/mtls.key` - Private key
- `/etc/runos/ca.crt` - CA certificate

**Note:** Registration only needs to be done once per node.

---

### `install`

Install required components (VPN and Kubernetes).

```bash
runos install
```

**Description:**
Installs and configures all necessary software components for the node to join your RunOS cluster.

**What it installs:**
- WireGuard VPN software and configuration
- Kubernetes components (kubeadm, kubelet, kubectl)
- Container runtime (containerd)
- Required network plugins

**Usage:**
```bash
sudo runos install
```

**Duration:** Installation typically takes 5-15 minutes depending on network speed.

**Prerequisites:** Node must be registered first.

---

### `uninstall`

Remove RunOS components from the node.

```bash
runos uninstall
```

**Description:**
Removes Kubernetes, container runtime, and VPN components from the node.

**What it removes:**
- Kubernetes components (kubeadm, kubelet, kubectl)
- Container runtime (containerd)
- WireGuard VPN
- Network configuration

**Usage:**
```bash
sudo runos uninstall
```

**Warning:**
- This operation is destructive and will remove all Kubernetes resources on the node
- The node will reboot after uninstallation
- Configuration and certificates in `/etc/runos/` are preserved

---

### `sync vpn`

Synchronize VPN peer configuration with RunOS control plane.

```bash
runos sync vpn
```

**Description:**
Updates the VPN configuration to reflect the current cluster topology.

**When to use:**
- After adding or removing nodes from the cluster
- When experiencing VPN connectivity issues
- As part of troubleshooting network problems

**Usage:**
```bash
sudo runos sync vpn
```

**What it does:**
- Retrieves current VPN peer list from control plane
- Updates WireGuard configuration
- Refreshes VPN tunnels to all cluster nodes

---

### `status`

Display the current status of the node agent.

```bash
runos status
```

**Description:**
Shows whether the agent is running and connected to the control plane.

**Usage:**
```bash
runos status
```

**Example output** (representative; actual values vary):
```
╔═══════════════════════════════════════════════╗
║   RunOS Node Agent Status                     ║
╚═══════════════════════════════════════════════╝

Agent Version:
  Version: 0.21.22
  Binary:  /usr/local/bin/runos

Connection Status:
  ✓ Connected to Nodeward: nodeward.runos.com

Kubernetes Status:
  ✓ Kubernetes is installed
  → Node Type: Worker
  ✓ Node Status: Ready

Node Configuration:
  Account ID (AID):  <account-id>
  Node ID (NID):     <node-id>
  Node IP:           <node-ip>
  External IP:       <external-ip>
  VPN IP (wg0):      <wg0-ip>
  Nodeward Server:   nodeward.runos.com

mTLS Certificate:
  Certificate Path:  /etc/runos/mtls.crt
  Expires:           2026-01-01 00:00:00 UTC
  ✓ Certificate expires in 195 days

Log File:
  Log File:          /var/log/runos.log
  ✓ Size:            2.4M
```

---

### `version`

Display version information.

```bash
runos version
```

**Description:**
Shows the current version of the Node Agent. The command prints the version
string only (the value injected at build time from the git tag).

**Usage:**
```bash
runos version
```

**Example output** (representative; actual value varies):
```
0.21.22
```

---

### `preflight`

Check if the system is ready for installation.

```bash
runos preflight
```

**Description:**
Performs comprehensive pre-flight checks to ensure the system meets all requirements before attempting installation. This helps identify potential issues early and saves time troubleshooting failed installations.

**What it checks:**
- **System Requirements:**
  - Minimum 2 CPU cores
  - Minimum 4 GB RAM
  - Minimum 16 GB available disk space on root partition
  - Operating system: Ubuntu 24.04
- **System State:**
  - No pending system reboot required
  - No running package manager processes (apt, dpkg)
  - No package manager lock files held

**Usage:**
```bash
# Run preflight checks before installation
sudo runos preflight
```

**Example output (success):**
```
System is ready for installation
```

**Example output (failure):**
```
Preflight check failed: insufficient RAM: found 2.0 GB, need at least 4 GB
```

**Common failures:**

1. **System reboot required:**
   ```
   system reboot required before installation can proceed
   Packages requiring reboot:
   linux-image-5.15.0-89-generic

   Please reboot the system with: sudo reboot
   ```

2. **Package manager running:**
   ```
   package manager is currently running (PIDs: 1234)

   Please wait for package operations to complete or kill these processes
   ```

3. **Insufficient resources:**
   ```
   insufficient disk space: found 10 GB available, need at least 16 GB
   ```

**When to use:**
- Before running `runos install`
- After system updates or configuration changes
- When troubleshooting installation failures
- As part of automated provisioning scripts

**Best practice:** Always run `preflight` before attempting installation to catch issues early.

---

### `logs`

Display RunOS Node Agent logs with color formatting.

```bash
runos logs [flags]
```

**Flags:**
- `-f, --follow` - Follow log output in real-time (like `tail -f`)
- `-n, --lines int` - Number of lines to display (default: 50)

**Description:**
Displays formatted logs from `/var/log/runos.log` with colored output for easy reading. Automatically parses JSON log entries and presents them in a human-readable format.

**Usage:**
```bash
# Show last 50 lines (default)
runos logs

# Show last 100 lines
runos logs -n 100

# Follow logs in real-time
runos logs -f

# Follow with specific line count
runos logs -f -n 200
```

**Color coding:**
- **Gray** - Timestamp
- **Cyan** - INFO level
- **Yellow** - WARN level
- **Red** - ERROR level
- **Magenta** - DEBUG level
- **Blue** - Request tags/IDs
- **White** - Log messages

**Example output:**
```
15:04:05.123 [a1b2c3d4] INFO  Connected to control plane
15:04:10.456 [a1b2c3d4] INFO  Heartbeat sent
15:04:15.789 [e5f6g7h8] INFO  VPN peers synchronized
15:04:20.012 [i9j0k1l2] WARN  Certificate expires in 30 days
15:04:25.345           ERROR Failed to execute command
```

**Features:**
- JSON log parsing and formatting
- Color-coded log levels
- Shortened request/tag IDs (8 characters)
- Real-time following with `-f`
- Handles log rotation automatically
- Only displays valid JSON log entries

**When to use:**
- Monitoring agent operations
- Troubleshooting issues
- Viewing real-time agent activity
- Checking for errors or warnings

**Note:** This command provides a more user-friendly alternative to `journalctl` or directly reading `/var/log/runos.log`.

---

### `certificate`

Manage node agent mTLS certificates.

```bash
runos certificate <subcommand>
```

**Subcommands:**

#### `certificate renew`

Renew the mTLS certificate used for secure communication with the control plane.

```bash
runos certificate renew
```

**Description:**
Renews the node's mTLS certificate by requesting a new one from the Nodeward control plane. This is necessary when certificates are approaching expiration or have been compromised.

**What it does:**
1. Connects to Nodeward using the current certificate
2. Requests a new certificate from the control plane
3. Tests the new certificate to ensure it works
4. Saves the new certificate to `/etc/runos/mtls.crt` if successful
5. Backs up the old certificate

**Usage:**
```bash
sudo runos certificate renew
```

**Important:** After successful renewal, you **must restart the agent** within 5 minutes:
```bash
sudo systemctl restart runos.service
```

**When to use:**
- Certificate is expiring soon (check with: `openssl x509 -in /etc/runos/mtls.crt -noout -dates`)
- Certificate has been compromised
- As part of regular security maintenance
- Before certificate expiration alerts

**Certificate validity:**
- Check expiration: `openssl x509 -in /etc/runos/mtls.crt -noout -enddate`
- Check issuer: `openssl x509 -in /etc/runos/mtls.crt -noout -issuer`
- View full details: `openssl x509 -in /etc/runos/mtls.crt -noout -text`

**Typical renewal process:**
```bash
# 1. Check current certificate expiration
openssl x509 -in /etc/runos/mtls.crt -noout -dates

# 2. Renew certificate
sudo runos certificate renew

# 3. Verify new certificate
openssl x509 -in /etc/runos/mtls.crt -noout -dates

# 4. Restart agent service
sudo systemctl restart runos.service

# 5. Verify connection
runos status
```

**Error handling:**
- If renewal fails, the old certificate remains unchanged
- The agent can continue operating with the old certificate
- Check logs for specific error messages: `runos logs | grep -i certificate`

**Best practices:**
- Renew certificates 30 days before expiration
- Test the renewal process in a non-production environment first
- Monitor certificate expiration dates proactively
- Always restart the agent after renewal

---

### `update`

Update the installed node agent binary to the advertised (or a pinned) version.

```bash
runos update [--version <tag>]
```

**Flags:**
- `--version <tag>` (optional) - Exact release tag to pin to (e.g. `v0.24.0`).
  A leading `v` is accepted and stripped.

**Description:**
Downloads and installs a node agent release from the installer. The agent ships
as attested binaries on GitHub Releases, so the installer always needs an exact
version, never a floating "latest".

**Behavior:**
- `runos update --version v0.24.0` pins to that exact release tag.
- `runos update` (no flag) updates to the version advertised by the installer.
  When the version is empty, no `?version=` pin is sent and the installer is
  fail-closed: it will **not** fall back to a floating "latest".

**Usage:**
```bash
# Update to the advertised version
sudo runos update

# Pin to an exact release tag
sudo runos update --version v0.24.0
```

**Note:** Publishing a release does not roll the live fleet; which version a node
runs is gated downstream by the advertised `NODE_AGENT_VERSION` and a per-cluster
conductor pin.

---

### `kubeproxy`

Manage the local HAProxy Kubernetes API proxy (the load balancer on port 6446
that fronts the control-plane API servers).

```bash
runos kubeproxy <subcommand>
```

**Subcommands:**
- `list` - List the current HAProxy backend servers.
- `refresh` - Refresh HAProxy backends by updating kube-proxy.

**Usage:**
```bash
# List configured backends
sudo runos kubeproxy list

# Refresh backends
sudo runos kubeproxy refresh
```

---

### `etcd`

Manage etcd cluster membership.

```bash
runos etcd <subcommand>
```

**Subcommands:**

#### `etcd list`

List the etcd members with detailed cluster information.

```bash
sudo runos etcd list
```

#### `etcd remove`

Remove an etcd member, by IP or by member ID.

```bash
runos etcd remove --ip <NODE_IP>
runos etcd remove --id <MEMBER_ID>
```

**Flags:**
- `--ip` - IP address of the etcd member to remove.
- `--id` - Member ID to remove (find it with `runos etcd list`).

**Usage:**
```bash
# Remove a member by IP
sudo runos etcd remove --ip 10.0.0.5

# Remove a member by ID
sudo runos etcd remove --id 8e9e05c52164694d
```

---

### `set-config`

Set a single configuration value and persist it to `/etc/runos/config.yaml`.

```bash
runos set-config <key> <value>
```

**Arguments:**
- `<key>` (required) - Dotted config key (e.g. `client.server.nodeward`).
- `<value>` (required) - The value to store.

**Usage:**
```bash
sudo runos set-config client.server.installer https://my-server.com
sudo runos set-config client.server.nodeward nodeward.runos.com
sudo runos set-config node.ip 192.168.1.100
```

**Description:**
Writes the key/value into the config file. Useful for repointing the installer or
nodeward host, or overriding the detected node IP, without hand-editing YAML.

---

### `--help`

Display help information.

```bash
runos --help
runos [command] --help
```

**Description:**
Shows available commands and usage information.

**Usage:**
```bash
# General help
runos --help

# Command-specific help
runos register --help
runos sync --help
runos preflight --help
```

---

## Common Command Workflows

### Initial Node Setup

```bash
# 1. Run preflight checks
sudo runos preflight

# 2. Register the node
sudo runos register --token <TOKEN> --aid <ACCOUNT> --control-plane 0

# 3. Install components
sudo runos install

# 4. Start the agent
sudo systemctl start runos.service

# 5. Verify status
runos status

# 6. View logs
runos logs
```

### Troubleshooting Connectivity

```bash
# 1. Check agent status
runos status

# 2. View recent logs for errors
runos logs | grep -i error

# 3. Follow logs in real-time
runos logs -f

# 4. Sync VPN configuration
sudo runos sync vpn

# 5. Restart agent if needed
sudo systemctl restart runos.service

# 6. Monitor startup
runos logs -f
```

### Certificate Renewal

```bash
# 1. Check certificate expiration
openssl x509 -in /etc/runos/mtls.crt -noout -dates

# 2. Renew the certificate
sudo runos certificate renew

# 3. Verify new expiration date
openssl x509 -in /etc/runos/mtls.crt -noout -dates

# 4. Restart the agent (must do within 5 minutes)
sudo systemctl restart runos.service

# 5. Verify connectivity
runos status

# 6. Check logs for any issues
runos logs -n 20
```

### Node Decommissioning

```bash
# 1. Stop the agent
sudo systemctl stop runos.service

# 2. Uninstall components
sudo runos uninstall

# (System will reboot automatically)
```

## Running Commands with Systemd

When the agent is running as a systemd service:

```bash
# View service status
sudo systemctl status runos.service

# Start service
sudo systemctl start runos.service

# Stop service
sudo systemctl stop runos.service

# Restart service
sudo systemctl restart runos.service

# View service logs (journalctl)
sudo journalctl -u runos.service -f

# Or use the built-in logs command (recommended)
runos logs -f
```

## Exit Codes

The agent uses standard exit codes:
- `0` - Success
- `1` - General error
- `2` - Invalid arguments
- `130` - Interrupted by user (Ctrl+C)

## Command Permissions

All commands require root privileges (sudo) because they:
- Manage system networking
- Install system packages
- Configure kernel modules
- Write to protected directories

Always run commands with `sudo` or as root user.
