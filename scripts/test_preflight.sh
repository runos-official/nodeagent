#!/bin/bash
#
# test_preflight.sh - Mirror of Go preflight checks for testing
#
# Run this script on the target instance to verify all preflight checks
# will pass before deploying the actual nodeagent binary.
#

set -e

RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
NC='\033[0m' # No Color

WARNINGS=()

pass() {
    echo -e "${GREEN}[PASS]${NC} $1"
}

fail() {
    echo -e "${RED}[FAIL]${NC} $1"
    exit 1
}

warn() {
    echo -e "${YELLOW}[WARN]${NC} $1"
    WARNINGS+=("$1")
}

info() {
    echo -e "[INFO] $1"
}

echo "========================================"
echo "RunOS Node Agent Preflight Check"
echo "========================================"
echo ""

# =============================================================================
# 1. System Requirements
# =============================================================================
echo "--- System Requirements ---"

# Check CPU count (need >= 2)
CPU_COUNT=$(nproc)
if [ "$CPU_COUNT" -lt 2 ]; then
    fail "Insufficient CPU cores: found $CPU_COUNT, need at least 2"
else
    pass "CPU cores: $CPU_COUNT (>= 2 required)"
fi

# Check RAM (need >= 3.5 GB)
# /proc/meminfo reports in KiB, convert to decimal GB
MEM_KB=$(grep MemTotal /proc/meminfo | awk '{print $2}')
MEM_BYTES=$((MEM_KB * 1024))
MEM_GB=$(echo "scale=1; $MEM_BYTES / 1000 / 1000 / 1000" | bc)
MEM_GB_INT=$(echo "$MEM_GB" | cut -d. -f1)
MEM_GB_DEC=$(echo "$MEM_GB" | cut -d. -f2)

# Compare 3.5 GB (using integer math: 35 vs MEM_GB*10)
MEM_CHECK=$((MEM_GB_INT * 10 + MEM_GB_DEC))
if [ "$MEM_CHECK" -lt 35 ]; then
    fail "Insufficient RAM: found ${MEM_GB} GB, need at least 3.5 GB"
else
    pass "RAM: ${MEM_GB} GB (>= 3.5 GB required)"
fi

# Check disk space (need >= 25 GB available on root)
DISK_AVAIL_KB=$(df / | tail -1 | awk '{print $4}')
DISK_AVAIL_GB=$((DISK_AVAIL_KB * 1024 / 1000 / 1000 / 1000))
if [ "$DISK_AVAIL_GB" -lt 25 ]; then
    fail "Insufficient disk space: found ${DISK_AVAIL_GB} GB available, need at least 25 GB"
else
    pass "Disk space: ${DISK_AVAIL_GB} GB available (>= 25 GB required)"
fi

# Check OS version (need Ubuntu 24.04)
if [ -f /etc/os-release ]; then
    OS_NAME=$(grep "^NAME=" /etc/os-release | cut -d= -f2 | tr -d '"')
    OS_VERSION=$(grep "^VERSION_ID=" /etc/os-release | cut -d= -f2 | tr -d '"')

    if [[ ! "${OS_NAME,,}" =~ ubuntu ]]; then
        fail "Unsupported OS: found $OS_NAME, need Ubuntu 24.04"
    fi

    if [ "$OS_VERSION" != "24.04" ]; then
        fail "Unsupported Ubuntu version: found $OS_VERSION, need 24.04"
    fi

    pass "OS: $OS_NAME $OS_VERSION"
else
    fail "Cannot read /etc/os-release"
fi

echo ""

# =============================================================================
# 2. Reboot Required Check
# =============================================================================
echo "--- Reboot Check ---"

if [ -f /var/run/reboot-required ]; then
    PKG_INFO=""
    if [ -f /var/run/reboot-required.pkgs ]; then
        PKG_INFO=$(cat /var/run/reboot-required.pkgs)
    fi
    fail "System reboot required before installation can proceed. Packages: $PKG_INFO"
else
    pass "No reboot required"
fi

echo ""

# =============================================================================
# 3. Package Manager Locks
# =============================================================================
echo "--- Package Manager Locks ---"

# Check for running apt/dpkg processes
for proc in apt-get apt dpkg; do
    PIDS=$(pgrep -x "$proc" 2>/dev/null || true)
    if [ -n "$PIDS" ]; then
        fail "Package manager process '$proc' is running (PIDs: $PIDS)"
    fi
done
pass "No apt/dpkg processes running"

# Check lock files using flock
LOCK_FILES=(
    "/var/lib/dpkg/lock"
    "/var/lib/dpkg/lock-frontend"
    "/var/lib/apt/lists/lock"
)

