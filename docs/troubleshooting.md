# Troubleshooting Guide

This guide helps you diagnose and resolve common issues with the RunOS Node Agent.

## Quick Diagnostic Steps

When experiencing issues, start with these basic checks:

```bash
# 1. Check if agent is running
sudo systemctl status runos

# 2. View recent logs
sudo journalctl -u runos -n 50

# 3. Check connectivity
ping -c 3 nodeward.runos.com

# 4. Verify configuration
sudo cat /etc/runos/config.yaml

# 5. Check disk space
df -h /var/log
```

## Common Issues

### Agent Won't Start

**Symptoms:**
- Service fails to start
- Agent exits immediately after launch
- Error: "Failed to start runos.service"

**Possible causes and solutions:**

1. **Missing configuration file**
   ```bash
   # Check if config exists
   sudo ls -l /etc/runos/config.yaml

   # If missing, re-register the node
   sudo runos register --token <TOKEN> --aid <ACCOUNT> --control-plane 0
   ```

2. **Missing certificates**
   ```bash
   # Check for certificate files
   sudo ls -l /etc/runos/mtls.*

   # If missing, re-register to obtain new certificates
   sudo runos register --token <TOKEN> --aid <ACCOUNT> --control-plane 0
   ```

3. **Permission issues**
   ```bash
   # Fix permissions
   sudo chown root:root /etc/runos/config.yaml
   sudo chmod 600 /etc/runos/config.yaml
   sudo chmod 600 /etc/runos/mtls.key
   ```

4. **Port conflicts**
   ```bash
   # Check if port 6446 is in use
   sudo netstat -tlnp | grep 6446

   # If occupied by another process, stop that process
   ```

5. **Invalid configuration syntax**
   ```bash
   # View logs for syntax errors
   sudo journalctl -u runos -n 20

   # Common issues: incorrect YAML indentation, missing colons
   # Validate YAML syntax online or with a linter
   ```

---

### Cannot Connect to Control Plane

**Symptoms:**
- Agent starts but shows "Failed to connect"
- No heartbeat messages in logs
- Error: "connection timeout" or "connection refused"

**Diagnostic steps:**

1. **Check network connectivity**
   ```bash
   # Ping control plane server
   ping -c 5 nodeward.runos.com

   # Test specific ports
   nc -zv nodeward.runos.com 9192
   nc -zv nodeward.runos.com 9191

   # Check DNS resolution
   nslookup nodeward.runos.com
   ```

2. **Verify firewall rules**
   ```bash
   # Check outbound firewall rules
   sudo iptables -L OUTPUT -v -n

   # Ensure ports 9191, 9192 are allowed outbound
   ```

3. **Check certificate validity**
   ```bash
   # View certificate expiration
   openssl x509 -in /etc/runos/mtls.crt -noout -dates

   # If expired, re-register
   sudo runos register --token <TOKEN> --aid <ACCOUNT> --control-plane 0
   ```

4. **Verify system time**
   ```bash
   # Check if system time is accurate (TLS requires accurate time)
   date

   # If incorrect, update system time
   sudo ntpdate -u pool.ntp.org
   # Or use: sudo timedatectl set-ntp true
   ```

5. **Test with debug logging**
   ```bash
   # Enable debug mode
   sudo RUNOS_LOG_LEVEL=debug runos agent

   # Look for specific connection errors
   ```

---

### VPN Connectivity Issues

**Symptoms:**
- Cannot reach other nodes
- VPN peers not showing up
- WireGuard interface down
- Cannot access cluster services remotely

**Important:** The Node Agent manages **two VPN interfaces**:
- **wg0** (172.24.0.0/16) - Kubernetes internal node-to-node communication
- **wg1** (172.24.200.0/21) - External user access to cluster services

**Solutions:**

1. **Check both WireGuard interfaces**
   ```bash
   # Check if both interfaces are running
   sudo wg show

   # Check wg0 (Kubernetes internal)
   ip addr show wg0
   # Should show: inet 172.24.x.x  netmask 255.255.0.0

   # Check wg1 (user access)
   ip addr show wg1
   # Should show: inet 172.24.200.x  netmask 255.255.248.0

   # Verify both interfaces are UP
   ip link show wg0
   ip link show wg1
   ```

