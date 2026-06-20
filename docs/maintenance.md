# Maintenance

Regular maintenance ensures your RunOS Node Agent runs smoothly and efficiently.

## Routine Maintenance Tasks

### Daily Tasks

**Monitor agent status:**
```bash
# Check if agent is running
sudo systemctl status runos

# Quick log check for errors
sudo journalctl -u runos --since today -p err
```

### Weekly Tasks

**Review logs:**
```bash
# Check for warning messages
sudo journalctl -u runos --since "7 days ago" -p warn

# Look for connection issues
sudo grep -i "connect\|error" /var/log/runos.log | tail -50

# Check log disk usage
du -sh /var/log/runos.log*
```

**Verify VPN connectivity:**
```bash
# Check peer count
sudo wg show wg0 | grep peer

# Test connectivity to other nodes
# (Get peer IPs from: sudo wg show wg0 endpoints)
ping -c 3 <peer-wg0-ip>
```

### Monthly Tasks

**Certificate expiration check:**
```bash
# Check certificate validity
openssl x509 -in /etc/runos/mtls.crt -noout -dates

# Should show dates like:
# notBefore=Jan 1 00:00:00 2025 GMT
# notAfter=Jan 1 00:00:00 2026 GMT

# Alert if expiring within 30 days
```

**Disk space cleanup:**
```bash
# Check disk usage
df -h /var/log

# Review old log files
ls -lh /var/log/runos.log*

# Remove old compressed logs if needed (optional)
sudo find /var/log -name "runos.log.*.gz" -mtime +60 -delete
```

**System updates:**
```bash
# Update system packages (example for Ubuntu/Debian)
sudo apt update
sudo apt upgrade -y

# Reboot if kernel was updated
sudo reboot
```

## Log Management

### Log Rotation Configuration

The agent uses logrotate to manage log files automatically.

**View current configuration:**
```bash
cat /etc/logrotate.d/runos
```

**Default settings:**
- Rotate daily
- Keep 7 days of logs
- Compress old logs
- Don't rotate empty logs

### Manual Log Rotation

Force immediate log rotation:
```bash
sudo logrotate -f /etc/logrotate.d/runos
```

### Checking Log Disk Usage

```bash
# Total log directory size
du -sh /var/log/runos.log*

# List individual log files
ls -lh /var/log/runos.log*

# Check disk space
df -h /var/log
```

### Cleaning Old Logs

**Automatic cleanup** (configured in logrotate):
```bash
# Edit rotation configuration
sudo nano /etc/logrotate.d/runos

# Change rotation period
rotate 30  # Keep 30 days instead of 7
```

**Manual cleanup:**
```bash
# Remove compressed logs older than 60 days
sudo find /var/log -name "runos.log.*.gz" -mtime +60 -delete

# Be careful with manual deletion - verify first
sudo find /var/log -name "runos.log.*.gz" -mtime +60 -ls
```

**Emergency cleanup** (when disk is full):
```bash
# Check what's using space
sudo du -sh /var/log/*

# Compress current log (if very large)
sudo gzip /var/log/runos.log

# Remove old archives
sudo rm /var/log/runos.log.*.gz

# Restart agent to create fresh log
sudo systemctl restart runos
```

## Updates

### Checking for Updates

Check the current agent version:
```bash
runos version
```

Check for new releases:
- Visit RunOS console for update notifications
- Subscribe to RunOS release announcements
- Check RunOS status page

### Updating the Agent

Updates are applied with the built-in `runos update` command. The agent ships as
attested binaries on GitHub Releases (github.com/runos-official/nodeagent); the
updater downloads the binary for the requested version, verifies its sha256, swaps
`/usr/local/bin/runos`, and restarts `runos.service`. There is no manual download
step.

**Update to the advertised version**
```bash
sudo runos update
```

**Pin to an exact version**
```bash
sudo runos update --version vX.Y.Z
```

The updater prints the previous and new versions on completion. If you are already
on the requested version it reports "No Update Available" and makes no changes.

**Verify operation after updating**
```bash
# Confirm the new version
runos version

# Confirm the service is running
sudo systemctl status runos

# Check logs for errors
sudo journalctl -u runos -n 50

# Verify connectivity
runos status
```

### Rolling Back Updates

If a version has issues, pin back to the previous known-good version:

```bash
sudo runos update --version vX.Y.Z
```

Substitute the exact tag of the version you want to return to (for example the
version reported by `runos version` before the upgrade).

## Backup and Recovery

### What to Backup

**Critical files:**
- `/etc/runos/config.yaml` - Configuration
- `/etc/runos/mtls.crt` - Client certificate
- `/etc/runos/mtls.key` - Private key
- `/etc/runos/ca.crt` - CA certificate

**Optional:**
- `/var/log/runos.log` - Recent logs (for troubleshooting)

### Creating a Backup

```bash
# Create backup directory
mkdir -p ~/runos-backup-$(date +%Y%m%d)

# Backup configuration and certificates
sudo cp -r /etc/runos/ ~/runos-backup-$(date +%Y%m%d)/

# Backup systemd service file (if customized)
sudo cp /etc/systemd/system/runos.service ~/runos-backup-$(date +%Y%m%d)/

# Create archive
tar -czf runos-backup-$(date +%Y%m%d).tar.gz ~/runos-backup-$(date +%Y%m%d)/

# Secure the backup (contains private key!)
chmod 600 runos-backup-$(date +%Y%m%d).tar.gz

# Store securely off the node
```

### Restoring from Backup

