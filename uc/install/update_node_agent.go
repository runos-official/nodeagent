package install

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"

	"github.com/runos-official/nodeagent/backend"
	"github.com/runos-official/nodeagent/config"
	"github.com/runos-official/nodeagent/roslog"
)

// semverRe matches an exact, non-floating release tag: an optional leading "v"
// followed by MAJOR.MINOR.PATCH, with an optional pre-release/build suffix
// (e.g. "v0.24.0", "0.24.0", "1.2.3-rc.1"). The updater is fail-closed against
// floating versions, so values like "latest" or "banana" are rejected here
// before we ever build a release URL.
var semverRe = regexp.MustCompile(`^v?\d+\.\d+\.\d+([-+][0-9A-Za-z.\-+]+)?$`)

// releaseBaseURL is the GitHub Releases download base for the node agent. The
// agent ships as attested binaries here alongside a per-release checksums.txt,
// so the verified-update flow fetches both directly from this host (never a
// remote shell script). Declared as a var so tests can point it at a fixture
// server.
var releaseBaseURL = "https://github.com/runos-official/nodeagent/releases/download"

// binaryPath is the installed node-agent binary that gets atomically replaced.
// Declared as a var so tests can target a temp file.
var binaryPath = "/usr/local/bin/runos"

// downloadTimeout bounds the binary/checksum fetches so a hung installer host
// cannot wedge the update forever.
const downloadTimeout = 5 * time.Minute

// advertisedVersionTimeout bounds the conductor query for the advertised version
// so a hung control plane cannot wedge a bare `runos update`.
const advertisedVersionTimeout = 15 * time.Second

// conductorBaseURL returns the conductor base URL for the advertised-version
// query. Declared as a var so tests can point it at a fixture server.
var conductorBaseURL = config.GetConductorURL

// advertisedAID returns this node's account id. Declared as a var so tests can
// override it without touching global viper state.
var advertisedAID = config.GetAID

