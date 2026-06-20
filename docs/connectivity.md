# Connectivity & Networking

This guide explains how the RunOS Node Agent connects to backend services and what networking requirements are needed.

## Overview

The Node Agent maintains secure connections to RunOS backend services to enable cluster management and inter-node communication.

## Connection Architecture

```
Your Node                    RunOS Backend
    |                              |
    |---- Registration (Port 9191) --->|  (Initial setup only)
    |<--- Certificates ------------|
    |                              |
    |---- Secure Channel (Port 9192) -->|  (Ongoing operations)
    |<--- Management Commands -----|
    |---- Status Updates --------->|
    |<--- Configuration Sync ------|
    |                              |
    +-- WireGuard VPN ---------> Other Nodes
```

## Backend Services

### 1. Registration Service (L1Sec)

**Hostname**: `nodeward.runos.com`
**Port**: `9191`
**Protocol**: HTTPS with TLS
**Purpose**: Initial node registration and certificate issuance

**Used for:**
- Node registration (`runos register`)
- Obtaining security certificates
- Initial authentication

**Connection frequency**: Only during registration (one-time operation)

**Security**: TLS encryption protects the registration token and certificate exchange

---

### 2. Management Service (L2Sec)

**Hostname**: `nodeward.runos.com`
**Port**: `9192`
**Protocol**: gRPC with mutual TLS (mTLS)
**Purpose**: Ongoing cluster management and operations

**Used for:**
- Receiving management commands from RunOS control plane
- Sending node status updates
- Heartbeat monitoring
- Configuration synchronization
- Kubernetes operations

**Connection frequency**: Persistent connection while agent is running

**Security**:
- Mutual TLS authentication (both client and server verify certificates)
- Encrypted bidirectional communication
- Certificate-based authentication prevents unauthorized access

---

### 3. Installer Service

**Hostname**: `get.runos.com`
**Port**: `443`
**Protocol**: HTTPS
**Purpose**: Component downloads and updates

**Used for:**
- Downloading Kubernetes components
- Installing WireGuard VPN
- Fetching configuration templates
- Agent updates

**Connection frequency**: During installation and updates

---

## Network Requirements

### Outbound Connectivity

Your nodes must allow **outbound** connections to:

| Service | Hostname | Port | Protocol | Purpose |
|---------|----------|------|----------|---------|
| Registration | nodeward.runos.com | 9191 | HTTPS/TLS | Initial registration |
| Management | nodeward.runos.com | 9192 | gRPC/mTLS | Operations |
| Installer | get.runos.com | 443 | HTTPS | Downloads |

**No inbound connections required** - The agent only makes outbound connections to RunOS services.

### Firewall Configuration

**For iptables:**
```bash
# Allow outbound to RunOS services
sudo iptables -A OUTPUT -p tcp -d nodeward.runos.com --dport 9191 -j ACCEPT
sudo iptables -A OUTPUT -p tcp -d nodeward.runos.com --dport 9192 -j ACCEPT
sudo iptables -A OUTPUT -p tcp -d get.runos.com --dport 443 -j ACCEPT

# Save rules
sudo iptables-save > /etc/iptables/rules.v4
```

**For UFW:**
```bash
# UFW typically allows outbound by default
# If you have restricted outbound rules:
sudo ufw allow out to nodeward.runos.com port 9191 proto tcp
sudo ufw allow out to nodeward.runos.com port 9192 proto tcp
sudo ufw allow out to get.runos.com port 443 proto tcp
```

**For cloud providers:**
- AWS: Configure Security Groups to allow outbound HTTPS
- Azure: Configure Network Security Groups (NSGs)
- GCP: Configure firewall rules for egress traffic
- Most cloud providers allow outbound by default

### Proxy Configuration

If your environment requires an HTTP proxy:

**Set environment variables:**
```bash
export HTTPS_PROXY=http://proxy.example.com:8080
export NO_PROXY=localhost,127.0.0.1
```

**For systemd service:**
```bash
sudo systemctl edit runos
```

