package install

import (
	"fmt"
	"net/url"
	"os/exec"
	"strings"

	"github.com/runos-official/nodeagent/backend"
	"github.com/runos-official/nodeagent/config"
	"github.com/runos-official/nodeagent/roslog"
)

// UpdateNodeAgent updates the installed node agent. When version is empty it
// updates to the version advertised by the installer (the existing behavior).
// When version is a non-empty exact tag (e.g. "v0.24.0") it pins to that
// version: the tag is exported as RUNOS_TARGET_VERSION so the invoked installer
// can resolve it.
func UpdateNodeAgent(version string) {
	// Get current version before update
	currentVersion := getInstalledVersion()
	roslog.I("Starting node agent update", "current_version", currentVersion, "target_version", version)

	fmt.Printf("\n╔═══════════════════════════════════════════════╗\n")
	fmt.Printf("║   RunOS Node Agent Update                     ║\n")
	fmt.Printf("╚═══════════════════════════════════════════════╝\n\n")
	fmt.Printf("Current Installation:\n")
	fmt.Printf("  Version: %s\n", currentVersion)
	fmt.Printf("  Binary:  /usr/local/bin/runos\n\n")
	if version != "" {
		fmt.Printf("  Target:  %s (pinned)\n\n", version)
	}
	fmt.Printf("→ Checking for updates...\n\n")

	// Execute the update command. The node agent ships as attested binaries on
	// GitHub Releases, so the installer needs an EXACT version (never a floating
	// "latest"). The version is passed via the installer's ?version= query
	// parameter, which the installer substitutes into the update script; the
	// script then downloads nodeagent-linux-<arch> for that exact tag and
	// verifies its sha256 before swapping the binary.
	//
	// When version is empty (a bare `runos update`), no pin is requested and the
	// installer is fail-closed: it will not fall back to a floating "latest".
	// The control-plane-driven update path supplies the advertised (or pinned)
	// version directly, and an operator can always pass `--version <tag>`.
	updateURL := buildUpdateURL(config.GetROSInstallerURL(), version)
	cmd := fmt.Sprintf("curl -sSL %q | sudo bash", updateURL)
	if err := executeCommand(cmd); err != nil {
		roslog.E("Node agent update command failed", err)
		fmt.Printf("\n✗ Update failed: %v\n\n", err)
		if err2 := backend.AddNodelog(1, "AgentUpdateFailure", fmt.Sprintf("Node agent update failed: %v", err)); err2 != nil {
			roslog.I("Could not add nodelog", err2)
		}
		return
	}

	// Get new version after update
	newVersion := getInstalledVersion()

	fmt.Printf("\n")
	fmt.Printf("═══════════════════════════════════════════════\n\n")

	if newVersion != "" && currentVersion != "" && newVersion != currentVersion {
		// Update was successful and version changed
		fmt.Printf("✓ Update Successful!\n\n")
		fmt.Printf("Version Change:\n")
		fmt.Printf("  Previous: %s\n", currentVersion)
		fmt.Printf("  Current:  %s\n\n", newVersion)

		roslog.I("Node agent updated successfully", "old_version", currentVersion, "new_version", newVersion)

		if err := backend.AddNodelog(3, "AgentUpdated", fmt.Sprintf("Node agent updated from %s to %s", currentVersion, newVersion)); err != nil {
			roslog.I("Could not add nodelog", err)
		}

		fmt.Printf("ℹ  The node agent service has been restarted with the new version.\n\n")
	} else if newVersion != "" && currentVersion != "" && newVersion == currentVersion {
		// No update available - already on latest version
		fmt.Printf("✓ No Update Available\n\n")
		fmt.Printf("Your node agent is already running the latest version:\n")
		fmt.Printf("  Version: %s\n\n", currentVersion)
		fmt.Printf("No action needed.\n\n")

		roslog.I("Node agent is already up to date", "version", currentVersion)
	} else if newVersion != "" {
		// Update completed - show new version
		fmt.Printf("✓ Update Completed\n\n")
		fmt.Printf("Previous Version: %s\n", currentVersion)
		fmt.Printf("New Version:      %s\n\n", newVersion)

		roslog.I("Node agent updated", "old_version", currentVersion, "new_version", newVersion)

		if err := backend.AddNodelog(3, "AgentUpdated", fmt.Sprintf("Node agent updated from %s to %s", currentVersion, newVersion)); err != nil {
			roslog.I("Could not add nodelog", err)
		}

		fmt.Printf("ℹ  The node agent service has been restarted with the new version.\n\n")
	} else {
		// Version detection failed
		fmt.Printf("✓ Update Process Completed\n\n")
		fmt.Printf("ℹ  Please run 'runos version' to verify the installed version.\n\n")

		roslog.I("Update completed but version detection failed")

		if err := backend.AddNodelog(3, "AgentUpdated", "The node agent has been successfully updated"); err != nil {
			roslog.I("Could not add nodelog", err)
		}
	}
}

// buildUpdateURL builds the installer update URL. When version is empty it
// returns installerURL+"/update" with no query: the installer is fail-closed
// and will not fall back to a floating "latest". When version is a non-empty
// tag it appends ?version=<tag> with a leading "v" stripped and the value
// query-escaped, so the installer downloads that exact, attested release.
func buildUpdateURL(installerURL, version string) string {
	if version == "" {
		return installerURL + "/update"
	}
	return fmt.Sprintf("%s/update?version=%s",
		installerURL, url.QueryEscape(strings.TrimPrefix(version, "v")))
}

// getInstalledVersion runs the runos binary to get its version
func getInstalledVersion() string {
	cmd := exec.Command("/usr/local/bin/runos", "version")
	output, err := cmd.Output()
	if err != nil {
		roslog.I("Could not detect installed version", err)
		return ""
	}

	// The version command outputs just the version number (e.g., "0.21.22")
	return strings.TrimSpace(string(output))
}
