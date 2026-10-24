package agentstream

import (
	"fmt"
	"regexp"
	"strings"
)

// The UPGRADE_NODE_K8S handler runs commands as root with the request's
// version fields in them. Each field must have the exact shape that nodeward
// sends, or the handler refuses the request before it runs any command
// (FCR 1648).
//
// Nodeward builds the request from its version matrix
// (config/node_dependency_versions.go): Kubernetes, containerd and helm are
// plain X.Y.Z, and the apt repo version is "v" plus the Kubernetes major.minor
// (SoftwareVersions.KubeRepoVersion).
var (
	upgradePackageVersionRe = regexp.MustCompile(`^[0-9]{1,4}\.[0-9]{1,4}\.[0-9]{1,4}$`)
	upgradeRepoVersionRe    = regexp.MustCompile(`^v[0-9]{1,4}\.[0-9]{1,4}$`)
)

// validateUpgradeNodeK8sRequest returns an error that names the first field
// with the wrong shape. The error never echoes the value.
func validateUpgradeNodeK8sRequest(req upgradeNodeK8sRequest) error {
	for _, f := range []struct{ name, value string }{
		{"targetKubernetesVersion", req.TargetKubernetesVersion},
		{"targetContainerdVersion", req.TargetContainerdVersion},
		{"targetHelmVersion", req.TargetHelmVersion},
	} {
		if !upgradePackageVersionRe.MatchString(f.value) {
			return fmt.Errorf("%s is not a version of the form X.Y.Z", f.name)
		}
	}
	if !upgradeRepoVersionRe.MatchString(req.KubeRepoVersion) {
		return fmt.Errorf("kubeRepoVersion is not of the form vX.Y")
	}
	parts := strings.SplitN(req.TargetKubernetesVersion, ".", 3)
	if want := "v" + parts[0] + "." + parts[1]; req.KubeRepoVersion != want {
		return fmt.Errorf("kubeRepoVersion does not match the minor of targetKubernetesVersion")
	}
	return nil
}

// shellQuote returns s as one single-quoted shell word. The handler validates
// every value first; the quoting is defence in depth for the few commands that
// still go through a shell.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
