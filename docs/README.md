# RunOS Node Agent Documentation

Welcome to the RunOS Node Agent documentation. The Node Agent is a lightweight daemon that runs on each node in your RunOS cluster, enabling seamless communication with the RunOS control plane and managing your Kubernetes infrastructure.

## Documentation Overview

- **[Getting Started](getting-started.md)** - Installation and initial setup
- **[Configuration](configuration.md)** - Configure the agent for your environment
- **[Commands Reference](commands.md)** - Available commands and usage
- **[Connectivity](connectivity.md)** - How the agent connects to RunOS services
- **[Logs & Monitoring](logs-monitoring.md)** - Finding and understanding logs
- **[Troubleshooting](troubleshooting.md)** - Common issues and solutions
- **[Maintenance](maintenance.md)** - Log rotation and routine maintenance

## Quick Start

```bash
# Register your node with RunOS
./nodeagent register --token <YOUR_TOKEN> --aid <ACCOUNT_ID>

# Start the agent
./nodeagent agent

# Check agent status
./nodeagent status
```

## What is the Node Agent?

The Node Agent is responsible for:

- **Secure Communication** - Maintains encrypted connections to RunOS control plane
- **Cluster Management** - Executes Kubernetes operations on your nodes
- **VPN Network Management** - Synchronizes and manages two WireGuard VPN interfaces:
  - **wg0** (172.24.0.0/16) - Internal Kubernetes node-to-node communication
  - **wg1** (172.24.200.0/21) - External user access to cluster services
- **Health Monitoring** - Reports node status and health metrics
- **Configuration Sync** - Keeps your nodes in sync with desired state

The agent automatically manages VPN connectivity, ensuring all cluster nodes can communicate securely and authorized users can access cluster services remotely.

## System Requirements

- **Operating System**: Linux (Ubuntu 20.04+, Debian 10+, RHEL 8+, or compatible)
- **Architecture**: x86_64 (amd64) or ARM64
- **Network**: Outbound HTTPS access (ports 443, 9191, 9192)
- **Permissions**: Must run with root privileges
- **Disk Space**: Minimum 100MB for agent and logs

## Support

For issues or questions:
- Check the [Troubleshooting Guide](troubleshooting.md)
- Review [logs](logs-monitoring.md) for error messages
- Contact RunOS support with relevant log excerpts
