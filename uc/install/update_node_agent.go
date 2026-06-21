package install

import (
	"encoding/json"
	"fmt"
	"net/url"
	"os/exec"
	"regexp"
	"strings"

	"github.com/runos-official/nodeagent/backend"
	"github.com/runos-official/nodeagent/config"
	"github.com/runos-official/nodeagent/roslog"
)

// semverRe matches an exact, non-floating release tag: an optional leading "v"
// followed by MAJOR.MINOR.PATCH, with an optional pre-release/build suffix
// (e.g. "v0.24.0", "0.24.0", "1.2.3-rc.1"). The installer is fail-closed
// against floating versions, so values like "latest" or "banana" are rejected
// here before we ever build the update URL.
var semverRe = regexp.MustCompile(`^v?\d+\.\d+\.\d+([-+][0-9A-Za-z.\-+]+)?$`)

// UpdateResult is the stable, machine-readable outcome of an update run. It is
// emitted as a single JSON object when the caller requests --json.
type UpdateResult struct {
	// Status is one of: "updated", "up-to-date", "completed", "failed".
	Status          string `json:"status"`
	PreviousVersion string `json:"previousVersion"`
	NewVersion      string `json:"newVersion"`
	TargetVersion   string `json:"targetVersion,omitempty"`
	// Changed is true when the installed version differs after the update.
	Changed bool `json:"changed"`
	// Error carries the failure cause when Status is "failed".
	Error string `json:"error,omitempty"`
}

// UpdateNodeAgent updates the installed node agent. When version is empty it
// updates to the version advertised by the installer (the existing behavior).
// When version is a non-empty exact tag (e.g. "v0.24.0") it pins to that
// version: the tag is passed to the installer via the ?version= query so the
// invoked installer downloads that exact, attested release.
//
// asJSON suppresses the human-readable banner/result and instead prints a single
// UpdateResult JSON object to stdout. On failure UpdateNodeAgent returns an
// already-reported error (via roslog.Fail) so the command exits non-zero with a
// single canonical failure block; in --json mode it returns a plain error after
// emitting the failure result object.
func UpdateNodeAgent(version string, asJSON bool) error {
	// Validate --version up front. The installer is fail-closed against floating
	// versions, so an explicit pin must be an exact semver tag.
	version = strings.TrimSpace(version)
	if version != "" && !semverRe.MatchString(version) {
		cause := fmt.Sprintf("invalid --version %q: not an exact semantic version", version)
		remedy := "pass an exact release tag, e.g. --version v0.24.0 (floating values like 'latest' are not allowed)"
		if asJSON {
			emitJSON(UpdateResult{Status: "failed", TargetVersion: version, Error: cause})
			return roslog.AlreadyReported(fmt.Errorf("%s", cause))
		}
		return roslog.Fail("Update node agent", cause, remedy)
	}

	// Guard an unconfigured installer URL before building "curl ... | sudo bash".
	installerURL := config.GetROSInstallerURL()
	if installerURL == "" {
		cause := "installer URL is not configured (client.server.installer is empty)"
		remedy := "run `runos register` first, or set client.server.installer in /etc/runos/config.yaml"
		if asJSON {
			emitJSON(UpdateResult{Status: "failed", TargetVersion: version, Error: cause})
			return roslog.AlreadyReported(fmt.Errorf("%s", cause))
		}
		return roslog.Fail("Update node agent", cause, remedy)
	}

	// Get current version before update.
	currentVersion := getInstalledVersion()
	roslog.I("Starting node agent update", "current_version", currentVersion, "target_version", version)

	if !asJSON {
		fmt.Printf("\n╔═══════════════════════════════════════════════╗\n")
		fmt.Printf("║   RunOS Node Agent Update                     ║\n")
		fmt.Printf("╚═══════════════════════════════════════════════╝\n\n")
		fmt.Printf("Current Installation:\n")
		fmt.Printf("  Version: %s\n", displayVersion(currentVersion))
		fmt.Printf("  Binary:  /usr/local/bin/runos\n\n")
		if version != "" {
			fmt.Printf("  Target:  %s (pinned)\n\n", version)
		}
		fmt.Printf("→ Checking for updates...\n\n")
	}

	// Execute the update command. The node agent ships as attested binaries on
	// GitHub Releases, so the installer needs an EXACT version (never a floating
	// "latest"). The version is passed via the installer's ?version= query
	// parameter, which the installer substitutes into the update script; the
	// script then downloads nodeagent-linux-<arch> for that exact tag and
	// verifies its sha256 before swapping the binary.
	//
	// When version is empty (a bare `runos update`), no pin is requested and the
	// installer is fail-closed: it will not fall back to a floating "latest".
	updateURL := buildUpdateURL(installerURL, version)
	cmd := fmt.Sprintf("curl -sSL %q | sudo bash", updateURL)
	if err := executeCommand(cmd); err != nil {
		roslog.E("Node agent update command failed", err)
		if err2 := backend.AddNodelog(1, "AgentUpdateFailure", fmt.Sprintf("Node agent update failed: %v", err)); err2 != nil {
			roslog.I("Could not add nodelog", "error", err2)
		}
		cause := fmt.Sprintf("installer update failed: %v", err)
		remedy := "check network access to the installer host and that this node can run curl | sudo bash; re-run runos update --version <tag>"
		if asJSON {
			emitJSON(UpdateResult{
				Status:          "failed",
				PreviousVersion: currentVersion,
				TargetVersion:   version,
				Error:           cause,
			})
			return roslog.AlreadyReported(fmt.Errorf("%s", cause))
		}
		return roslog.Fail("Update node agent", cause, remedy)
	}

	// Get new version after update.
	newVersion := getInstalledVersion()
	result := classifyResult(currentVersion, newVersion, version)

	if asJSON {
		emitJSON(result)
		return nil
	}

	printResult(result)
	return nil
}

