#!/bin/bash
#
# RunOS node installer.
#
# This file is STATIC. Conductor serves it byte for byte at /install/node.sh on the API host, the
# same file for every account and every node. Nothing is filled in on the server. The install
# command passes every value as an argument:
#
#   curl -fsSL https://api.runos.com/install/node.sh | sudo bash -s -- \
#     --api https://api.runos.com --account <account id> --token <registration token> \
#     [--version <node agent version>]
#
# --version installs that exact release, a hidden release candidate included (for example
# --version 1.8.5-rc.5). The binary is verified against that release's own checksums.txt.
# Without --version the script installs the version conductor advertises for the account.
# The join command that conductor generates passes the cluster's pinned version as --version.
#
# The script runs as root, so it checks the shape of every argument BEFORE it runs any command.
# A value of the wrong shape stops the script with exit code 2 and nothing runs. The values that
# conductor returns at run time (the version, the nodeward host, the CDN base) are checked the
# same way before use.
#
# The source of truth for this file is the node agent repository (installer/node-install.sh).
# Conductor ships a copy pinned at its build, and a conductor test fails when the copy and this
# file differ.

# Character classes such as [A-Za-z] must mean ASCII only, whatever the caller's locale is.
export LC_ALL=C

# --- Arguments: parsed and checked with shell builtins only, before anything runs --------------

refuse() {
    # Never echo the refused value: it can carry terminal escape sequences.
    printf 'RunOS node installer: %s\n' "$1" >&2
    printf 'Nothing was installed. Copy the install command again from the RunOS Console or from `runos nodes join-command`.\n' >&2
    exit 2
}

API_URL=""
ACCOUNT_ID=""
SECURITY_TOKEN=""
VERSION=""

while [ "$#" -gt 0 ]; do
    case "$1" in
        --api|--account|--token|--version)
            [ "$#" -ge 2 ] || refuse "the argument $1 needs a value"
            opt="$1"
            val="$2"
            shift 2
            ;;
        --api=*|--account=*|--token=*|--version=*)
            opt="${1%%=*}"
            val="${1#*=}"
            shift
            ;;
        *)
            refuse "unknown argument (this installer takes only --api, --account, --token and --version)"
            ;;
    esac
    case "$opt" in
        --api) API_URL="$val" ;;
        --account) ACCOUNT_ID="$val" ;;
        --token) SECURITY_TOKEN="$val" ;;
        --version) VERSION="$val" ;;
    esac
done

# Shapes, from the issuers:
#   token:   nodeward mints 64 characters of [A-Za-z0-9].
#   account: conductor mints 5 characters of [a-z0-9].
#   version: a node agent release tag, with or without the leading "v".
#   api:     an https URL of a host name, with an optional port and no path.
TOKEN_RE='^[A-Za-z0-9]{64}$'
ACCOUNT_RE='^[a-z0-9]{5}$'
VERSION_RE='^[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.]+)?$'
HOST_RE='[a-z0-9]([a-z0-9-]*[a-z0-9])?(\.[a-z0-9]([a-z0-9-]*[a-z0-9])?)*(:[0-9]{1,5})?'
API_RE="^https://${HOST_RE}\$"
NODEWARD_RE="^${HOST_RE}\$"
CDN_RE="^https://${HOST_RE}(/[A-Za-z0-9._~-]+)*\$"

# The security floor: no release below it is ever installed (FCR 1650). It must equal
# MinimumVersion in uc/install/version_floor.go; a test keeps the two in step. A pre-release
# of the floor itself (1.8.4-rc.1) sorts below the floor and is refused. Any exact release or
# hidden release candidate above the floor (for example 1.8.5-rc.5) is installed exactly.
MIN_VERSION="1.8.4"

