package k8s

import (
	"bytes"
	"context"
	"fmt"
	"github.com/runos-official/nodeagent/roslog"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
	"os"
	"os/exec"
	"strings"
)

// IsNodeReady reports whether the local node is Ready in the Kubernetes API.
func IsNodeReady() bool {
	// Check if /etc/kubernetes/admin.conf exists, else use /etc/kubernetes/kubelet.conf
	var k8sCredsFile string
	if _, err := os.Stat("/etc/kubernetes/admin.conf"); err == nil {
		k8sCredsFile = "/etc/kubernetes/admin.conf"
	} else {
		k8sCredsFile = "/etc/kubernetes/kubelet.conf"
	}
	config, err := clientcmd.BuildConfigFromFlags("", k8sCredsFile)
	if err != nil {
		roslog.E("Error building kubeconfig", err)
		return false
	}

	// create the clientset
	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		roslog.E("Error setting kubeconfig", err)
		return false
	}

	// Define the context
	ctx := context.Background()

	// System hostname
	hostname, err := os.Hostname()
	if err != nil {
		roslog.E("Error getting hostname", err)
		return false
	}

	// Get the node
	node, err := clientset.CoreV1().Nodes().Get(ctx, hostname, metav1.GetOptions{})
	if err != nil {
		roslog.E("Error Getting node info", err)
		return false
	}

	var ready bool
	for _, condition := range node.Status.Conditions {
		if condition.Type == corev1.NodeReady && condition.Status == corev1.ConditionTrue {
			ready = true
			break
		}
	}

	return ready
}

// ApplyYamlResourceFile writes the manifest to a unique temp file and applies it
// with kubectl. A unique temp path (os.CreateTemp) is required because APPLY_CR
// and APPLY_OPERATOR run concurrently across the worker pool; a constant path
// would let concurrent applies corrupt each other's manifest. The temp file is
// removed on return. Returns an error (rather than panicking) so callers can
// surface the failure through the normal handler error path.
func ApplyYamlResourceFile(file []byte) error {
	kubeConfigPath := "/etc/kubernetes/admin.conf"

	// Write the YAML content to a unique temp file so concurrent applies
	// (5 workers) cannot clobber each other.
	tmpFile, err := os.CreateTemp("", "runos_apply_*.yaml")
	if err != nil {
		return fmt.Errorf("failed to create temp YAML file: %v", err)
	}
	outputPath := tmpFile.Name()
	defer os.Remove(outputPath)

	if err := os.WriteFile(outputPath, file, 0644); err != nil {
		tmpFile.Close()
		return fmt.Errorf("failed to write YAML file: %v", err)
	}
	if err := tmpFile.Close(); err != nil {
		return fmt.Errorf("failed to close YAML file: %v", err)
	}
	fmt.Printf("YAML written to file: %s\n", outputPath)

	cmd := exec.Command("/usr/bin/kubectl", "apply", "-f", outputPath)
	cmd.Env = append(os.Environ(), fmt.Sprintf("KUBECONFIG=%s", kubeConfigPath))
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	// Run the kubectl command
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("failed to apply YAML file with kubectl: %v", err)
	}
	fmt.Println("Successfully applied the YAML file using kubectl.")
	return nil
}

// DeleteCR deletes a namespaced custom resource via the dynamic client. Returns
// an error (rather than panicking) so callers can surface the failure through
// the normal handler error path.
func DeleteCR(group string, version string, resource string, namespace string, resourceName string) error {
	// Path to the kubeconfig file
	kubeconfig := "/etc/kubernetes/admin.conf"
	config, err := clientcmd.BuildConfigFromFlags("", kubeconfig)
	if err != nil {
		roslog.E("Failed to load kubeconfig", err)
		return fmt.Errorf("failed to load kubeconfig: %w", err)
	}

	// Create a dynamic client
	dynamicClient, err := dynamic.NewForConfig(config)
	if err != nil {
		roslog.E("Failed to create dynamic client", err)
		return fmt.Errorf("failed to create dynamic client: %w", err)
	}

	// Define the GroupVersionResource for the Custom Resource
	gvr := schema.GroupVersionResource{
		Group:    group,    // Replace with your CRD's API group
		Version:  version,  // Replace with your CRD's version
		Resource: resource, // Plural name from the CRD
	}

	// Delete the resource
	err = dynamicClient.Resource(gvr).Namespace(namespace).Delete(context.TODO(), resourceName, metav1.DeleteOptions{})
	if err != nil {
		roslog.E("Failed to delete resource", err)
		return fmt.Errorf("failed to delete resource: %w", err)
	}

	roslog.I("Resource deleted successfully")
	return nil
}