// classifyResult derives the UpdateResult from the before/after versions. Empty
// before/after versions are treated as "unknown" (not a clean success), so a
// failure to detect the version is never reported as a green check on a known
// version change.
func classifyResult(currentVersion, newVersion, target string) UpdateResult {
	r := UpdateResult{
		PreviousVersion: currentVersion,
		NewVersion:      newVersion,
		TargetVersion:   target,
	}
	switch {
	case currentVersion != "" && newVersion != "" && newVersion != currentVersion:
		r.Status = "updated"
		r.Changed = true
	case currentVersion != "" && newVersion != "" && newVersion == currentVersion:
		r.Status = "up-to-date"
	default:
		// At least one version is unknown: the update ran, but we cannot assert a
		// known-to-known transition.
		r.Status = "completed"
	}
	return r
}

// printResult renders the human-readable summary for a finished update.
func printResult(r UpdateResult) {
	fmt.Printf("\n")
	fmt.Printf("═══════════════════════════════════════════════\n\n")

	switch r.Status {
	case "updated":
		fmt.Printf("✓ Update Successful!\n\n")
		fmt.Printf("Version Change:\n")
		fmt.Printf("  Previous: %s\n", displayVersion(r.PreviousVersion))
		fmt.Printf("  Current:  %s\n\n", displayVersion(r.NewVersion))

		roslog.I("Node agent updated successfully", "old_version", r.PreviousVersion, "new_version", r.NewVersion)

		if err := backend.AddNodelog(3, "AgentUpdated", fmt.Sprintf("Node agent updated from %s to %s", r.PreviousVersion, r.NewVersion)); err != nil {
			roslog.I("Could not add nodelog", "error", err)
		}

		fmt.Printf("ℹ  The node agent service has been restarted with the new version.\n\n")

	case "up-to-date":
		fmt.Printf("✓ No Update Available\n\n")
		fmt.Printf("Your node agent is already running the latest version:\n")
		fmt.Printf("  Version: %s\n\n", displayVersion(r.PreviousVersion))
		fmt.Printf("No action needed.\n\n")

		roslog.I("Node agent is already up to date", "version", r.PreviousVersion)

	default: // "completed": the update ran but we could not confirm a known transition.
		fmt.Printf("✓ Update Process Completed\n\n")
		fmt.Printf("Previous Version: %s\n", displayVersion(r.PreviousVersion))
		fmt.Printf("New Version:      %s\n\n", displayVersion(r.NewVersion))
		fmt.Printf("ℹ  Run 'runos version' to verify the installed version.\n\n")

		roslog.I("Node agent update completed", "old_version", r.PreviousVersion, "new_version", r.NewVersion)

		if err := backend.AddNodelog(3, "AgentUpdated", "The node agent has been updated"); err != nil {
			roslog.I("Could not add nodelog", "error", err)
		}
	}
}

// emitJSON prints a single UpdateResult JSON object to stdout.
func emitJSON(r UpdateResult) {
	b, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		// Should never happen for this flat struct; fall back to a minimal object.
		roslog.E("Could not marshal update result", err)
		fmt.Printf("{\"status\":%q,\"error\":\"could not marshal result\"}\n", r.Status)
		return
	}
	fmt.Println(string(b))
}

// displayVersion renders an empty (undetected) version as "unknown" for
// human-readable output, so a missing version is never shown as a blank that
// reads like a clean success.
func displayVersion(v string) string {
	if v == "" {
		return "unknown"
	}
	return v
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

// getInstalledVersion runs the runos binary to get its version.
func getInstalledVersion() string {
	cmd := exec.Command("/usr/local/bin/runos", "version")
	output, err := cmd.Output()
	if err != nil {
		roslog.I("Could not detect installed version", "error", err)
		return ""
	}

	// The version command outputs just the version number (e.g., "0.21.22").
	return strings.TrimSpace(string(output))
}
