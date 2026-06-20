# Getting Started with RunOS Node Agent

This guide will help you install and configure the RunOS Node Agent on your nodes.

## Prerequisites

Before installing the Node Agent, ensure:

1. You have a RunOS account and access token
2. Your node has outbound internet connectivity
3. You have root/sudo access on the node
4. Ports 9191 and 9192 can reach the RunOS control plane

## Installation

### Step 1: Obtain the Agent Binary

The node agent is distributed as raw Linux binaries via GitHub Releases and is
installed as `runos` at `/usr/local/bin/runos`. Download the release asset for an
exact version and install it:

```bash
# Download for Linux x86_64 (replace <version> with an exact release tag, e.g. v0.24.0)
sudo curl -L \
  https://github.com/runos-official/nodeagent/releases/download/<version>/nodeagent-linux-amd64 \
  -o /usr/local/bin/runos

# Make it executable
sudo chmod +x /usr/local/bin/runos
```

### Step 2: Run Pre-flight Checks

Before installation, verify your system meets all requirements:

```bash
sudo runos preflight
```

This command checks:
- CPU cores (minimum 2)
- RAM (minimum 4 GB)
- Disk space (minimum 16 GB available)
- Operating system (Ubuntu 24.04)
- No pending system reboots
- No active package manager processes

If any checks fail, address the issues before proceeding.

### Step 3: Register Your Node

Registration connects your node to the RunOS control plane and obtains security certificates:

```bash
sudo runos register --token <YOUR_TOKEN> --aid <ACCOUNT_ID> --control-plane <0|1>
```

**Parameters:**
- `--token`: Your RunOS registration token (obtain from RunOS console)
- `--aid`: Your RunOS account ID
- `--control-plane`: Set to `1` if this is a control plane node, `0` for worker nodes

**Example:**
```bash
sudo runos register --token abc123xyz789 --aid acct_1234 --control-plane 0
```

### Step 4: Install Required Components

Install Kubernetes and networking components:

```bash
sudo runos install
```

This command will:
- Install WireGuard VPN software
- Configure two VPN interfaces for network connectivity:
  - **wg0** (172.24.0.0/16) - Internal Kubernetes node-to-node communication
  - **wg1** (172.24.200.0/21) - User VPN access to reach internal services from outside
- Install Kubernetes components (kubeadm, kubelet, kubectl)
- Set up the node to join your cluster

**About VPN Interfaces:**

The Node Agent manages two WireGuard VPN tunnels:

- **wg0** - Kubernetes Internal Network
  - Used for pod-to-pod and node-to-node communication within the cluster
  - All cluster nodes communicate through this network
  - Automatically synchronized when nodes join or leave

- **wg1** - External User Access
  - Allows authorized users to access cluster services remotely
  - Enables secure connections from outside the cluster network
  - Both interfaces can communicate with each other

### Step 5: Start the Agent

Start the agent daemon:

```bash
sudo runos agent
```

The agent will:
- Connect to the RunOS control plane
- Begin accepting management commands
- Report node health and status
- Maintain VPN connectivity (both wg0 and wg1 interfaces)
- Automatically sync VPN peers as nodes join or leave the cluster

## Running as a System Service

For production use, run the agent as a systemd service:

### Create Service File

Create `/etc/systemd/system/runos.service`:

```ini
[Unit]
Description=RunOS Node Agent
After=network.target
Wants=network-online.target

[Service]
Type=simple
ExecStart=/usr/local/bin/runos agent
Restart=always
RestartSec=10
User=root

[Install]
WantedBy=multi-user.target
```

### Enable and Start Service

```bash
sudo systemctl daemon-reload
sudo systemctl enable runos.service
sudo systemctl start runos.service
sudo systemctl status runos.service
```

## Verifying Installation

Check that the agent is running correctly:

```bash
# Check agent status
runos status

# View version information
runos version

# Check systemd service (if using systemd)
sudo systemctl status runos.service

# Verify VPN interfaces are up
ip addr show wg0
ip addr show wg1

# Check VPN peer connectivity
sudo wg show
```

**Expected VPN Configuration:**

```bash
# wg0 should show internal Kubernetes network
wg0: flags=209<UP,POINTOPOINT,RUNNING,NOARP>  mtu 1420
        inet 172.24.x.x  netmask 255.255.0.0

# wg1 should show user access network
wg1: flags=209<UP,POINTOPOINT,RUNNING,NOARP>  mtu 1420
        inet 172.24.200.x  netmask 255.255.248.0
```

## Next Steps

- Review [Configuration](configuration.md) options
- Learn about available [Commands](commands.md)
- Set up [Log Monitoring](logs-monitoring.md)
- Understand [Connectivity](connectivity.md) requirements