// RunKubectlCommand runs kubectl with the given arguments against the local
// admin kubeconfig and returns its combined output.
func RunKubectlCommand(kubectlArgs []string) (string, error) {
	kubeConfigPath := "/etc/kubernetes/admin.conf"

	cmd := exec.Command("/usr/bin/kubectl", kubectlArgs...)
	cmd.Env = append(os.Environ(), fmt.Sprintf("KUBECONFIG=%s", kubeConfigPath))

	fmt.Printf("Running Kubectl Command: %v with args: %v", cmd.Path, cmd.Args)

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	// Run the kubectl command
	err := cmd.Run()
	if err != nil {
		errOutput := stderr.String()
		return "", fmt.Errorf("failed to run kubectl command: %v\nError details: %s", err, errOutput)
	}

	return stdout.String(), nil
}

// InstallHelmChart installs the given Helm chart (HTTP or OCI repo) into the
// namespace and returns the helm command output.
func InstallHelmChart(repoURL, myRepoName, myChartName, chartName, namespace, valuesURL, version string) (string, error) {
	// Check if this is an OCI-based chart
	if strings.HasPrefix(repoURL, "oci://") {
		return handleOCIBasedChartInstall(repoURL, myChartName, chartName, namespace, valuesURL, version)
	}

	// Handle traditional repository-based charts
	return handleTraditionalChartInstall(repoURL, myRepoName, myChartName, chartName, namespace, valuesURL, version)
}

// helmInstallArgs assembles the argument slice for a `helm upgrade --install`
// invocation. It is a pure helper so the argument shape can be unit-tested
// without shelling out to helm. The optional --values and --version flags are
// only appended when their corresponding inputs are non-empty.
func helmInstallArgs(releaseName, chartRef, namespace, valuesURL, version string) []string {
	args := []string{
		"upgrade",
		"--install",
		releaseName,
		chartRef,
		"--create-namespace",
		"--namespace", namespace,
	}

	// Add values URL if provided
	if valuesURL != "" {
		args = append(args, "--values", valuesURL)
	}

	// Add version flag if provided
	if version != "" {
		args = append(args, "--version", version)
	}

	return args
}

func handleOCIBasedChartInstall(ociURL, releaseName, chartName, namespace, valuesURL, version string) (string, error) {
	kubeConfigPath := "/etc/kubernetes/admin.conf"

	// Build the full OCI chart reference
	fullChartRef := fmt.Sprintf("%s/%s", ociURL, chartName)

	// Create upgrade/install command for OCI
	args := helmInstallArgs(releaseName, fullChartRef, namespace, valuesURL, version)

	// Execute helm upgrade --install command
	installCmd := exec.Command("helm", args...)
	installCmd.Env = append(os.Environ(), fmt.Sprintf("KUBECONFIG=%s", kubeConfigPath))

	var stdout, stderr bytes.Buffer
	installCmd.Stdout = &stdout
	installCmd.Stderr = &stderr

	fmt.Printf("Running Helm OCI Command: %v with args: %v\n", installCmd.Path, installCmd.Args)

	err := installCmd.Run()
	if err != nil {
		return stderr.String(), fmt.Errorf("failed to install OCI helm chart: %v", err)
	}

	fmt.Printf("Successfully installed OCI chart: %s\n", stdout.String())
	return stdout.String(), nil
}