```bash
# Extract backup
tar -xzf runos-backup-20250115.tar.gz

# Stop the agent
sudo systemctl stop runos

# Restore configuration and certificates
sudo cp -r runos-backup-20250115/runos/* /etc/runos/

# Fix permissions
sudo chmod 600 /etc/runos/mtls.key
sudo chmod 644 /etc/runos/mtls.crt

# Restore systemd service (if needed)
sudo cp runos-backup-20250115/runos.service /etc/systemd/system/
sudo systemctl daemon-reload

# Start the agent
sudo systemctl start runos

# Verify
runos status
```

## Health Monitoring

### Setting Up Monitoring

**Basic monitoring script:**

Create `/usr/local/bin/check-nodeagent.sh`:
```bash
#!/bin/bash

# Check if agent is running
if ! systemctl is-active --quiet runos; then
    echo "CRITICAL: Node agent is not running"
    exit 2
fi

# Check for recent errors (last 5 minutes)
ERROR_COUNT=$(journalctl -u runos --since "5 minutes ago" -p err | wc -l)
if [ "$ERROR_COUNT" -gt 5 ]; then
    echo "WARNING: $ERROR_COUNT errors in last 5 minutes"
    exit 1
fi

# Check disk space
DISK_USAGE=$(df /var/log | tail -1 | awk '{print $5}' | sed 's/%//')
if [ "$DISK_USAGE" -gt 90 ]; then
    echo "WARNING: Disk usage at ${DISK_USAGE}%"
    exit 1
fi

echo "OK: Node agent healthy"
exit 0
```

Make it executable:
```bash
sudo chmod +x /usr/local/bin/check-nodeagent.sh
```

**Schedule with cron:**
```bash
# Edit crontab
sudo crontab -e

# Add monitoring (runs every 5 minutes)
*/5 * * * * /usr/local/bin/check-nodeagent.sh
```

### Metrics to Monitor

**Service health:**
- Agent process running
- Connection to control plane active
- Heartbeats being sent

**Resource usage:**
- CPU usage (should be <5% normally)
- Memory usage (typically 50-200 MB)
- Disk space for logs

**Networking:**
- VPN peer count (should match cluster size)
- Connection errors in logs
- Failed heartbeat attempts

**Certificates:**
- Days until expiration
- Certificate validity

## Performance Tuning

### Log Level Optimization

Reduce log verbosity for production:

```bash
# Edit systemd service
sudo systemctl edit runos
```

Set log level to `warn` or `error`:
```ini
[Service]
Environment="RUNOS_LOG_LEVEL=warn"
```

```bash
sudo systemctl daemon-reload
sudo systemctl restart runos
```

### Resource Limits

Set resource limits if needed:

```bash
sudo systemctl edit runos
```

Add limits:
```ini
[Service]
# Limit memory to 512MB
MemoryMax=512M

# Limit CPU to 50%
CPUQuota=50%
```

```bash
sudo systemctl daemon-reload
sudo systemctl restart runos
```

## Disaster Recovery

### Complete Node Failure

If the node fails completely:

1. **Provision new node**
2. **Restore backup** (if available)
3. **Or re-register:**
   ```bash
   sudo runos register --token <TOKEN> --aid <ACCOUNT> --control-plane 0
   sudo runos install
   sudo systemctl start runos
   ```

### Certificate Issues

If certificates are lost or corrupted:

```bash
# Re-register to get new certificates
sudo runos register --token <NEW_TOKEN> --aid <ACCOUNT> --control-plane 0

# This will replace /etc/runos/ certificates
# Then restart the agent
sudo systemctl restart runos
```

### Configuration Corruption

If config.yaml is corrupted:

```bash
# Restore from backup
sudo cp ~/backup/config.yaml /etc/runos/config.yaml

# Or re-register
sudo runos register --token <TOKEN> --aid <ACCOUNT> --control-plane 0
```

## Maintenance Windows

### Planning Maintenance

For minimal disruption:

1. **Schedule during low-activity periods**
2. **Notify users of planned maintenance**
3. **Backup before making changes**
4. **Test in development first** (if possible)
5. **Have rollback plan ready**

### Performing Maintenance

```bash
# 1. Backup current state
sudo tar -czf /root/runos-pre-maintenance-$(date +%Y%m%d).tar.gz /etc/runos/

# 2. Stop the agent
sudo systemctl stop runos

# 3. Perform maintenance (update, config changes, etc.)

# 4. Start the agent
sudo systemctl start runos

# 5. Verify operation
sudo systemctl status runos
runos status

# 6. Monitor logs for issues
sudo journalctl -u runos -f
```

## Automation

### Automated Backup Script

Create `/usr/local/bin/backup-nodeagent.sh`:
```bash
#!/bin/bash

BACKUP_DIR="/root/nodeagent-backups"
DATE=$(date +%Y%m%d-%H%M%S)

mkdir -p "$BACKUP_DIR"

# Create backup
tar -czf "$BACKUP_DIR/nodeagent-$DATE.tar.gz" /etc/runos/

# Keep only last 30 days
find "$BACKUP_DIR" -name "nodeagent-*.tar.gz" -mtime +30 -delete

echo "Backup completed: nodeagent-$DATE.tar.gz"
```

Schedule daily backups:
```bash
sudo crontab -e

# Add: Daily backup at 2 AM
0 2 * * * /usr/local/bin/backup-nodeagent.sh
```

## Best Practices

1. **Regular backups** - Backup `/etc/runos/` weekly
2. **Monitor proactively** - Don't wait for failures
3. **Keep logs manageable** - Configure appropriate rotation
4. **Update promptly** - Apply updates within 30 days
5. **Test changes** - Verify after updates or configuration changes
6. **Document customizations** - Keep notes on local modifications
7. **Secure credentials** - Protect private keys and backups
8. **Plan for failures** - Have recovery procedures documented