Add:
```ini
[Service]
Environment="HTTPS_PROXY=http://proxy.example.com:8080"
Environment="NO_PROXY=localhost,127.0.0.1"
```

Then reload and restart:
```bash
sudo systemctl daemon-reload
sudo systemctl restart runos
```

## Connection Lifecycle

### 1. Registration Phase

```
Node Agent                    RunOS Backend (Port 9191)
    |                                |
    |-- POST /register -------------→| (with token)
    |                                |
    |← Certificate + Config ---------| (mTLS certs)
    |                                |
```

**What happens:**
1. Agent sends registration token and node information
2. Backend validates token and account
3. Backend generates unique mTLS certificate for this node
4. Agent receives and stores certificates in `/etc/runos/`

**One-time operation** - Only needs to happen once per node

---

### 2. Operational Phase

```
Node Agent                    RunOS Backend (Port 9192)
    |                                |
    |-- Connect with mTLS ----------→|
    |                                |
    |← Management Commands ----------|
    |                                |
    |-- Status Updates -------------→|
    |                                |
    |-- Heartbeat (every 5s) -------→|
    |                                |
```

**Persistent connection:**
- Bidirectional gRPC stream stays open
- Agent receives commands in real-time
- Agent sends status updates continuously
- Heartbeats every 5 seconds prove node is alive

**Auto-reconnect:**
- If connection drops, agent automatically reconnects
- Exponential backoff prevents connection storms
- No manual intervention required

---

### 3. Heartbeat Mechanism

The agent sends heartbeat messages every 5 seconds:

**Heartbeat contains:**
- Node ID
- Timestamp
- Current status (healthy/degraded)
- Resource availability

**Purpose:**
- Detect node failures quickly
- Trigger alerts for offline nodes
- Update node status in RunOS console

**If heartbeats stop:**
- Control plane marks node as offline after 30 seconds
- Alerts may be triggered
- Cluster may reschedule workloads

---

## Inter-Node Communication (VPN)

The Node Agent is **responsible for managing and synchronizing VPN connectivity** between all nodes in your cluster. It maintains two separate WireGuard VPN interfaces for different purposes.

### Dual VPN Architecture

The agent manages two WireGuard interfaces:

```
Node A                          Node B                          Node C
├─ wg0: 172.24.1.10            ├─ wg0: 172.24.1.20            ├─ wg0: 172.24.1.30
│  (Kubernetes Internal)       │  (Kubernetes Internal)       │  (Kubernetes Internal)
│                               │                               │
│  ←──────── Encrypted ────────┼──────── Tunnels ──────────────→
│            Tunnels            │                               │
│                               │                               │
└─ wg1: 172.24.200.1           └─ wg1: 172.24.200.2           └─ wg1: 172.24.200.3
   (User Access)                  (User Access)                  (User Access)
        ↑                              ↑                              ↑
        │                              │                              │
        └──────────────────────────────┴──────────────────────────────┘
                          Remote Users/Admins
```

### wg0 - Kubernetes Internal Network

**Purpose**: Internal cluster communication between Kubernetes nodes

**Interface**: `wg0`
**IP Range**: `172.24.0.0/16` (172.24.1.0 - 172.24.255.255)
**Subnet Mask**: `255.255.0.0` (/16)

**Used for:**
- Pod-to-pod communication across nodes
- Node-to-node Kubernetes traffic
- Internal service mesh communication
- etcd cluster communication (control plane nodes)
- Kubernetes API server traffic

**Management:**
- The Node Agent automatically synchronizes all wg0 peers
- When a node joins the cluster, it's added to all existing nodes' wg0 configuration
- When a node leaves, it's removed from all wg0 peer lists
- Synchronization happens automatically via the control plane

View your wg0 configuration:
```bash
ip addr show wg0
# Example output:
# wg0: flags=209<UP,POINTOPOINT,RUNNING,NOARP>  mtu 1420
#         inet 172.24.1.81  netmask 255.255.0.0
```

### wg1 - External User Access Network

**Purpose**: Secure remote access for users and administrators