func handleTraditionalChartInstall(repoURL, myRepoName, myChartName, chartName, namespace, valuesURL, version string) (string, error) {
	kubeConfigPath := "/etc/kubernetes/admin.conf"

	// Add the Helm repository
	addRepoCmd := exec.Command("helm", "repo", "add", myRepoName, repoURL, "--force-update")
	addRepoCmd.Env = append(os.Environ(), fmt.Sprintf("KUBECONFIG=%s", kubeConfigPath))

	var addRepoStdout, addRepoStderr bytes.Buffer
	addRepoCmd.Stdout = &addRepoStdout
	addRepoCmd.Stderr = &addRepoStderr

	fmt.Printf("Running Helm Command: %v with args: %v\n", addRepoCmd.Path, addRepoCmd.Args)

	err := addRepoCmd.Run()
	if err != nil {
		return addRepoStderr.String(), fmt.Errorf("failed to add helm repo: %v", err)
	}

	// Update Helm repositories
	updateRepoCmd := exec.Command("helm", "repo", "update")
	updateRepoCmd.Env = append(os.Environ(), fmt.Sprintf("KUBECONFIG=%s", kubeConfigPath))

	var updateRepoStdout, updateRepoStderr bytes.Buffer
	updateRepoCmd.Stdout = &updateRepoStdout
	updateRepoCmd.Stderr = &updateRepoStderr

	fmt.Printf("Running Helm Command: %v with args: %v\n", updateRepoCmd.Path, updateRepoCmd.Args)

	err = updateRepoCmd.Run()
	if err != nil {
		return updateRepoStderr.String(), fmt.Errorf("failed to update helm repos: %v", err)
	}

	// Create upgrade/install command
	args := helmInstallArgs(myChartName, fmt.Sprintf("%s/%s", myRepoName, chartName), namespace, valuesURL, version)

	// Execute helm upgrade --install command
	installCmd := exec.Command("helm", args...)
	installCmd.Env = append(os.Environ(), fmt.Sprintf("KUBECONFIG=%s", kubeConfigPath))

	var stdout, stderr bytes.Buffer
	installCmd.Stdout = &stdout
	installCmd.Stderr = &stderr

	fmt.Printf("Running Helm Command: %v with args: %v\n", installCmd.Path, installCmd.Args)

	err = installCmd.Run()
	if err != nil {
		return stderr.String(), fmt.Errorf("failed to install helm chart: %v", err)
	}

	fmt.Printf("Successfully installed chart: %s\n", stdout.String())
	return stdout.String(), nil
}

// UninstallHelmChart uninstalls the named Helm release from the namespace and
// returns the helm command output.
func UninstallHelmChart(myRepoName, myChartName, namespace string) (string, error) {
	kubeConfigPath := "/etc/kubernetes/admin.conf"

	// Create uninstall command
	args := []string{
		"uninstall",
		myChartName,
		"--namespace", namespace,
	}

	// Execute helm uninstall command
	uninstallCmd := exec.Command("helm", args...)
	uninstallCmd.Env = append(os.Environ(), fmt.Sprintf("KUBECONFIG=%s", kubeConfigPath))

	var stdout, stderr bytes.Buffer
	uninstallCmd.Stdout = &stdout
	uninstallCmd.Stderr = &stderr

	fmt.Printf("Running Helm Command: %v with args: %v\n", uninstallCmd.Path, uninstallCmd.Args)

	err := uninstallCmd.Run()
	if err != nil {
		return stderr.String(), fmt.Errorf("failed to uninstall helm chart: %v", err)
	}

	// Only remove repository for traditional repos (skip for OCI)
	if myRepoName != "" {
		removeRepoCmd := exec.Command("helm", "repo", "remove", myRepoName)
		removeRepoCmd.Env = append(os.Environ(), fmt.Sprintf("KUBECONFIG=%s", kubeConfigPath))

		var removeStdout, removeStderr bytes.Buffer
		removeRepoCmd.Stdout = &removeStdout
		removeRepoCmd.Stderr = &removeStderr

		fmt.Printf("Running Helm Command: %v with args: %v\n", removeRepoCmd.Path, removeRepoCmd.Args)

		err = removeRepoCmd.Run()
		if err != nil {
			return removeStderr.String(), fmt.Errorf("failed to remove helm repo: %v", err)
		}
	}

	fmt.Printf("Successfully uninstalled chart: %s\n", stdout.String())
	return stdout.String(), nil
}