for lock_file in "${LOCK_FILES[@]}"; do
    if [ -f "$lock_file" ]; then
        # Try to acquire lock (non-blocking)
        if ! flock -n "$lock_file" true 2>/dev/null; then
            fail "Package manager lock file is held: $lock_file"
        fi
    fi
done
pass "No package manager locks held"

echo ""

# =============================================================================
# 4. DNS Resolution
# =============================================================================
echo "--- DNS Resolution ---"

DOMAINS=("github.com" "pkgs.k8s.io" "helm.cilium.io")

for domain in "${DOMAINS[@]}"; do
    if ! getent hosts "$domain" > /dev/null 2>&1; then
        fail "DNS resolution failed for $domain"
    fi
done
pass "DNS resolution working for all required domains"

echo ""

# =============================================================================
# 5. Network Connectivity
# =============================================================================
echo "--- Network Connectivity ---"

declare -A ENDPOINTS=(
    ["Kubernetes packages"]="https://pkgs.k8s.io"
    ["Helm Cilium repo"]="https://helm.cilium.io"
    ["GitHub"]="https://github.com"
    ["Kubernetes registry"]="https://registry.k8s.io"
    ["Docker Hub"]="https://registry-1.docker.io"
)

for name in "${!ENDPOINTS[@]}"; do
    url="${ENDPOINTS[$name]}"

    HTTP_CODE=$(curl -s -o /dev/null -w "%{http_code}" --connect-timeout 10 --max-time 15 "$url" 2>/dev/null || echo "000")

    if [ "$HTTP_CODE" = "000" ]; then
        fail "Cannot reach $name ($url): network timeout or connection refused"
    elif [ "$HTTP_CODE" -ge 500 ]; then
        fail "Cannot reach $name ($url): received HTTP $HTTP_CODE"
    else
        pass "$name ($url): HTTP $HTTP_CODE"
    fi
done

echo ""

# =============================================================================
# 6. APT Sources Check
# =============================================================================
echo "--- APT Sources ---"

info "Running apt-get update (this may take a moment)..."
APT_OUTPUT=$(apt-get update -qq 2>&1) || true
APT_EXIT=$?

if [ $APT_EXIT -ne 0 ]; then
    if echo "$APT_OUTPUT" | grep -q "does not have a Release file\|404  Not Found"; then
        fail "Broken APT sources detected: $APT_OUTPUT"
    else
        fail "apt-get update failed: $APT_OUTPUT"
    fi
else
    pass "APT sources are valid"
fi

echo ""

# =============================================================================
# 7. Kernel Modules
# =============================================================================
echo "--- Kernel Modules ---"

MODULES=("br_netfilter" "overlay" "nf_conntrack" "wireguard")

for mod in "${MODULES[@]}"; do
    if ! modprobe --dry-run "$mod" 2>/dev/null; then
        if [ "$mod" = "wireguard" ]; then
            fail "WireGuard kernel module not available. On Ubuntu 24.04, WireGuard is built into the kernel. Try: sudo modprobe wireguard"
        else
            fail "Kernel module '$mod' cannot be loaded"
        fi
    else
        pass "Kernel module '$mod' available"
    fi
done

echo ""

# =============================================================================
# 8. Conflicting Services
# =============================================================================
echo "--- Conflicting Services ---"

# Check for existing Kubernetes installation (this is a hard fail)
if [ -f /etc/kubernetes/admin.conf ]; then
    fail "Existing Kubernetes installation detected at /etc/kubernetes/. Run 'kubeadm reset -f' to clean up first"
fi
pass "No existing Kubernetes installation"

# Check for conflicting services (warnings only)
declare -A CONFLICTS=(
    ["k3s"]="K3s is installed and may conflict with kubeadm"
    ["k0s"]="K0s is installed and may conflict with kubeadm"
    ["microk8s"]="MicroK8s is installed and may conflict with kubeadm"
    ["docker"]="Docker is installed (containerd will be used instead - this is just a warning)"
)

for service in "${!CONFLICTS[@]}"; do
    if systemctl is-active --quiet "$service" 2>/dev/null; then
        warn "${CONFLICTS[$service]}"
    fi
done

echo ""

# =============================================================================
# Summary
# =============================================================================
echo "========================================"
if [ ${#WARNINGS[@]} -gt 0 ]; then
    echo -e "${YELLOW}Preflight checks passed with warnings:${NC}"
    for w in "${WARNINGS[@]}"; do
        echo -e "  - $w"
    done
else
    echo -e "${GREEN}All preflight checks passed!${NC}"
fi
echo "========================================"
echo ""
echo "System is ready for installation"
