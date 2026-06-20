#!/bin/bash

# Dynamic variables
CDN_URL="{{.CdnUrl}}"
# Exact node-agent version to update to (the advertised version or a per-cluster
# pin, supplied by the control plane). Never a floating "latest".
VERSION="{{.Version}}"

# Detect the architecture
ARCH=$(uname -m)
case $ARCH in
    x86_64)
        ARCH="amd64"
        ;;
    aarch64)
        ARCH="arm64"
        ;;
    *)
        echo "Unsupported architecture: $ARCH"
        echo "Only Linux AMD64 and ARM64 are supported"
        exit 1
        ;;
esac

# Function to log both to console and file
log_output() {
    tee -a /var/log/runos.log
}

if [ -z "$VERSION" ]; then
    echo "No target node-agent version was provided; aborting update." | log_output
    exit 1
fi

# The node agent ships as attested binaries on GitHub Releases. Download the
# exact tag and verify its sha256 against the release's published checksums.txt
# before swapping the running binary. Fail closed on any mismatch.
RELEASE_BASE="https://github.com/runos-official/nodeagent/releases/download/v${VERSION}"
BINARY_NAME="nodeagent-linux-$ARCH"
TMP_BIN="$(mktemp)"

echo "Downloading RunOS node agent ${VERSION} (${BINARY_NAME})..." | log_output
if ! curl -fSL -o "$TMP_BIN" "$RELEASE_BASE/$BINARY_NAME" 2>&1 | log_output; then
    echo "Failed to download the binary from $RELEASE_BASE/$BINARY_NAME" | log_output
    rm -f "$TMP_BIN"
    exit 1
fi
EXPECTED="$(curl -fsSL "$RELEASE_BASE/checksums.txt" 2>/dev/null | awk -v f="$BINARY_NAME" '$2 == f || $2 == "*"f {print $1}')"
if [ -z "$EXPECTED" ] || [ "$(sha256sum "$TMP_BIN" | awk '{print $1}')" != "$EXPECTED" ]; then
    echo "Checksum verification failed for $BINARY_NAME; aborting update." | log_output
    rm -f "$TMP_BIN"
    exit 1
fi
install -m 0755 "$TMP_BIN" /usr/local/bin/runos.new
rm -f "$TMP_BIN"

# Download and install the public CA certificate
echo "Downloading RunOS public CA certificate..." | log_output
mkdir -p /etc/runos
curl -o /etc/runos/l1sec-ca.runos.public.pem $CDN_URL/artifacts/l1sec-ca.runos.public.pem 2>&1 | log_output
if [ $? -ne 0 ]; then
    echo "Failed to download the public CA certificate" | log_output
    exit 1
fi
chmod 644 /etc/runos/l1sec-ca.runos.public.pem

# Print out the two versions
echo "-----------------" | log_output
echo "## Previous version:" | log_output
/usr/local/bin/runos version 2>&1 | log_output

echo "-----------------" | log_output
echo "## New version:" | log_output
/usr/local/bin/runos.new version 2>&1 | log_output
echo "-----------------" | log_output

echo "Stopping the current RunOS Node Agent service..." | log_output
sudo systemctl stop runos.service 2>&1 | log_output

# Replace the old binary with the new one
cp /usr/local/bin/runos /usr/local/bin/runos-$(date +%s)
mv /usr/local/bin/runos.new /usr/local/bin/runos

# Ensure systemd service file has ExecReload directive
echo "Ensuring systemd service configuration is up to date..." | log_output
SERVICE_FILE="/etc/systemd/system/runos.service"
if [ -f "$SERVICE_FILE" ]; then
    # Check if ExecReload is present
    if ! grep -q "ExecReload=" "$SERVICE_FILE"; then
        echo "Updating systemd service to support reload..." | log_output
        # Recreate the service file with ExecReload
        cat << EOF > $SERVICE_FILE
[Unit]
Description=RunOS Node Agent
After=network.target wg-quick@wg0.service
Wants=wg-quick@wg0.service

[Service]
ExecStart=/usr/local/bin/runos agent
ExecReload=/bin/kill -HUP \$MAINPID
Restart=always
User=root
RestartSec=10
StandardOutput=append:/var/log/runos.log
StandardError=append:/var/log/runos.log

[Install]
WantedBy=multi-user.target
EOF
        sudo systemctl daemon-reload 2>&1 | log_output
    else
        echo "Systemd service already has reload support." | log_output
    fi
else
    echo "Warning: systemd service file not found." | log_output
fi

# Ensure logrotate configuration is present
echo "Ensuring logrotate configuration is present..." | log_output
LOGROTATE_FILE="/etc/logrotate.d/runos"
if [ ! -f "$LOGROTATE_FILE" ]; then
    echo "Creating logrotate configuration..." | log_output
    cat << EOF > $LOGROTATE_FILE
/var/log/runos.log {
    daily
    rotate 7
    compress
    delaycompress
    missingok
    notifempty
    create 644 root root
    postrotate
        systemctl reload runos.service > /dev/null 2>&1 || true
    endscript
}
EOF
else
    echo "Logrotate configuration already exists." | log_output
fi

echo "Starting the updated RunOS Node Agent service..." | log_output
sudo systemctl start runos.service 2>&1 | log_output

echo "RunOS Node Agent update completed successfully." | log_output