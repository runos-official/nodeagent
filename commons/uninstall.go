package commons

import (
	"fmt"
)

// Uninstall removes Kubernetes, Containerd and WireGuard and resets networking.
// When full is true it also clears RunOS configuration and certificates.
func Uninstall(full bool) {
	fmt.Println("Starting uninstallation process...")

	// Stop and reset Kubernetes
	fmt.Println(ExecuteCommandGetResponse("kubeadm reset -f || true"))

	// Remove cluster configurations
	fmt.Println(ExecuteCommandGetResponse("rm -rf /etc/kubernetes || true"))
	fmt.Println(ExecuteCommandGetResponse("rm -rf /var/lib/kubelet || true"))
	fmt.Println(ExecuteCommandGetResponse("rm -rf /var/lib/etcd || true"))
	fmt.Println(ExecuteCommandGetResponse("rm -rf ~/.kube || true"))

	// Remove CNI configurations
	fmt.Println(ExecuteCommandGetResponse("rm -rf /etc/cni || true"))
	fmt.Println(ExecuteCommandGetResponse("rm -rf /opt/cni || true"))
	fmt.Println(ExecuteCommandGetResponse("rm -rf /var/lib/cni || true"))

	// Stop and remove Wireguard
	fmt.Println(ExecuteCommandGetResponse("systemctl stop wg-quick@wg0 || true"))
	fmt.Println(ExecuteCommandGetResponse("systemctl disable wg-quick@wg0 || true"))
	fmt.Println(ExecuteCommandGetResponse("ip link delete wg0 || true"))
	fmt.Println(ExecuteCommandGetResponse("rm -rf /etc/wireguard || true"))

	// Clean up DNS configuration
	fmt.Println(ExecuteCommandGetResponse("systemctl stop dnsmasq || true"))
	fmt.Println(ExecuteCommandGetResponse("systemctl disable dnsmasq || true"))

	// Remove dnsmasq systemd override
	fmt.Println(ExecuteCommandGetResponse("rm -rf /etc/systemd/system/dnsmasq.service.d || true"))

	// Remove dnsmasq configuration
	fmt.Println(ExecuteCommandGetResponse("rm -f /etc/dnsmasq.d/runos.conf || true"))

	// Remove netplan DNS override
	fmt.Println(ExecuteCommandGetResponse("rm -f /etc/netplan/99-runos-disable-dhcp-dns.yaml || true"))
	fmt.Println(ExecuteCommandGetResponse("netplan apply || true"))

	// Restore original systemd-resolved configuration if backup exists
	fmt.Println(ExecuteCommandGetResponse("if [ -f /etc/systemd/resolved.conf.runos-bak ]; then mv /etc/systemd/resolved.conf.runos-bak /etc/systemd/resolved.conf; fi || true"))

	// Restart systemd-resolved to apply restored configuration
	fmt.Println(ExecuteCommandGetResponse("systemctl restart systemd-resolved || true"))

	// Restore default resolv.conf symlink
	fmt.Println(ExecuteCommandGetResponse("ln -sf /run/systemd/resolve/stub-resolv.conf /etc/resolv.conf || true"))

	// Flush DNS caches
	fmt.Println(ExecuteCommandGetResponse("resolvectl flush-caches || systemd-resolve --flush-caches || true"))

	// Uninstall dnsmasq
	fmt.Println(ExecuteCommandGetResponse("apt-get remove -y --purge dnsmasq || true"))

	// Reset iptables
	fmt.Println(ExecuteCommandGetResponse("iptables -F || true"))
	fmt.Println(ExecuteCommandGetResponse("iptables -t nat -F || true"))
	fmt.Println(ExecuteCommandGetResponse("iptables -t mangle -F || true"))
	fmt.Println(ExecuteCommandGetResponse("iptables -X || true"))
	fmt.Println(ExecuteCommandGetResponse("iptables -P FORWARD ACCEPT || true"))

	// Reset IP forwarding and network configurations
	fmt.Println(ExecuteCommandGetResponse("sysctl net.bridge.bridge-nf-call-iptables=0 || true"))
	fmt.Println(ExecuteCommandGetResponse("sysctl net.bridge.bridge-nf-call-ip6tables=0 || true"))
	fmt.Println(ExecuteCommandGetResponse("sysctl net.ipv4.ip_forward=0 || true"))

	// Remove sysctl configurations
	fmt.Println(ExecuteCommandGetResponse("rm -f /etc/sysctl.d/99-ipforward.conf || true"))

	// Clean /etc/modules and /etc/modules-load.d
	fmt.Println(ExecuteCommandGetResponse("rm -f /etc/modules-load.d/kubernetes.conf || true"))

	// Remove 'br_netfilter' from /etc/modules
	sedCmd := "sed -i '/br_netfilter/d' /etc/modules || true"
	fmt.Println(ExecuteCommandGetResponse(sedCmd))

	// Remove RunOS Managed entries from /etc/hosts
	hostsCmd := "sed -i '/#RunOS Managed/d' /etc/hosts || true"
	fmt.Println(ExecuteCommandGetResponse(hostsCmd))

	// Uninstall Kubernetes packages
	fmt.Println(ExecuteCommandGetResponse("apt-mark unhold kubelet kubeadm kubectl || true"))
	fmt.Println(ExecuteCommandGetResponse("apt-get remove -y --purge kubeadm kubectl kubelet kubernetes-cni || true"))

	// Remove Kubernetes repo
	fmt.Println(ExecuteCommandGetResponse("rm -f /etc/apt/sources.list.d/kubernetes.list || true"))
	fmt.Println(ExecuteCommandGetResponse("rm -f /etc/apt/keyrings/kubernetes-apt-keyring.gpg || true"))

	// Uninstall containerd
	fmt.Println(ExecuteCommandGetResponse("apt-get remove -y --purge containerd || true"))

	// Uninstall wireguard
	fmt.Println(ExecuteCommandGetResponse("apt-get remove -y --purge wireguard || true"))

	// Clean apt cache
	fmt.Println(ExecuteCommandGetResponse("apt-get autoremove -y || true"))
	fmt.Println(ExecuteCommandGetResponse("apt-get clean || true"))

	// Stop and remove HAProxy
	fmt.Println(ExecuteCommandGetResponse("systemctl stop haproxy || true"))
	fmt.Println(ExecuteCommandGetResponse("systemctl disable haproxy || true"))
	fmt.Println(ExecuteCommandGetResponse("rm -rf /var/run/haproxy || true"))
	fmt.Println(ExecuteCommandGetResponse("apt-get remove -y --purge haproxy || true"))

	// Remove the RunOS Node Agent
	fmt.Println(ExecuteCommandGetResponse("rm -Rf /root/.runos"))
	fmt.Println(ExecuteCommandGetResponse("systemctl disable runos || true"))
	fmt.Println(ExecuteCommandGetResponse("rm -f /etc/systemd/system/runos.service || true"))
	if full {
		fmt.Println(ExecuteCommandGetResponse("systemctl daemon-reload || true"))
		fmt.Println(ExecuteCommandGetResponse("systemctl stop runos || true"))
	}

	fmt.Println("Uninstallation complete. You may need to reboot the system for all changes to take effect.")
}
