#!/bin/bash

# Dynamic variables
SECURITY_TOKEN="{{.SecurityToken}}"
ACCOUNT_ID="{{.AccountID}}"
CP="{{.Cp}}"
CDN_URL="{{.CdnUrl}}"
NODEWARD_BACKEND="{{.NodewardBackend}}"
TEMPLATES_URL="{{.TemplatesUrl}}"
# Exact node-agent version to install, supplied by the control plane (the
# advertised version or a per-cluster pin). Never a floating "latest".
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

# Create log directory and file first
touch /var/log/runos.log
chmod 600 /var/log/runos.log

# Function to log both to console and file
log_output() {
    tee -a /var/log/runos.log
}

# fail prints a structured, actionable failure block and exits non-zero. Every
# install step that can fail routes through this so the operator always gets a
# clear WHAT/WHY/REMEDY instead of a false "started successfully" banner.
#   fail "<step>" "<exit code>" "<remedy>"
fail() {
    local step="$1" code="$2" remedy="$3"
    {
        echo ""
        echo "FAILED: ${step} failed (exit ${code})."
        echo "  Try:   ${remedy}"
        echo "  Logs:  runos logs   (full log: /var/log/runos.log)"
    } | log_output
    exit 1
}

if [ -z "$VERSION" ]; then
    echo "No target node-agent version was provided; aborting install." | log_output
    exit 1
fi

# The node agent ships as attested binaries on GitHub Releases. Download the
# exact tag and verify its sha256 against the release's published checksums.txt
# before installing. Fail closed on any mismatch.
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
    echo "Checksum verification failed for $BINARY_NAME; aborting install." | log_output
    rm -f "$TMP_BIN"
    exit 1
fi
install -m 0755 "$TMP_BIN" /usr/local/bin/runos
rm -f "$TMP_BIN"
echo "Verified and installed RunOS node agent ${VERSION}." | log_output

# Set the installer configuration
echo "Configuring RunOS installer..." | log_output
/usr/local/bin/runos set-config client.server.installer "$TEMPLATES_URL" 2>&1 | log_output
RC=${PIPESTATUS[0]}
if [ "$RC" -ne 0 ]; then
    fail "Configure installer" "$RC" \
        "ensure /etc/runos is writable (run as root) and re-run the installer"
fi

# Download and install the public CA certificate.
# Use curl -fSL so an HTTP error (404, proxy/captive-portal error page) fails
# closed instead of writing a garbage "cert" that later breaks the TLS handshake
# with an opaque error. Then verify the bytes are actually a PEM certificate.
echo "Downloading RunOS public CA certificate..." | log_output
mkdir -p /etc/runos
CA_PATH="/etc/runos/l1sec-ca.runos.public.pem"
curl -fSL -o "$CA_PATH" "$CDN_URL/artifacts/l1sec-ca.runos.public.pem" 2>&1 | log_output
RC=${PIPESTATUS[0]}
if [ "$RC" -ne 0 ]; then
    fail "Download CA certificate" "$RC" \
        "check egress to $CDN_URL on 443 (HTTP error, proxy, or DNS); verify HTTP(S)_PROXY settings"
fi
if ! grep -q "BEGIN CERTIFICATE" "$CA_PATH"; then
    fail "Verify CA certificate" "1" \
        "the downloaded CA is not a valid certificate (a proxy/captive portal may have returned an error page); check $CDN_URL"
fi
chmod 644 "$CA_PATH"

# Run preflight checks. Pass --server so preflight can probe the actual Nodeward
# host (DNS/firewall reachability on 9191/9192) before we attempt registration.
echo "Running preflight checks..." | log_output
/usr/local/bin/runos preflight -s "$NODEWARD_BACKEND" 2>&1 | log_output
RC=${PIPESTATUS[0]}
if [ "$RC" -ne 0 ]; then
    fail "Preflight checks" "$RC" \
        "address the issues reported above; re-run 'sudo runos preflight -s $NODEWARD_BACKEND' to re-check"
fi

echo "Registering RunOS node..." | log_output
/usr/local/bin/runos register -a "$ACCOUNT_ID" -t "$SECURITY_TOKEN" -c "$CP" -s "$NODEWARD_BACKEND" 2>&1 | log_output
RC=${PIPESTATUS[0]}
if [ "$RC" -ne 0 ]; then
    fail "Register node" "$RC" \
        "re-copy the registration command from the RunOS console (tokens are short-lived) and re-run; check connectivity to $NODEWARD_BACKEND"
fi

# Create a systemd service file for the RunOS Node Agent agent
SERVICE_FILE="/etc/systemd/system/runos.service"
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

# Create logrotate configuration for RunOS logs
LOGROTATE_FILE="/etc/logrotate.d/runos"
cat << EOF > $LOGROTATE_FILE
/var/log/runos.log {
    daily
    rotate 7
    compress
    delaycompress
    missingok
    notifempty
    create 600 root root
    postrotate
        systemctl reload runos.service > /dev/null 2>&1 || true
    endscript
}
EOF

# Reload systemd, enable and start the RunOS Node Agent service
echo "Setting up RunOS service..." | log_output
sudo systemctl daemon-reload 2>&1 | log_output
sudo systemctl enable runos.service 2>&1 | log_output
sudo systemctl start runos.service 2>&1 | log_output
RC=${PIPESTATUS[0]}
if [ "$RC" -ne 0 ]; then
    fail "Start RunOS service" "$RC" \
        "inspect the unit with 'systemctl status runos.service' and 'journalctl -u runos'"
fi

echo "Installing RunOS components..." | log_output
/usr/local/bin/runos install 2>&1 | log_output
RC=${PIPESTATUS[0]}
if [ "$RC" -ne 0 ]; then
    fail "Install Kubernetes/WireGuard" "$RC" \
        "see /var/log/runos.log and 'journalctl -u runos'; run 'sudo runos preflight -s $NODEWARD_BACKEND' to diagnose, then re-run 'sudo runos install'"
fi

# Only reached when every step above succeeded.
echo "RunOS Node Agent installed and started successfully." | log_output