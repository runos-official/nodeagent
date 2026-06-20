# Logs & Monitoring

The RunOS Node Agent provides comprehensive logging to help you monitor operations and troubleshoot issues.

## Log File Locations

### Main Agent Log

**Location**: `/var/log/runos.log`

This is the primary log file containing all agent operations, including:
- Connection status to control plane
- Commands received and executed
- Error messages and warnings
- VPN peer synchronization
- Node health updates

**Format**: JSON structured logs (when running as daemon)

### System Service Logs

When running as a systemd service, logs are also available through journald:

```bash
# View recent logs
sudo journalctl -u runos

# Follow logs in real-time
sudo journalctl -u runos -f

# View logs from today
sudo journalctl -u runos --since today

# View last 100 lines
sudo journalctl -u runos -n 100
```

## Log Rotation

The agent uses logrotate to manage log file size and prevent disk space issues.

### Logrotate Configuration

**Location**: `/etc/logrotate.d/runos`

**Default configuration**:
```
/var/log/runos.log {
    daily
    rotate 7
    compress
    delaycompress
    missingok
    notifempty
    create 0640 root root
    sharedscripts
    postrotate
        systemctl reload runos > /dev/null 2>&1 || true
    endscript
}
```

**Settings explained**:
- `daily` - Rotate logs once per day
- `rotate 7` - Keep 7 days of archived logs
- `compress` - Compress archived logs with gzip
- `delaycompress` - Compress previous day's log (not today's)
- `missingok` - Don't error if log file is missing
- `notifempty` - Don't rotate if log is empty
- `create 0640 root root` - Create new log with specific permissions

### Customizing Log Rotation

Edit `/etc/logrotate.d/runos` to customize rotation:

```bash
sudo nano /etc/logrotate.d/runos
```

**Common customizations**:

Keep 30 days of logs:
```
rotate 30
```

Rotate when log reaches 100MB:
```
size 100M
```

Rotate weekly instead of daily:
```
weekly
```

### Manual Log Rotation

Force log rotation manually:

```bash
sudo logrotate -f /etc/logrotate.d/runos
```

## Understanding Log Output

### Log Levels

The agent uses these log levels (set via `RUNOS_LOG_LEVEL` environment variable):

- **DEBUG** - Detailed diagnostic information
- **INFO** - Normal operational messages
- **WARN** - Warning messages that don't prevent operation
- **ERROR** - Error conditions that may affect functionality

### Common Log Messages

**Successful connection:**
```json
{"level":"info","msg":"Connected to control plane","timestamp":"2025-01-15T10:30:45Z"}
```

**Heartbeat sent:**
```json
{"level":"info","msg":"Heartbeat sent","timestamp":"2025-01-15T10:30:50Z"}
```

**VPN peer sync:**
```json
{"level":"info","msg":"VPN peers synchronized","peers":12,"timestamp":"2025-01-15T10:31:00Z"}
```

**Command execution:**
```json
{"level":"info","msg":"Executing instruction","type":"GET_NODE_STATUS","timestamp":"2025-01-15T10:31:15Z"}
```

**Connection error:**
```json
{"level":"error","msg":"Failed to connect to control plane","error":"connection timeout","timestamp":"2025-01-15T10:32:00Z"}
```

## Viewing Logs

### Real-time Monitoring

Watch logs as they're written:

```bash
# Using tail
sudo tail -f /var/log/runos.log

# Using journalctl
sudo journalctl -u runos -f

# Filter by log level (journalctl)
sudo journalctl -u runos -f -p err  # errors only
```

### Searching Logs

Find specific log entries:

```bash
# Search for errors
sudo grep '"level":"error"' /var/log/runos.log

# Search for connection issues
sudo grep -i "connect" /var/log/runos.log

# Search with journalctl
sudo journalctl -u runos | grep -i "error"

# Search in compressed archives
sudo zgrep "error" /var/log/runos.log.*.gz
```