2. **Identify which interface has issues**
   ```bash
   # Check wg0 peer count (should match number of cluster nodes)
   sudo wg show wg0 peers | wc -l

   # Check wg1 peer count (should match number of connected users)
   sudo wg show wg1 peers | wc -l

   # View detailed status
   sudo wg show wg0
   sudo wg show wg1
   ```

3. **Synchronize VPN peers**
   ```bash
   # Force VPN sync for both interfaces
   sudo runos sync vpn

   # Wait 30 seconds, then verify
   sudo wg show wg0 peers
   sudo wg show wg1 peers
   ```

4. **Test connectivity on each interface**
   ```bash
   # Test wg0 (node-to-node) - use another node's wg0 IP
   ping -c 3 172.24.1.x

   # Test wg1 (user access) - use wg1 IP
   ping -c 3 172.24.200.x

   # Check if both networks can communicate
   # From a wg0 IP, try to reach wg1, and vice versa
   ```

5. **Verify WireGuard installation**
   ```bash
   # Check if WireGuard is installed
   which wg

   # Check WireGuard version
   wg --version

   # If not installed, reinstall
   sudo runos install
   ```

6. **Check kernel module**
   ```bash
   # Verify WireGuard kernel module is loaded
   lsmod | grep wireguard

   # If not loaded, load it manually
   sudo modprobe wireguard
   ```

7. **Check routing between interfaces**
   ```bash
   # Verify routes for both networks
   ip route | grep 172.24

   # Should see routes for both 172.24.0.0/16 and 172.24.200.0/21
   ```

8. **Review VPN logs in agent logs**
   ```bash
   # Look for VPN-related errors
   sudo grep -i "vpn\|wireguard\|wg0\|wg1" /var/log/runos.log | tail -50

   # Check for synchronization messages
   sudo grep -i "sync" /var/log/runos.log | tail -20
   ```

**Interface-specific troubleshooting:**

**For wg0 issues (cannot reach other nodes):**
- Verify all cluster nodes are registered and running
- Check that the node count matches peer count
- Ensure firewall allows UDP port 51820

**For wg1 issues (users cannot access cluster):**
- Verify users are properly configured in RunOS console
- Check that user VPN clients have correct configuration
- Ensure firewall allows UDP port 51821
- Verify routing allows wg1 to reach wg0 network

---

### High CPU or Memory Usage

**Symptoms:**
- Agent consuming excessive CPU
- High memory usage
- System slowdown

**Diagnostic steps:**

1. **Check resource usage**
   ```bash
   # View agent resource consumption
   top -p $(pgrep runos)

   # Or use htop
   htop -p $(pgrep runos)
   ```

2. **Check for log flooding**
   ```bash
   # Check log write rate
   sudo tail -f /var/log/runos.log | pv > /dev/null

   # If excessive, reduce log level
   sudo systemctl edit runos
   # Add: Environment="RUNOS_LOG_LEVEL=warn"
   sudo systemctl restart runos
   ```

3. **Review recent commands**
   ```bash
   # Look for stuck operations
   sudo grep "Executing instruction" /var/log/runos.log | tail -20
   ```

4. **Restart the agent**
   ```bash
   sudo systemctl restart runos
   ```

---

### Registration Failures

**Symptoms:**
- "Invalid token" error
- "Registration failed" message
- Cannot obtain certificates

**Solutions:**

1. **Verify token validity**
   - Check that token hasn't expired
   - Obtain fresh token from RunOS console
   - Ensure token is for correct account

2. **Check account ID**
   ```bash
   # Verify account ID format (should be: acct_xxxxx)
   # Get correct account ID from RunOS console
   ```

3. **Network connectivity during registration**
   ```bash
   # Test connection to registration endpoint
   curl -I https://nodeward.runos.com:9191
   ```

4. **Clear previous registration**
   ```bash
   # Remove old config and certificates
   sudo rm -rf /etc/runos/*

   # Try registration again
   sudo runos register --token <TOKEN> --aid <ACCOUNT> --control-plane 0
   ```

---

### Installation Failures

**Symptoms:**
- `runos install` command fails
- Kubernetes components not installed
- VPN not configured

**Solutions:**

1. **Check internet connectivity**
   ```bash
   # Test access to installer server
   curl -I https://get.runos.com

   # Check apt/yum repositories
   sudo apt-get update  # Debian/Ubuntu
   sudo yum check-update  # RHEL/CentOS
   ```