**Interface**: `wg1`
**IP Range**: `172.24.200.0/21` (172.24.200.0 - 172.24.207.255)
**Subnet Mask**: `255.255.248.0` (/21)

**Used for:**
- Remote user access to cluster services
- Administrator access to internal resources
- Secure connectivity from outside the cluster network
- Access to internal APIs and dashboards

**Inter-communication:**
- Traffic from wg1 can reach wg0 (users can access cluster services)
- Traffic from wg0 can reach wg1 (services can respond to users)
- Both networks are routable to each other

View your wg1 configuration:
```bash
ip addr show wg1
# Example output:
# wg1: flags=209<UP,POINTOPOINT,RUNNING,NOARP>  mtu 1420
#         inet 172.24.200.1  netmask 255.255.248.0
```

### Key VPN Features

**Common to both interfaces:**
- Encrypted tunnels (WireGuard protocol)
- Works behind NAT and firewalls
- UDP-based (typically port 51820 for wg0, 51821 for wg1)
- Automatic peer discovery and configuration
- Persistent keepalive (5 seconds) maintains connections
- MTU 1420 to prevent fragmentation

**Automatic Synchronization:**
The Node Agent continuously synchronizes VPN configurations:
- Polls control plane for peer updates
- Applies changes to WireGuard configuration
- Establishes tunnels to new peers
- Removes stale peer configurations

### VPN Peer Synchronization Process

The Node Agent handles all VPN peer synchronization automatically:

**For wg0 (Kubernetes Internal):**
1. Agent retrieves its wg0 public key and IP address
2. Sends this information to RunOS control plane
3. Receives list of all other cluster nodes' public keys and wg0 IPs
4. Configures WireGuard to create encrypted tunnels to all peers
5. Repeats when cluster topology changes

**For wg1 (User Access):**
1. Agent retrieves its wg1 public key and IP address
2. Sends this information to RunOS control plane
3. Receives list of authorized users' public keys and wg1 IPs
4. Configures WireGuard to allow user connections
5. Updates as users are added or removed

**Automatic synchronization occurs:**
- On agent startup
- When nodes join or leave the cluster
- When users are added or removed
- Periodically (every few minutes)
- Can be manually triggered: `sudo runos sync vpn`

### Testing VPN Connectivity

**View all WireGuard interfaces:**
```bash
# Show both wg0 and wg1 status
sudo wg show

# Show only wg0 (Kubernetes internal)
sudo wg show wg0

# Show only wg1 (user access)
sudo wg show wg1
```

**List VPN peers:**
```bash
# List all wg0 peers (other cluster nodes)
sudo wg show wg0 peers

# List all wg1 peers (connected users)
sudo wg show wg1 peers

# Show detailed peer information
sudo wg show wg0 endpoints
sudo wg show wg0 allowed-ips
```

**Test connectivity:**
```bash
# Test connectivity to another node via wg0
ping 172.24.1.x  # Replace with actual peer wg0 IP

# Test connectivity to user network via wg1
ping 172.24.200.x  # Replace with actual wg1 IP

# Check tunnel traffic statistics
sudo wg show wg0 transfer
sudo wg show wg1 transfer

# Verify both interfaces are up
ip addr show wg0
ip addr show wg1
```

## Connection Security

### Certificate-Based Authentication

**Registration (Port 9191):**
- TLS encryption protects data in transit
- Registration token authenticates initial request
- Short-lived tokens prevent replay attacks

**Operations (Port 9192):**
- Mutual TLS (mTLS) requires both sides to authenticate
- Agent presents client certificate
- Server validates certificate against CA
- Prevents man-in-the-middle attacks

### Certificate Management

**Location**: `/etc/runos/`

**Files:**
- `mtls.crt` - Client certificate (identifies this node)
- `mtls.key` - Private key (keep secure!)
- `ca.crt` - Certificate Authority certificate
- `l1sec-ca.runos.public.pem` - Public CA for registration

**Certificate validity**: Typically 1 year

**Renewal**: Contact RunOS support before expiration or re-register the node