### Filtering by Time

View logs from specific time periods:

```bash
# Logs from last hour
sudo journalctl -u runos --since "1 hour ago"

# Logs from specific date
sudo journalctl -u runos --since "2025-01-15" --until "2025-01-16"

# Logs from last 24 hours
sudo journalctl -u runos --since "24 hours ago"
```

## Analyzing Logs

### JSON Log Parsing

Parse JSON logs for easier reading:

```bash
# Install jq (if not already installed)
sudo apt-get install jq

# Pretty-print logs
sudo tail -100 /var/log/runos.log | jq '.'

# Filter by level
sudo cat /var/log/runos.log | jq 'select(.level=="error")'

# Extract specific fields
sudo cat /var/log/runos.log | jq '{time: .timestamp, message: .msg, level: .level}'
```

### Common Analysis Tasks

**Count errors in last 1000 lines:**
```bash
sudo tail -1000 /var/log/runos.log | grep '"level":"error"' | wc -l
```

**Find unique error messages:**
```bash
sudo grep '"level":"error"' /var/log/runos.log | jq -r '.msg' | sort | uniq -c
```

**Check connection attempts:**
```bash
sudo grep -i "connect" /var/log/runos.log | tail -20
```

## Log Retention

### Disk Space Management

Check log disk usage:

```bash
# Check runos log directory size
sudo du -sh /var/log/runos.log*

# List log files by size
sudo ls -lh /var/log/runos.log*

# Check overall /var/log usage
df -h /var/log
```

### Manual Cleanup

If logs are consuming too much space:

```bash
# Remove old compressed logs (older than 30 days)
sudo find /var/log -name "runos.log.*.gz" -mtime +30 -delete

# Truncate current log (use with caution)
sudo truncate -s 0 /var/log/runos.log
```

## Monitoring Best Practices

1. **Regular monitoring** - Check logs daily for errors or warnings
2. **Set up alerts** - Use log monitoring tools to alert on error patterns
3. **Baseline behavior** - Understand normal log patterns for your environment
4. **Correlate events** - Cross-reference agent logs with Kubernetes and system logs
5. **Archive important logs** - Save logs during incidents for later analysis

## Troubleshooting with Logs

### Agent Won't Start

```bash
# Check systemd service status
sudo systemctl status runos

# View recent errors
sudo journalctl -u runos -n 50 -p err

# Check for configuration issues
sudo grep -i "config" /var/log/runos.log | tail -20
```

### Connection Issues

```bash
# Look for connection errors
sudo grep -i "connection\|connect\|dial" /var/log/runos.log | tail -30

# Check TLS/certificate errors
sudo grep -i "tls\|certificate\|handshake" /var/log/runos.log | tail -20
```

### Performance Issues

```bash
# Enable debug logging temporarily
sudo systemctl edit runos
# Add: Environment="RUNOS_LOG_LEVEL=debug"

# Restart agent
sudo systemctl restart runos

# Monitor detailed logs
sudo journalctl -u runos -f
```

## Exporting Logs for Support

When contacting RunOS support, provide relevant logs:

```bash
# Export last 1000 lines
sudo tail -1000 /var/log/runos.log > ~/nodeagent-logs.txt

# Export logs from specific timeframe
sudo journalctl -u runos --since "1 hour ago" > ~/nodeagent-logs.txt

# Include system information
uname -a >> ~/nodeagent-logs.txt
runos version >> ~/nodeagent-logs.txt

# Compress for sending
gzip ~/nodeagent-logs.txt
```

**Note**: Review logs before sharing to ensure no sensitive information is included.

## Log Security

- Logs may contain IP addresses and node identifiers
- Logs do not contain credentials or secrets
- Restrict log file access to root and administrators
- Use secure channels when transmitting logs

**Verify log permissions**:
```bash
sudo ls -l /var/log/runos.log
# Should show: -rw-r----- root root
```