# below_floor succeeds when the release version $1 sorts below MIN_VERSION. Builtins only.
below_floor() {
    local core="${1%%-*}" pre="" IFS=. a b i
    [ "$core" != "$1" ] && pre="1"
    # Both values are checked digits and dots, so word splitting on "." is safe here.
    # shellcheck disable=SC2206
    a=( $core )
    # shellcheck disable=SC2206
    b=( $MIN_VERSION )
    for i in 0 1 2; do
        if [ $((10#${a[$i]})) -lt $((10#${b[$i]})) ]; then return 0; fi
        if [ $((10#${a[$i]})) -gt $((10#${b[$i]})) ]; then return 1; fi
    done
    [ -n "$pre" ]
}

[[ "$SECURITY_TOKEN" =~ $TOKEN_RE ]] || refuse "the --token value is missing or malformed (expected 64 letters and digits)"
[[ "$ACCOUNT_ID" =~ $ACCOUNT_RE ]] || refuse "the --account value is missing or malformed (expected 5 lowercase letters or digits)"
[[ "$API_URL" =~ $API_RE ]] || refuse "the --api value is missing or malformed (expected https://<host>)"
if [ -n "$VERSION" ]; then
    VERSION="${VERSION#v}"
    [[ "$VERSION" =~ $VERSION_RE ]] || refuse "the --version value is malformed (expected a release version such as 1.2.3)"
    below_floor "$VERSION" && refuse "the --version value is below the security floor ${MIN_VERSION}"
fi

if [ "$(id -u)" != "0" ]; then
    refuse "run the installer as root (pipe it into sudo bash)"
fi

# --- From here on every argument has a known, safe shape -------------------------------------

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

# Create log directory and file first (root-only: the log can carry command
# output that includes secrets, so it must not be world-readable).
touch /var/log/runos.log
chmod 600 /var/log/runos.log

# Function to log both to console and file
log_output() {
    tee -a /var/log/runos.log
}

# fail prints a clear, non-zero failure block naming the step that failed and
# aborts. A failed register/install can never be followed by a misleading
# "started successfully" line: the script stops here with a non-zero exit.
fail() {
    local step="$1"
    local code="$2"
    echo "" | log_output
    echo "================================================================" | log_output
    echo "FAILED: ${step} (exit ${code})" | log_output
    echo "The node was NOT fully installed." | log_output
    echo "See the cause above. Full log: /var/log/runos.log  (or run: runos logs)" | log_output
    echo "================================================================" | log_output
    exit 1
}

# fetch_to_file downloads $2 to $1, retrying transport failures.
#
# `if ! curl ... | log_output; then` would test the exit status of the
# PIPELINE, which is tee's, and tee always succeeds. So the status comes from
# ${PIPESTATUS[0]}, and a dead connection is reported as a download failure,
# never as a checksum mismatch.
fetch_to_file() {
    local dest="$1"
    local url="$2"
    local attempt rc errf
    errf="$(mktemp 2>/dev/null || echo /dev/null)"
    for attempt in 1 2 3; do
        [ "$attempt" -gt 1 ] && sleep 2
        curl -fSL --connect-timeout 15 --max-time 300 -o "$dest" "$url" 2>"$errf" | log_output
        rc=${PIPESTATUS[0]}
        [ -s "$errf" ] && log_output < "$errf"
        if [ "$rc" -eq 0 ] && [ -s "$dest" ]; then
            rm -f "$errf"
            return 0
        fi
        # curl exit 22 is an HTTP error. A 404 or 410 is a permanent answer (the release or
        # asset does not exist) and retrying only delays it. A 5xx, 429 or 403 from a busy
        # object store is transient and IS retried like a dropped connection.
        if [ "$rc" -eq 22 ] && grep -qE 'returned error: 4(04|10)' "$errf"; then
            echo "HTTP error fetching ${url} (curl exit 22, not found); not retrying." | log_output
            rm -f "$errf"
            return 22
        fi
        echo "Download attempt ${attempt} of 3 failed for ${url} (curl exit ${rc})" | log_output
    done
    rm -f "$errf"
    return 1
}

# fetch_stdout prints $1 to stdout, retrying transport failures. The log lines
# go to stderr because callers capture stdout with $(...).
fetch_stdout() {
    local url="$1"
    local attempt out rc errf
    errf="$(mktemp 2>/dev/null || echo /dev/null)"
    for attempt in 1 2 3; do
        [ "$attempt" -gt 1 ] && sleep 2
        out="$(curl -fsSL --connect-timeout 15 --max-time 30 "$url" 2>"$errf")"
        rc=$?
        if [ "$rc" -eq 0 ] && [ -n "$out" ]; then
            rm -f "$errf"
            printf '%s\n' "$out"
            return 0
        fi
        echo "Fetch attempt ${attempt} of 3 failed for ${url} (curl exit ${rc}): $(tr '\n' ' ' < "$errf")" | log_output >&2
        # Give up early only on a permanent HTTP answer; a 5xx or 429 is retried.
        [ "$rc" -eq 22 ] && grep -qE 'returned error: 4(04|10)' "$errf" && break
    done
    rm -f "$errf"
    return 1
}

# json_string prints the value of the string field $2 in the one-line JSON
# object $1, or nothing. The caller checks the shape of the result.
json_string() {
    printf '%s' "$1" | sed -n "s/.*\"$2\"[[:space:]]*:[[:space:]]*\"\\([^\"]*\\)\".*/\\1/p" | head -n1
}

# --- Settings from conductor: the nodeward host and the CDN base -------------------------------

echo "Reading the install settings from ${API_URL}..." | log_output
if ! SETTINGS="$(fetch_stdout "${API_URL}/install/node/settings")"; then
    fail "Read the install settings from ${API_URL}/install/node/settings (check network/DNS/proxy)" 1
fi
NODEWARD_BACKEND="$(json_string "$SETTINGS" nodeward)"
CDN_URL="$(json_string "$SETTINGS" cdn)"
[[ "$NODEWARD_BACKEND" =~ $NODEWARD_RE ]] || fail "Check the nodeward host in the install settings (missing or malformed)" 1
[[ "$CDN_URL" =~ $CDN_RE ]] || fail "Check the CDN base in the install settings (missing or malformed)" 1

# --- The node agent version --------------------------------------------------------------------

# The install command names the version when the cluster pins one. Otherwise
# conductor answers with the version it advertises for this account. There is
# no floating "latest": no version means no install.
if [ -z "$VERSION" ]; then
    if ! VERSION_JSON="$(fetch_stdout "${API_URL}/${ACCOUNT_ID}/node-agent-version")"; then
        fail "Read the node agent version from ${API_URL}/${ACCOUNT_ID}/node-agent-version" 1
    fi
    VERSION="$(json_string "$VERSION_JSON" version)"
    VERSION="${VERSION#v}"
    if [ -z "$VERSION" ]; then
        fail "Read the node agent version (no version is advertised for this account)" 1
    fi
    [[ "$VERSION" =~ $VERSION_RE ]] || fail "Check the advertised node agent version (malformed answer)" 1
    if below_floor "$VERSION"; then
        fail "Check the advertised node agent version (${VERSION} is below the security floor ${MIN_VERSION})" 1
    fi
fi

# The node agent ships as attested binaries on GitHub Releases. Download the
# exact tag and verify its sha256 against the release's published checksums.txt.
RELEASE_BASE="https://github.com/runos-official/nodeagent/releases/download/v${VERSION}"
BINARY_NAME="nodeagent-linux-$ARCH"
BINARY_URL="$RELEASE_BASE/$BINARY_NAME"
TMP_BIN="$(mktemp)"

echo "Downloading RunOS node agent ${VERSION} (${BINARY_NAME})..." | log_output
fetch_to_file "$TMP_BIN" "$BINARY_URL"
FETCH_RC=$?
if [ "$FETCH_RC" -eq 22 ]; then
    echo "HTTP error fetching $BINARY_URL (release v$VERSION or asset $BINARY_NAME does not exist?)." | log_output
    echo "The server answered, so this is not an egress problem. Check that the release and its assets are published." | log_output
    rm -f "$TMP_BIN"
    exit 1
elif [ "$FETCH_RC" -ne 0 ]; then
    echo "Failed to download the binary from $BINARY_URL after 3 attempts." | log_output
    echo "This is a download failure, not a bad artifact. Check egress to github.com and objects.githubusercontent.com." | log_output
    rm -f "$TMP_BIN"
    exit 1
fi

# Two distinct failures: checksums.txt unreachable, and checksums.txt fetched
# but naming no entry for this asset.
if ! CHECKSUMS="$(fetch_stdout "$RELEASE_BASE/checksums.txt")"; then
    echo "Could not fetch $RELEASE_BASE/checksums.txt after 3 attempts; aborting. See the curl errors above." | log_output
    rm -f "$TMP_BIN"
    exit 1
fi
EXPECTED="$(printf '%s\n' "$CHECKSUMS" | awk -v f="$BINARY_NAME" '$2 == f || $2 == "*"f {print $1}')"
if [ -z "$EXPECTED" ]; then
    echo "checksums.txt for v$VERSION has no entry for $BINARY_NAME; aborting. The release is incomplete or the asset name changed." | log_output
    rm -f "$TMP_BIN"
    exit 1
fi

# Prove the file actually arrived BEFORE hashing it. The sha256 of an empty
# file would be reported as a checksum mismatch, which is a security-shaped
# claim about a problem that was purely network.
if [ ! -s "$TMP_BIN" ]; then
    echo "Downloaded 0 bytes for $BINARY_NAME; aborting. This is a download failure, not a checksum failure." | log_output
    rm -f "$TMP_BIN"
    exit 1
fi

ACTUAL="$(sha256sum "$TMP_BIN" | awk '{print $1}')"
if [ "$ACTUAL" != "$EXPECTED" ]; then
    echo "Checksum mismatch for $BINARY_NAME (expected $EXPECTED, got $ACTUAL); aborting." | log_output
    echo "The file downloaded completely but does not match the published checksum. Do NOT install it." | log_output
    rm -f "$TMP_BIN"
    exit 1
fi

# Verified: install the binary.
install -m 0755 "$TMP_BIN" /usr/local/bin/runos
rm -f "$TMP_BIN"
echo "Verified and installed RunOS node agent ${VERSION}." | log_output

# The node agent reaches conductor at client.server.conductor. It is written
# here explicitly, so the agent never derives the conductor host from the
# installer host. The installer host is the same API host.
echo "Configuring the RunOS node agent..." | log_output
/usr/local/bin/runos set-config client.server.installer "$API_URL" 2>&1 | log_output
/usr/local/bin/runos set-config client.server.conductor "$API_URL" 2>&1 | log_output
CONDUCTOR_SET=${PIPESTATUS[0]}
if [ "$CONDUCTOR_SET" -ne 0 ]; then
    fail "Write the conductor host into the node agent config" "$CONDUCTOR_SET"
fi

# Download and install the public CA certificate. -fSL makes an HTTP error
# (proxy / 404 / captive portal) fail instead of writing an error page as the
# "cert", and the result is checked to be a PEM before it is trusted.
echo "Downloading RunOS public CA certificate..." | log_output
mkdir -p /etc/runos
# Fetched into a temp file and installed only after the PEM check, so a re-run
# on a box that already has a CA never leaves a truncated one behind.
TMP_CA="$(mktemp)"
if ! fetch_to_file "$TMP_CA" "$CDN_URL/artifacts/l1sec-ca.runos.public.pem"; then
    rm -f "$TMP_CA"
    fail "Download the RunOS public CA certificate from $CDN_URL (check network/DNS/proxy)" 1
fi
if ! grep -q "BEGIN CERTIFICATE" "$TMP_CA" 2>/dev/null || ! grep -q "END CERTIFICATE" "$TMP_CA" 2>/dev/null; then
    rm -f "$TMP_CA"
    fail "Validate the RunOS public CA certificate (got a non-PEM or truncated response: proxy or error page?)" 1
fi
install -m 644 "$TMP_CA" /etc/runos/l1sec-ca.runos.public.pem
rm -f "$TMP_CA"

# Run preflight checks
echo "Running preflight checks..." | log_output
/usr/local/bin/runos preflight -s "$NODEWARD_BACKEND" --cdn "$CDN_URL" 2>&1 | log_output
PREFLIGHT_EXIT_CODE=${PIPESTATUS[0]}
if [ $PREFLIGHT_EXIT_CODE -ne 0 ]; then
    echo "Preflight checks failed. Please address the issues above and try again." | log_output
    exit 1
fi

echo "Registering RunOS node..." | log_output
# The control-plane vs worker role is carried by the registration token
# (resolved server-side), NOT a register flag.
/usr/local/bin/runos register -a "$ACCOUNT_ID" -t "$SECURITY_TOKEN" -s "$NODEWARD_BACKEND" 2>&1 | log_output
REGISTER_EXIT_CODE=${PIPESTATUS[0]}
if [ $REGISTER_EXIT_CODE -ne 0 ]; then
    fail "Register the node with the control plane" $REGISTER_EXIT_CODE
fi

# Create a systemd service file for the RunOS Node Agent.
# DUPLICATION: conductor's node update path
# (conductor/src/templates/scripts/023-update-runos-node-agent.sh.hbs) migrates
# this unit on an existing node. If you change the unit here, change it there too.
SERVICE_FILE="/etc/systemd/system/runos.service"
cat << EOF > $SERVICE_FILE
[Unit]
Description=RunOS Node Agent
After=network-online.target wg-quick@wg0.service
Wants=network-online.target wg-quick@wg0.service
# Disable start-rate limiting: this unit is meant to always run and can be
# explicitly restarted (operator / self-restart / wake self-heal). Without this,
# a burst of restarts within 10s trips DefaultStartLimitBurst and systemd leaves
# the unit failed ("start request repeated too quickly"), needing a manual
# reset-failed on the box.
StartLimitIntervalSec=0

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

# Create logrotate configuration for RunOS logs (root-only; see the log chmod
# above).
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
systemctl daemon-reload 2>&1 | log_output
systemctl enable runos.service 2>&1 | log_output
systemctl start runos.service 2>&1 | log_output
START_EXIT_CODE=${PIPESTATUS[0]}
if [ $START_EXIT_CODE -ne 0 ]; then
    fail "Start the RunOS node agent service (systemctl start runos)" $START_EXIT_CODE
fi

echo "Installing RunOS components..." | log_output
/usr/local/bin/runos install 2>&1 | log_output
INSTALL_EXIT_CODE=${PIPESTATUS[0]}
if [ $INSTALL_EXIT_CODE -ne 0 ]; then
    fail "Install RunOS components (Kubernetes / container runtime / networking)" $INSTALL_EXIT_CODE
fi

echo "RunOS Node Agent installed and started successfully." | log_output