**Security best practices:**
```bash
# Verify permissions
sudo chmod 600 /etc/runos/mtls.key
sudo chmod 644 /etc/runos/mtls.crt

# Check expiration date
openssl x509 -in /etc/runos/mtls.crt -noout -dates
```

## Bandwidth Usage

### Typical Traffic Patterns

**During normal operations:**
- Heartbeats: ~1 KB every 5 seconds (~17 KB/min)
- Status updates: ~2-5 KB per minute
- Command reception: Variable, typically <10 KB/min
- **Total**: Usually <1 MB/hour per node

**During cluster operations:**
- Kubernetes deployments: 10-100 KB per operation
- Log streaming: Variable based on verbosity
- Configuration syncs: 5-50 KB per sync

**VPN traffic:**
- Depends on inter-node Kubernetes traffic
- Encrypted overlay adds ~60 bytes per packet
- Typical: 10-500 KB/s during active workloads

### Bandwidth Requirements

**Minimum**: 128 Kbps sustained
**Recommended**: 1 Mbps or higher
**Latency**: <200ms to RunOS backend preferred

## Testing Connectivity

### Quick connectivity test:

```bash
#!/bin/bash
echo "Testing RunOS Backend Connectivity..."

# Test DNS resolution
echo -n "DNS resolution: "
if nslookup nodeward.runos.com > /dev/null 2>&1; then
    echo "OK"
else
    echo "FAILED"
fi

# Test port 9191 (registration)
echo -n "Port 9191 (registration): "
if nc -zv nodeward.runos.com 9191 2>&1 | grep -q succeeded; then
    echo "OK"
else
    echo "FAILED"
fi

# Test port 9192 (management)
echo -n "Port 9192 (management): "
if nc -zv nodeward.runos.com 9192 2>&1 | grep -q succeeded; then
    echo "OK"
else
    echo "FAILED"
fi

# Test installer service
echo -n "Installer service (443): "
if curl -sSf https://get.runos.com/health > /dev/null 2>&1; then
    echo "OK"
else
    echo "FAILED"
fi

echo "Done."
```

Save as `test-connectivity.sh` and run:
```bash
chmod +x test-connectivity.sh
./test-connectivity.sh
```

## NAT and Private Networks

The Node Agent works behind NAT:

**VPN handles NAT traversal:**
- WireGuard's UDP hole-punching handles most NAT scenarios
- Persistent keepalive (5 seconds) maintains NAT mappings
- Endpoint discovery allows direct peer-to-peer connections

**Requirements:**
- Outbound UDP allowed (for WireGuard)
- Outbound TCP to RunOS backend (ports 9191, 9192, 443)

**No port forwarding needed** - All connections are outbound-initiated

## Troubleshooting Connectivity

**Cannot reach backend services:**
```bash
# Test DNS
nslookup nodeward.runos.com

# Test port connectivity
nc -zv nodeward.runos.com 9192

# Check firewall rules
sudo iptables -L OUTPUT -v -n | grep 9192

# Test with curl
curl -v https://nodeward.runos.com:9191
```

**VPN peer connectivity issues:**
```bash
# Check both WireGuard interfaces
sudo wg show

# Check wg0 (Kubernetes internal) specifically
sudo wg show wg0
ip addr show wg0

# Check wg1 (user access) specifically
sudo wg show wg1
ip addr show wg1

# Test peer reachability on wg0
ping 172.24.1.x  # Replace with peer's wg0 IP

# Test wg1 connectivity
ping 172.24.200.x  # Replace with wg1 IP

# Resync both VPN interfaces
sudo runos sync vpn

# Check for firewall blocking UDP
sudo iptables -L OUTPUT -v -n | grep udp

# Verify routing between wg0 and wg1
ip route | grep -E "172.24"
```

**Connection keeps dropping:**
```bash
# Check for network instability
ping -c 100 nodeward.runos.com | grep loss

# Look for connection errors in logs
sudo grep -i "connection\|disconnect" /var/log/runos.log

# Check system time accuracy (TLS requires accurate time)
date
```

For more troubleshooting, see the [Troubleshooting Guide](troubleshooting.md).