// resolveAdvertisedVersion asks the conductor for the EXACT node-agent version
// advertised to this node's account:
//
//	GET {conductorURL}/{aid}/node-agent-version  ->  {"version":"<v>"}
//
// This is what a bare `runos update` (no --version) resolves to. Crucially it is
// NOT a floating "latest": the control plane returns a specific, pinned tag, which
// the updater then verifies (sha256 vs the release checksums) and installs exactly
// like an explicit --version. It fails closed (returns an error, never a guess) on
// a missing conductor URL / account id, a transport or non-2xx HTTP error, or an
// empty/unparseable body, so the updater can never fall back to an unpinned fetch.
func resolveAdvertisedVersion() (string, error) {
	base := strings.TrimRight(conductorBaseURL(), "/")
	aid := advertisedAID()
	if base == "" || aid == "" {
		return "", fmt.Errorf("no conductor URL or account id in config")
	}
	u, err := url.Parse(base)
	if err != nil {
		return "", fmt.Errorf("invalid conductor URL %q: %w", base, err)
	}
	u = u.JoinPath(url.PathEscape(aid), "node-agent-version")

	client := &http.Client{Timeout: advertisedVersionTimeout}
	body, err := fetch(client, u.String())
	if err != nil {
		return "", fmt.Errorf("query %s: %w", u.String(), err)
	}
	var payload struct {
		Version string `json:"version"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return "", fmt.Errorf("parse advertised-version response: %w", err)
	}
	v := strings.TrimSpace(payload.Version)
	if v == "" {
		return "", fmt.Errorf("conductor advertised an empty version for account %s", aid)
	}
	return v, nil
}

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

// UpdateNodeAgent updates the installed node agent to an exact, attested
// release. The flow is performed entirely in Go (no remote shell is fetched or
// piped to root):
//
//  1. resolve the exact target version (the pinned/advertised tag; never a
//     floating "latest"),
//  2. download nodeagent-linux-<arch> from the GitHub Release for that tag,
//  3. fetch that release's checksums.txt and verify the binary's sha256 matches
//     (abort on mismatch),
//  4. only then atomically replace /usr/local/bin/runos (install 0755 to a temp
//     path next to the target, then rename) and restart runos.service.
//
// This removes the prior `curl … | sudo bash` of a remote updater, which trusted
// whatever script the installer host served (a root-RCE vector if that host was
// compromised). The binary is now verified by this process before it is ever
// placed on disk, and the script that does the verifying is this code, not a
// remote download.
//
// When version is empty (a bare `runos update`) the target is resolved from the
// control plane: conductor's GET /{aid}/node-agent-version returns the EXACT
// advertised tag, which is then validated and verified like an explicit pin. This
// is never a floating "latest" -- if the advertised version cannot be resolved the
// updater fails closed rather than fetching an unpinned binary. A non-empty version
// must be an exact semver tag; floating values ("latest") are rejected.
//
// asJSON suppresses the human-readable banner/result and instead prints a single
// UpdateResult JSON object to stdout. On failure UpdateNodeAgent returns an
// already-reported error (via roslog.Fail) so the command exits non-zero with a
// single canonical failure block; in --json mode it returns a plain error after
// emitting the failure result object.
func UpdateNodeAgent(version string, asJSON bool) error {
	// Resolve and validate the exact target version up front. The agent ships as
	// attested binaries, so an update must name an exact semver tag; floating
	// values ("latest") are rejected and an empty version is fail-closed.
	version = strings.TrimSpace(version)
	if version == "" {
		// No explicit pin: resolve the EXACT version advertised by the control
		// plane (conductor) for this account and pin to it. This is not a floating
		// "latest" -- conductor returns a specific tag, which is then validated and
		// sha256-verified against the release checksums like any --version. Still
		// fail-closed: if resolution fails we do NOT fall back to an unpinned fetch.
		resolved, err := resolveAdvertisedVersion()
		if err != nil {
			cause := fmt.Sprintf("could not resolve the advertised version: %v", err)
			remedy := "check network access to the conductor API, or pass an exact release tag, e.g. runos update --version v0.24.0"
			return fail(asJSON, "", "", cause, remedy)
		}
		version = resolved
		if !asJSON {
			fmt.Printf("→ Resolved advertised version: %s\n", version)
		}
	}
	if !semverRe.MatchString(version) {
		cause := fmt.Sprintf("invalid --version %q: not an exact semantic version", version)
		remedy := "pass an exact release tag, e.g. --version v0.24.0 (floating values like 'latest' are not allowed)"
		return fail(asJSON, version, "", cause, remedy)
	}
	// Normalize to a leading-"v" tag for the release URL; strip any "v" the
	// caller passed, then re-add exactly one.
	tag := "v" + strings.TrimPrefix(version, "v")

	// Get current version before update.
	currentVersion := getInstalledVersion()
	roslog.I("Starting node agent update", "current_version", currentVersion, "target_version", tag)

	if !asJSON {
		fmt.Printf("\n╔═══════════════════════════════════════════════╗\n")
		fmt.Printf("║   RunOS Node Agent Update                     ║\n")
		fmt.Printf("╚═══════════════════════════════════════════════╝\n\n")
		fmt.Printf("Current Installation:\n")
		fmt.Printf("  Version: %s\n", displayVersion(currentVersion))
		fmt.Printf("  Binary:  %s\n\n", binaryPath)
		fmt.Printf("  Target:  %s (verified download)\n\n", tag)
		fmt.Printf("→ Downloading and verifying the release binary...\n\n")
	}

	// Perform the verified swap: download + sha256-verify + atomic replace.
	if err := installVerifiedBinary(tag); err != nil {
		roslog.E("Node agent update failed", err)
		if err2 := backend.AddNodelog(1, "AgentUpdateFailure", fmt.Sprintf("Node agent update failed: %v", err)); err2 != nil {
			roslog.I("Could not add nodelog", "error", err2)
		}
		cause := err.Error()
		remedy := "check network access to github.com (the release download host) and that this node can write " + binaryPath + "; re-run runos update --version " + tag
		return fail(asJSON, tag, currentVersion, cause, remedy)
	}

	// Restart the service so the new binary takes over.
	if err := restartService(); err != nil {
		roslog.E("Node agent service restart failed", err)
		if err2 := backend.AddNodelog(1, "AgentUpdateFailure", fmt.Sprintf("Node agent restart failed after update: %v", err)); err2 != nil {
			roslog.I("Could not add nodelog", "error", err2)
		}
		cause := fmt.Sprintf("restart runos.service after install: %v", err)
		remedy := "the new binary is installed; restart it manually with `sudo systemctl restart runos.service` and check `runos logs`"
		return fail(asJSON, tag, currentVersion, cause, remedy)
	}

	// Get new version after update.
	newVersion := getInstalledVersion()
	result := classifyResult(currentVersion, newVersion, tag)

	if asJSON {
		emitJSON(result)
		return nil
	}

	printResult(result)
	return nil
}

// fail reports an update failure consistently for both output modes and returns
// an already-reported error so the command exits non-zero with a single
// canonical block (human mode) or after emitting the JSON result (--json mode).
func fail(asJSON bool, target, previous, cause, remedy string) error {
	if asJSON {
		emitJSON(UpdateResult{
			Status:          "failed",
			PreviousVersion: previous,
			TargetVersion:   target,
			Error:           cause,
		})
		return roslog.AlreadyReported(fmt.Errorf("%s", cause))
	}
	return roslog.Fail("Update node agent", cause, remedy)
}

// installVerifiedBinary downloads nodeagent-linux-<arch> for the given exact tag
// from the GitHub Release, verifies its sha256 against that release's published
// checksums.txt, and only on a match installs it atomically over binaryPath.
// Any download, checksum, or filesystem error returns a non-nil error and leaves
// the existing binary untouched.
func installVerifiedBinary(tag string) error {
	arch := goarchToReleaseArch(runtime.GOARCH)
	if arch == "" {
		return fmt.Errorf("unsupported architecture %q: only linux amd64 and arm64 are supported", runtime.GOARCH)
	}
	binaryName := "nodeagent-linux-" + arch
	base := strings.TrimRight(releaseBaseURL, "/") + "/" + tag
	binaryURL := base + "/" + binaryName
	checksumsURL := base + "/checksums.txt"

	client := &http.Client{Timeout: downloadTimeout}

	// Download the binary into memory; we hash it before it ever touches the
	// real binary path, so a tampered or truncated download cannot be installed.
	binBytes, err := fetch(client, binaryURL)
	if err != nil {
		return fmt.Errorf("download %s: %w", binaryURL, err)
	}

	// Fetch the release's checksums.txt and find the expected sha256 for this
	// asset. Fail closed if the asset is not listed.
	checksumsBytes, err := fetch(client, checksumsURL)
	if err != nil {
		return fmt.Errorf("download %s: %w", checksumsURL, err)
	}
	expected, err := expectedChecksum(string(checksumsBytes), binaryName)
	if err != nil {
		return err
	}

	sum := sha256.Sum256(binBytes)
	actual := hex.EncodeToString(sum[:])
	if !strings.EqualFold(actual, expected) {
		return fmt.Errorf("checksum mismatch for %s (expected %s, got %s); refusing to install", binaryName, expected, actual)
	}
	roslog.I("Verified release binary sha256", "asset", binaryName, "sha256", actual)

	return atomicInstall(binaryPath, binBytes)
}

// fetch GETs url and returns its body, failing closed on any non-2xx status or
// transport error (so an error page, 404, or captive portal can never be
// mistaken for a binary or a checksum line).
func fetch(client *http.Client, url string) ([]byte, error) {
	resp, err := client.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		// Drain a small amount so the message is useful but bounded.
		io.CopyN(io.Discard, resp.Body, 4096)
		return nil, fmt.Errorf("unexpected HTTP status %s", resp.Status)
	}
	return io.ReadAll(resp.Body)
}

// expectedChecksum parses a `sha256sum`-style checksums.txt and returns the hex
// digest for binaryName. Lines look like "<hex>␠␠<name>" or "<hex>␠*<name>"
// (binary-mode marker). Returns an error if the asset is not present, so a
// missing entry fails the update closed.
func expectedChecksum(checksums, binaryName string) (string, error) {
	for _, line := range strings.Split(checksums, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		name := strings.TrimPrefix(fields[1], "*")
		if name == binaryName {
			return strings.ToLower(fields[0]), nil
		}
	}
	return "", fmt.Errorf("no published checksum for %s in checksums.txt; refusing to install", binaryName)
}

// atomicInstall writes data to a temp file in the same directory as dst with
// mode 0755, fsyncs it, then renames it over dst. The rename is atomic on the
// same filesystem, so a reader either sees the old binary or the fully written
// new one, never a partial file.
func atomicInstall(dst string, data []byte) error {
	dir := filepath.Dir(dst)
	tmp, err := os.CreateTemp(dir, ".runos-update-*")
	if err != nil {
		return fmt.Errorf("create temp file in %s: %w", dir, err)
	}
	tmpName := tmp.Name()
	// Best-effort cleanup if we bail before the rename.
	defer func() {
		if _, statErr := os.Stat(tmpName); statErr == nil {
			_ = os.Remove(tmpName)
		}
	}()

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("write temp binary: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("sync temp binary: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp binary: %w", err)
	}
	if err := os.Chmod(tmpName, 0o755); err != nil {
		return fmt.Errorf("chmod temp binary: %w", err)
	}
	if err := os.Rename(tmpName, dst); err != nil {
		return fmt.Errorf("install binary to %s: %w", dst, err)
	}
	roslog.I("Installed verified node agent binary", "path", dst)
	return nil
}

// restartService restarts runos.service so the freshly installed binary takes
// over. Declared via a var so tests can stub it.
var restartService = func() error {
	cmd := exec.Command("systemctl", "restart", "runos.service")
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("%v: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// goarchToReleaseArch maps a Go GOARCH to the release asset arch suffix. Returns
// "" for unsupported architectures so the caller fails closed.
func goarchToReleaseArch(goarch string) string {
	switch goarch {
	case "amd64":
		return "amd64"
	case "arm64":
		return "arm64"
	default:
		return ""
	}
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

// getInstalledVersion runs the runos binary to get its version.
func getInstalledVersion() string {
	cmd := exec.Command(binaryPath, "version")
	output, err := cmd.Output()
	if err != nil {
		roslog.I("Could not detect installed version", "error", err)
		return ""
	}

	// The version command outputs just the version number (e.g., "0.21.22").
	return strings.TrimSpace(string(output))
}
