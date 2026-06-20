# Configuration

The RunOS Node Agent stores its configuration in `/etc/runos/config.yaml`. This file is automatically created during registration but can be customized for your environment.

## Configuration File Location

**Default location**: `/etc/runos/config.yaml`

This file must be readable by the root user.

## Configuration Options

### Basic Configuration

```yaml
node:
  nid: <node-id>                  # Node ID (assigned during registration)
  ip: <external-ip>               # External IP address (auto-detected if omitted)

client:
  aid: <account-id>               # Your RunOS account ID
  server:
    nodeward: nodeward.runos.com  # RunOS control plane server
    installer: https://get.runos.com  # Component installer server
```

### Configuration Parameters

#### Node Section

- **nid** (string, required)
  - Node identifier assigned during registration
  - Uniquely identifies this node in your RunOS cluster
  - Do not modify after registration

- **ip** (string, optional)
  - External IP address of this node
  - Auto-detected if not specified
  - Override if auto-detection fails or behind NAT

#### Client Section

- **aid** (string, required)
  - Your RunOS account identifier
  - Set during registration

- **server.nodeward** (string, optional)
  - RunOS control plane hostname
  - Default: `nodeward.runos.com`
  - Override for private deployments

- **server.installer** (string, optional)
  - Component installation server URL
  - Default: `https://get.runos.com`
  - Override for air-gapped installations

## Environment Variables

Configuration values can be overridden using environment variables:

### Log Level

Control logging verbosity:

```bash
export RUNOS_LOG_LEVEL=info    # Options: debug, info, warn, error
```

**Log levels:**
- `debug` - Detailed diagnostic information
- `info` - General operational messages (default)
- `warn` - Warning messages for potential issues
- `error` - Error messages only

### Running with Environment Variables

```bash
# Start agent with debug logging
RUNOS_LOG_LEVEL=debug runos agent

# Use with systemd
sudo systemctl edit runos
```

Add to the systemd override:
```ini
[Service]
Environment="RUNOS_LOG_LEVEL=debug"
```

## Certificate Files

The agent uses TLS certificates for secure communication. These are stored in `/etc/runos/`:

- **mtls.crt** - Client certificate for authenticated connections
- **mtls.key** - Private key for client certificate
- **ca.crt** - Certificate authority certificate
- **l1sec-ca.runos.public.pem** - Public CA for initial registration

**Important:**
- These files are created automatically during registration
- Keep the `.key` file secure - it contains sensitive cryptographic material
- Do not modify or delete these files while the agent is running
- Backup these files for disaster recovery

## Network Configuration

The agent requires outbound connectivity to:

- **Port 9191** - Initial registration and certificate exchange
- **Port 9192** - Operational commands and management
- **Port 443** - Component downloads and updates

Ensure firewall rules allow outbound connections to:
- `nodeward.runos.com`
- `get.runos.com`

## Configuration Best Practices

1. **Backup configuration** - Keep copies of `/etc/runos/config.yaml` and certificates
2. **Secure permissions** - Ensure config files are only readable by root
   ```bash
   sudo chmod 600 /etc/runos/config.yaml
   sudo chmod 600 /etc/runos/mtls.key
   ```
3. **Monitor logs** - Watch for configuration-related errors after changes
4. **Test changes** - Restart the agent and verify connectivity after configuration updates
5. **Version control** - Track configuration changes for auditing

## Reconfiguring the Agent

If you need to reconfigure the agent:

1. Stop the agent:
   ```bash
   sudo systemctl stop runos
   ```

2. Edit the configuration:
   ```bash
   sudo nano /etc/runos/config.yaml
   ```

3. Restart the agent:
   ```bash
   sudo systemctl start runos
   ```

4. Verify operation:
   ```bash
   sudo systemctl status runos
   runos status
   ```

## Configuration Troubleshooting

**Agent fails to start after configuration change:**
- Check YAML syntax (indentation, colons, quotes)
- Verify file permissions
- Review logs in `/var/log/runos.log`

**Cannot connect to control plane:**
- Verify `server.nodeward` hostname is correct
- Check network connectivity to port 9192
- Ensure certificates are valid and not expired

**IP address detection issues:**
- Manually set `node.ip` in configuration
- Verify external IP is reachable from other nodes
- Check for NAT or firewall interference