2. **Verify system requirements**
   ```bash
   # Check OS version
   cat /etc/os-release

   # Check architecture
   uname -m  # Should be x86_64 or aarch64
   ```

3. **Check disk space**
   ```bash
   # Ensure sufficient space
   df -h /
   df -h /var

   # Minimum 10GB free recommended
   ```

4. **Review installation logs**
   ```bash
   # Check logs during installation
   sudo runos install 2>&1 | tee install.log

   # Review for specific error messages
   ```

5. **Retry installation**
   ```bash
   # Clean up partial installation
   sudo runos uninstall

   # Reboot and retry
   sudo reboot
   # After reboot:
   sudo runos install
   ```

---

## Advanced Troubleshooting

### Enable Debug Logging

For detailed diagnostic information:

1. **Temporary debug mode:**
   ```bash
   sudo RUNOS_LOG_LEVEL=debug runos agent
   ```

2. **Permanent debug mode (systemd):**
   ```bash
   sudo systemctl edit runos
   ```

   Add:
   ```ini
   [Service]
   Environment="RUNOS_LOG_LEVEL=debug"
   ```

   Save and restart:
   ```bash
   sudo systemctl daemon-reload
   sudo systemctl restart runos
   ```

3. **Return to normal logging:**
   ```bash
   sudo systemctl edit runos
   # Remove the Environment line
   sudo systemctl daemon-reload
   sudo systemctl restart runos
   ```

### Collect Diagnostic Information

When reporting issues to support:

```bash
#!/bin/bash
# Create diagnostic bundle

OUTFILE=~/nodeagent-diagnostics-$(date +%Y%m%d-%H%M%S).txt

echo "=== System Information ===" > $OUTFILE
uname -a >> $OUTFILE
cat /etc/os-release >> $OUTFILE

echo -e "\n=== Agent Version ===" >> $OUTFILE
runos version >> $OUTFILE

echo -e "\n=== Service Status ===" >> $OUTFILE
sudo systemctl status runos >> $OUTFILE 2>&1

echo -e "\n=== Recent Logs ===" >> $OUTFILE
sudo journalctl -u runos -n 500 >> $OUTFILE 2>&1

echo -e "\n=== Configuration ===" >> $OUTFILE
sudo cat /etc/runos/config.yaml >> $OUTFILE 2>&1

echo -e "\n=== Network Connectivity ===" >> $OUTFILE
ping -c 5 nodeward.runos.com >> $OUTFILE 2>&1
nc -zv nodeward.runos.com 9192 >> $OUTFILE 2>&1

echo -e "\n=== Certificate Info ===" >> $OUTFILE
sudo openssl x509 -in /etc/runos/mtls.crt -noout -text >> $OUTFILE 2>&1

echo -e "\n=== WireGuard Status ===" >> $OUTFILE
sudo wg show >> $OUTFILE 2>&1

echo -e "\n=== Disk Space ===" >> $OUTFILE
df -h >> $OUTFILE

echo "Diagnostics saved to: $OUTFILE"
```

### Reset Agent Completely

If all else fails, completely reset the agent:

```bash
# 1. Stop the agent
sudo systemctl stop runos
sudo systemctl disable runos

# 2. Uninstall components
sudo runos uninstall

# 3. After reboot, remove all configuration
sudo rm -rf /etc/runos/
sudo rm -f /var/log/runos.log*

# 4. Re-register from scratch
sudo runos register --token <TOKEN> --aid <ACCOUNT> --control-plane 0

# 5. Reinstall
sudo runos install

# 6. Restart agent
sudo systemctl enable runos
sudo systemctl start runos
```

## Getting Help

If you cannot resolve the issue:

1. **Collect diagnostic information** (see above)
2. **Review error messages** in logs
3. **Check RunOS status page** for service incidents
4. **Contact RunOS support** with:
   - Description of the problem
   - Steps to reproduce
   - Diagnostic bundle
   - Agent version and OS information

## Preventive Maintenance

Avoid issues with regular maintenance:

- **Monitor disk space** - Ensure adequate free space
- **Keep system updated** - Apply security patches
- **Check logs regularly** - Watch for warnings
- **Test connectivity** - Periodically verify network access
- **Backup configuration** - Save `/etc/runos/` directory
- **Update agent** - Keep agent updated to latest version
